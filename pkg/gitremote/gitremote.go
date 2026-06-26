// Package gitremote implements git's remote-helper line protocol for doSource —
// both directions: import (clone/fetch: projected op-log → git fast-import stream)
// and export (push: git fast-export stream → typed ops in the op-log). It is how
// `git clone dosource://…`, `git fetch dosource`, and `git push dosource://…` Just
// Work without forking git: git speaks its documented stdin/stdout helper protocol
// to the git-remote-dosource binary, and this package answers.
//
// # Why this exists
//
// The locked strategy is "replace GitHub the product, never fork git." A git
// remote-helper is exactly the seam git designed for third parties to teach it a
// new transport: ship a binary named git-remote-<scheme> and git invokes it for
// every <scheme>://… remote. The user/agent keeps typing pure git; doSource is
// the transport underneath. No pack-format change, no broken `git push origin`.
//
// # The private tracking namespace (load-bearing)
//
// The refspec maps remote refs into a PRIVATE tracking namespace
// (refs/heads/* → refs/dosource/heads/*), NOT directly onto refs/heads/*. Aliasing
// onto local branches makes git believe the remote already holds the local tip and
// export an empty range on push. Consequently the protocol is ASYMMETRIC by ref
// name: `list` advertises the REMOTE name (refs/heads/main, what git asked for),
// but the import stream writes the TRACKING name (refs/dosource/heads/main) so
// git's fetch finds the fetched ref where the refspec says it should be. git itself
// tracks the remote's state via that tracking ref: first push (no tracking ref) →
// full export; after a successful push git advances it → subsequent pushes export
// only the new commits.
//
// # Scope
//
// Both directions collapse history to the tip tree (symmetric single-commit model):
// import emits one tip commit; export records the pushed tip tree as ops. Per-op git
// history granularity is a later increment. The op-log Source/Sink is local; the
// cloud transport plugs in behind the same interfaces — the flagged cloud checkpoint.
//
// # Boundary
//
// OPEN component: pure Go standard library — no cloud, no do-core, no AWS SDK,
// no closed-source dependencies. Protocol read/write + stream codec are pure functions
// over io.Reader/io.Writer + Source/pushSink, unit-tested against fakes AND real
// `git fast-import`/`fast-export`/clone/push round-trips.
package gitremote

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// CommitInfo is the metadata stamped on the single tip commit an import produces.
type CommitInfo struct {
	// Committer is the identity line WITHOUT the trailing timestamp, e.g.
	// "gitevolved <noreply@gitevolved.ai>".
	Committer string
	// WhenUnix is the commit time as a raw unix timestamp (seconds).
	WhenUnix int64
	// TZ is the timezone suffix in git's "+0000" form.
	TZ string
	// Subject is the commit-message first line.
	Subject string
}

// Source is the read side of the backend: it projects the op-log. The local
// implementation reads a local op-log; a cloud implementation (later) satisfies
// the same contract so the protocol layer never changes.
type Source interface {
	// Tree returns the projected working tree: repo-relative path → content.
	Tree() (map[string][]byte, error)
	// Head returns commit metadata for the tip and whether the log has any
	// content at all. ok=false means an empty repo (clone yields no refs).
	Head() (CommitInfo, bool, error)
}

// pushSink is the write side: a Source that ALSO accepts recorded edits is
// push-capable, so Serve advertises + handles export. localrepo.Repo satisfies it;
// an import-only Source does not, and push stays disabled for it — the public Serve
// API is unchanged either way.
type pushSink interface {
	RecordFile(path string, content []byte) (bool, error)
	RecordDelete(path string) (bool, error)
}

// flusher is an OPTIONAL extension of a pushSink whose recorded edits are batched
// and committed as one unit (e.g. a cloud backend: stage all files, then one
// attach→add→push). If a sink implements it, handleExport calls Flush after the
// record loop, before reporting success. A sink that persists each RecordFile
// immediately (the local op-log) does not implement it — the export is a no-op flush.
type flusher interface {
	Flush() error
}

const (
	// remoteRef is the ref name git asks for / we advertise (the remote-side name).
	remoteRef = "refs/heads/main"
	// trackingRef is the refspec RHS — the private tracking namespace the import
	// stream writes to and git uses to track remote state for push range computation.
	trackingRef = "refs/dosource/heads/main"
	// refspecLine maps remote refs into the private tracking namespace.
	refspecLine = "refspec refs/heads/*:refs/dosource/heads/*"
)

// Serve runs the remote-helper command loop, reading newline-delimited commands
// from in and writing responses to out, until EOF. It returns the first
// protocol/IO error.
//
// Commands handled:
//
//	capabilities          → import + refspec (+ export iff src is push-capable)
//	list                  → advertise refs/heads/main (value "?", import-provided)
//	list for-push         → empty (push-capable: forces git to a full export)
//	import <ref>          → (batched until blank line) emit one fast-import stream
//	export                → consume git's fast-export stream → record ops → ok
//	option …              → unsupported (we honor none yet; git proceeds w/ defaults)
//
// Unknown commands are ignored (forward-compatible with optional capabilities).
func Serve(in io.Reader, out io.Writer, src Source) error {
	r := bufio.NewReader(in)
	w := bufio.NewWriter(out)
	defer w.Flush()

	sink, canPush := src.(pushSink)

	var importBatch []string
	for {
		line, err := r.ReadString('\n')
		line = strings.TrimRight(line, "\n")

		// A blank line terminates an import batch (git's framing).
		if line == "" {
			if len(importBatch) > 0 {
				if werr := writeImportStream(w, src); werr != nil {
					return werr
				}
				if ferr := w.Flush(); ferr != nil {
					return ferr
				}
				importBatch = importBatch[:0]
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			continue
		}

		switch {
		case line == "capabilities":
			fmt.Fprint(w, "import\n")
			if canPush {
				fmt.Fprint(w, "export\n")
			}
			fmt.Fprintf(w, "%s\n\n", refspecLine)
			if ferr := w.Flush(); ferr != nil {
				return ferr
			}
		case line == "list for-push":
			// Empty remote → git sends a full fast-export the first time; after a
			// successful push git advances the tracking ref and exports incrementally.
			fmt.Fprint(w, "\n")
			if ferr := w.Flush(); ferr != nil {
				return ferr
			}
		case line == "list" || strings.HasPrefix(line, "list "):
			if lerr := writeList(w, src); lerr != nil {
				return lerr
			}
			if ferr := w.Flush(); ferr != nil {
				return ferr
			}
		case strings.HasPrefix(line, "import "):
			importBatch = append(importBatch, strings.TrimPrefix(line, "import "))
		case strings.HasPrefix(line, "option "):
			fmt.Fprint(w, "unsupported\n")
			if ferr := w.Flush(); ferr != nil {
				return ferr
			}
		case line == "export":
			if !canPush {
				return fmt.Errorf("gitremote: export requested but backend is not push-capable")
			}
			if herr := handleExport(r, w, src, sink); herr != nil {
				return herr
			}
			if ferr := w.Flush(); ferr != nil {
				return ferr
			}
		default:
			// Unknown/optional command — ignore.
		}

		if err == io.EOF {
			if len(importBatch) > 0 {
				if werr := writeImportStream(w, src); werr != nil {
					return werr
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// writeList answers `list`: advertise the remote ref refs/heads/main as
// import-provided ("?") plus the HEAD symref, then a blank terminator. An empty
// op-log advertises NO refs (a clone of an empty repo).
func writeList(w io.Writer, src Source) error {
	_, ok, err := src.Head()
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprint(w, "\n")
		return nil
	}
	fmt.Fprintf(w, "? %s\n@%s HEAD\n\n", remoteRef, remoteRef)
	return nil
}

// writeImportStream emits the git fast-import stream that reconstructs the
// projected tree as a single tip commit. It writes the commit to the TRACKING ref
// (refspec RHS) so git's fetch finds the fetched ref where the refspec maps it.
// Blobs are emitted first (each with a mark) so the commit can reference them.
func writeImportStream(w io.Writer, src Source) error {
	tree, err := src.Tree()
	if err != nil {
		return err
	}
	info, ok, err := src.Head()
	if err != nil {
		return err
	}
	if !ok || len(tree) == 0 {
		fmt.Fprint(w, "done\n")
		return nil
	}

	paths := make([]string, 0, len(tree))
	for p := range tree {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	marks := make(map[string]int, len(paths))
	for i, p := range paths {
		mark := i + 1
		marks[p] = mark
		content := tree[p]
		fmt.Fprintf(w, "blob\nmark :%d\ndata %d\n", mark, len(content))
		if _, werr := w.Write(content); werr != nil {
			return werr
		}
		fmt.Fprint(w, "\n")
	}

	commitMark := len(paths) + 1
	msg := info.Subject
	if msg == "" {
		msg = "doSource import"
	}
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprintf(w, "commit %s\nmark :%d\ncommitter %s %d %s\ndata %d\n%s",
		trackingRef, commitMark, info.Committer, info.WhenUnix, info.TZ, len(msg), msg)
	for _, p := range paths {
		fmt.Fprintf(w, "M 100644 :%d %s\n", marks[p], p)
	}
	fmt.Fprint(w, "done\n")
	return nil
}

// handleExport consumes git's fast-export stream from r (a `git push`), records
// the resulting tip tree into the backend as typed ops, and reports the per-ref
// result. The pushed history is collapsed to its tip tree: every file in the tip
// is recorded (unchanged files no-op), every tracked path absent from the tip is
// deleted. A push carrying no commit is a no-op (never delete-all).
func handleExport(r *bufio.Reader, w io.Writer, src Source, sink pushSink) error {
	tree, sawCommit, pushedRef, err := parseFastExport(r)
	if err != nil {
		return err
	}
	if !sawCommit {
		fmt.Fprintf(w, "ok %s\n\n", remoteRef)
		return nil
	}
	// Single-mainline guard. doSource is single-CAS-fenced-mainline by design (the
	// D4 moat); multi-branch collaboration is a deliberate future feature. Reject a
	// push to any ref other than refs/heads/main BEFORE recording anything, so a
	// `git push dosource://… some-branch` fails loud instead of silently landing its
	// content on the mainline (a wrong-target data write). Report the error against
	// the ref git actually asked for so git surfaces it on the right line.
	if pushedRef != remoteRef {
		fmt.Fprintf(w, "error %s doSource currently supports only the %s branch (single-mainline by design); multi-branch push is not yet available\n\n", pushedRef, remoteRef)
		return nil
	}

	cur, err := src.Tree()
	if err != nil {
		return err
	}
	for path, content := range tree {
		if _, e := sink.RecordFile(path, content); e != nil {
			fmt.Fprintf(w, "error %s %v\n\n", remoteRef, e)
			return e
		}
	}
	for path := range cur {
		if _, ok := tree[path]; !ok {
			if _, e := sink.RecordDelete(path); e != nil {
				fmt.Fprintf(w, "error %s %v\n\n", remoteRef, e)
				return e
			}
		}
	}
	// A batched backend (cloud) commits here; an immediate one (local) no-ops.
	if f, ok := sink.(flusher); ok {
		if e := f.Flush(); e != nil {
			fmt.Fprintf(w, "error %s %v\n\n", remoteRef, e)
			return e
		}
	}
	fmt.Fprintf(w, "ok %s\n\n", remoteRef)
	return nil
}

// parseFastExport reads a `git fast-export` stream and returns the tip tree
// (path → content) after linear replay of its blob/commit/M/D commands, whether
// any commit was seen, and the ref the commits target (pushedRef — e.g.
// "refs/heads/main"). FAIL-LOUD: a merge/rename/copy commit, an unknown command, a
// dangling mark, or commits targeting MORE THAN ONE ref returns an error rather
// than silently producing a wrong tree. Scoped to linear single-branch history (the
// push common case); it tolerates git's real `feature`/`option`/`reset`/`from`
// framing lines. The caller enforces the single-mainline policy against pushedRef.
func parseFastExport(r *bufio.Reader) (tree map[string][]byte, sawCommit bool, pushedRef string, err error) {
	tree = map[string][]byte{}
	blobs := map[string][]byte{} // mark (":N") → content

	for {
		line, rerr := readStreamLine(r)
		if line == "" {
			if rerr == io.EOF {
				return tree, sawCommit, pushedRef, nil
			}
			if rerr != nil {
				return nil, false, "", rerr
			}
			continue // blank separator
		}

		switch {
		case line == "done":
			return tree, sawCommit, pushedRef, nil

		case line == "blob":
			mark, content, berr := readBlob(r)
			if berr != nil {
				return nil, false, "", berr
			}
			if mark != "" {
				blobs[mark] = content
			}

		case strings.HasPrefix(line, "commit "):
			sawCommit = true
			// Capture the commit's target ref. A linear single-branch push targets
			// exactly one ref; commits naming a second distinct ref are a multi-ref
			// push we do not support — fail loud rather than collapse them.
			ref := strings.TrimSpace(strings.TrimPrefix(line, "commit "))
			if pushedRef != "" && ref != pushedRef {
				return nil, false, "", fmt.Errorf("gitremote: fast-export targets multiple refs (%q and %q) — single-branch push only", pushedRef, ref)
			}
			pushedRef = ref
			if cerr := consumeCommitHeader(r); cerr != nil {
				return nil, false, "", cerr
			}

		case strings.HasPrefix(line, "reset "),
			strings.HasPrefix(line, "from "),
			strings.HasPrefix(line, "mark "),
			strings.HasPrefix(line, "feature "),
			strings.HasPrefix(line, "option "):
			// Ref/parent/mark/feature/option framing — irrelevant to tip-tree replay.

		case line == "deleteall":
			tree = map[string][]byte{}

		case strings.HasPrefix(line, "M "):
			if merr := applyModify(r, line, tree, blobs); merr != nil {
				return nil, false, "", merr
			}

		case strings.HasPrefix(line, "D "):
			path := unquotePath(strings.TrimSpace(strings.TrimPrefix(line, "D ")))
			delete(tree, path)

		case strings.HasPrefix(line, "merge "),
			strings.HasPrefix(line, "C "),
			strings.HasPrefix(line, "R "):
			return nil, false, "", fmt.Errorf("gitremote: unsupported fast-export command %q (merge/copy/rename — linear single-branch only in this increment)", line)

		default:
			return nil, false, "", fmt.Errorf("gitremote: unrecognized fast-export line %q", line)
		}

		if rerr == io.EOF {
			return tree, sawCommit, pushedRef, nil
		}
		if rerr != nil {
			return nil, false, "", rerr
		}
	}
}

// readBlob reads the "mark"(optional) + "data <n>" + payload after a `blob`.
func readBlob(r *bufio.Reader) (mark string, content []byte, err error) {
	line, lerr := readStreamLine(r)
	if lerr != nil && lerr != io.EOF {
		return "", nil, lerr
	}
	if strings.HasPrefix(line, "mark ") {
		mark = strings.TrimSpace(strings.TrimPrefix(line, "mark "))
		if line, lerr = readStreamLine(r); lerr != nil && lerr != io.EOF {
			return "", nil, lerr
		}
	}
	if !strings.HasPrefix(line, "data ") {
		return "", nil, fmt.Errorf("gitremote: blob: expected `data`, got %q", line)
	}
	content, err = readData(r, line)
	return mark, content, err
}

// consumeCommitHeader reads a commit's mark/author/committer/encoding lines (any
// order, any absent) up to and including the `data <n>` message block, discarding
// them — the tip-tree replay needs none of the commit metadata.
func consumeCommitHeader(r *bufio.Reader) error {
	for {
		line, lerr := readStreamLine(r)
		if lerr != nil && lerr != io.EOF {
			return lerr
		}
		switch {
		case strings.HasPrefix(line, "mark "),
			strings.HasPrefix(line, "author "),
			strings.HasPrefix(line, "committer "),
			strings.HasPrefix(line, "encoding "):
			// skip
		case strings.HasPrefix(line, "data "):
			if _, err := readData(r, line); err != nil {
				return err
			}
			return nil
		default:
			return fmt.Errorf("gitremote: commit header: unexpected line %q (expected data block)", line)
		}
		if lerr == io.EOF {
			return fmt.Errorf("gitremote: commit header ended before data block")
		}
	}
}

// applyModify applies an `M <mode> <dataref> <path>` line. dataref is a blob mark
// (":N") or `inline` (content follows as a data block).
func applyModify(r *bufio.Reader, line string, tree, blobs map[string][]byte) error {
	rest := strings.TrimPrefix(line, "M ")
	parts := strings.SplitN(rest, " ", 3)
	if len(parts) != 3 {
		return fmt.Errorf("gitremote: malformed M line %q", line)
	}
	dataref, rawPath := parts[1], parts[2]
	path := unquotePath(rawPath)

	if dataref == "inline" {
		dline, lerr := readStreamLine(r)
		if lerr != nil && lerr != io.EOF {
			return lerr
		}
		if !strings.HasPrefix(dline, "data ") {
			return fmt.Errorf("gitremote: M inline %q: expected data block, got %q", path, dline)
		}
		content, err := readData(r, dline)
		if err != nil {
			return err
		}
		tree[path] = content
		return nil
	}

	content, ok := blobs[dataref]
	if !ok {
		return fmt.Errorf("gitremote: M %q references unknown blob mark %q", path, dataref)
	}
	tree[path] = content
	return nil
}

// readStreamLine reads one newline-terminated command line, trimmed of the LF. A
// final unterminated line is returned with io.EOF.
func readStreamLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	return strings.TrimRight(line, "\n"), err
}

// readData reads a `data <n>` payload: exactly n bytes, then the single optional
// LF git emits after binary data.
func readData(r *bufio.Reader, dataLine string) ([]byte, error) {
	nStr := strings.TrimSpace(strings.TrimPrefix(dataLine, "data "))
	n, err := strconv.Atoi(nStr)
	if err != nil {
		return nil, fmt.Errorf("gitremote: bad data length %q: %w", nStr, err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("gitremote: short read of %d data bytes: %w", n, err)
	}
	if b, perr := r.ReadByte(); perr == nil && b != '\n' {
		_ = r.UnreadByte()
	}
	return buf, nil
}

// unquotePath undoes git's C-style path quoting. Unquotable input is returned
// as-is (simple paths are never quoted).
func unquotePath(p string) string {
	if len(p) >= 2 && p[0] == '"' && p[len(p)-1] == '"' {
		if unq, err := strconv.Unquote(p); err == nil {
			return unq
		}
	}
	return p
}
