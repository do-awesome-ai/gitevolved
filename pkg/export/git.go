// Package export turns a doSource operation log + materialized State
// into a git commit. Git is the export format for GitHub-compat with
// non-doSource tools, NOT the substrate — doSource's truth-of-record
// is the operation log itself.
//
// # Why this exists
//
// By design, git becomes a serialization for backwards-compat with
// GitHub / GitLab / Bitbucket and the existing dev ecosystem (CI
// systems, code-review tools, etc.). Customers who want to keep a
// foot in GitHub-world get a faithful git mirror of their doSource
// repo without doSource ever mutating their operator-facing source
// repo.
//
// # Operator-repo invariant
//
// ExportCommit writes to repoPath. CALLERS MUST pass a doSource-
// managed repository, NEVER the operator's source repo (the design
// rule is greenfield: never mutate an existing source repo). The
// function cannot enforce this — it has no way to
// distinguish operator-owned from doSource-owned paths. Production
// wiring at the API layer is responsible for the boundary.
//
// # Forward-only fidelity
//
// The thesis test TestThesis_ForwardOnlyFidelity proves the contract:
// exported commit, applied to a fresh clone, materializes byte-equal
// to the input State. Reverse-direction recovery (parse a git commit
// back into typed ops) is intentionally lossy — git carries unified
// diffs, not typed ops. The trailers preserve op intent in the commit
// message but reconstruction is not a v1 claim.
//
// # Structured trailers
//
// Each operation emits a commit-message trailer line:
//
//	X-DoSource-Operation: <OpType>(<path-or-key>, <name-if-any>)
//
// The trailer format is human-readable AND machine-parseable. Phase 2
// will add a richer JSON-trailer format for round-trippable export
// when bidirectional sync with non-doSource tools becomes critical.
package export

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// CommitOptions controls the metadata of the synthesized commit.
//
// For deterministic export (thesis test TestThesis_DeterministicSHA),
// AuthorDate and CommitterDate MUST be set explicitly — relying on
// the wall clock makes the SHA non-reproducible.
type CommitOptions struct {
	// Subject is the commit-message first line. Required.
	Subject string

	// Body is optional commit-message body inserted between the
	// subject and the trailers. May be empty.
	Body string

	// Author is the author identity in "Name <email>" form.
	// Defaults to "doSource <ops@dosource.local>" if empty.
	Author string

	// AuthorDate sets GIT_AUTHOR_DATE. Required for deterministic
	// commits.
	AuthorDate time.Time

	// Committer is the committer identity. Defaults to Author if empty.
	Committer string

	// CommitterDate sets GIT_COMMITTER_DATE. Required for deterministic
	// commits.
	CommitterDate time.Time
}

// Named errors.
var (
	// ErrNotAGitRepo is returned when repoPath is not a git repository
	// (no .git directory and not a git worktree).
	ErrNotAGitRepo = fmt.Errorf("export: target path is not a git repository")

	// ErrPathTraversal is returned when a State key contains "..", a
	// leading "/", or other forms that would escape repoPath. Defends
	// against malicious or buggy op-extractor output.
	ErrPathTraversal = fmt.Errorf("export: state contains path-traversal-unsafe key")

	// ErrMissingSubject is returned when CommitOptions.Subject is empty.
	ErrMissingSubject = fmt.Errorf("export: CommitOptions.Subject is required")

	// ErrMissingDates is returned when CommitOptions.AuthorDate or
	// CommitterDate is the zero value. Required for deterministic
	// commits.
	ErrMissingDates = fmt.Errorf("export: CommitOptions.AuthorDate and CommitterDate are required")
)

// ExportCommit synthesizes a git commit in repoPath capturing the
// materialized State and a structured trailer per operation.
// Returns the resulting commit SHA.
//
// repoPath MUST be an existing git repository AND a doSource-managed
// path (not the operator's source repo). The function cannot enforce
// the second invariant — callers are responsible.
//
// On error, the working tree may be left in a partial state. Callers
// running against shared repos should hold a lock; for the production
// use case (a fresh doSource-managed bare clone per export), the
// partial-state risk is bounded to the local working tree.
func ExportCommit(repoPath string, state projector.State, envelopes []*operation.Envelope, opts CommitOptions) (string, error) {
	if err := validateOpts(opts); err != nil {
		return "", err
	}
	if err := validateRepoPath(repoPath); err != nil {
		return "", err
	}
	if err := validateStateKeys(state); err != nil {
		return "", err
	}

	// Materialize: delete files not in state, write files in state.
	if err := materializeState(repoPath, state); err != nil {
		return "", fmt.Errorf("export: materialize state: %w", err)
	}

	// Build commit message: subject + body + trailers.
	msg := buildCommitMessage(opts, envelopes)

	// Stage + commit. --force so files the manifest carries that ALSO match a
	// materialized .gitignore are still committed: the lifeboat mirrors the
	// doSource manifest faithfully (the source of truth), and the real repo
	// legitimately tracks some otherwise-ignored paths (e.g. a committed
	// package-lock.json, a checked-in dist/). Without --force, `git add -A`
	// silently DROPPED those (files can go missing from the git mirror) — a
	// silent data-loss for exactly the files an ignore
	// rule would hide.
	if err := RunGit(repoPath, nil, "add", "-A", "--force"); err != nil {
		return "", fmt.Errorf("export: git add: %w", err)
	}

	env := []string{
		"GIT_AUTHOR_NAME=" + parseName(opts.Author),
		"GIT_AUTHOR_EMAIL=" + parseEmail(opts.Author),
		"GIT_AUTHOR_DATE=" + opts.AuthorDate.UTC().Format(time.RFC3339),
		"GIT_COMMITTER_NAME=" + parseName(opts.Committer),
		"GIT_COMMITTER_EMAIL=" + parseEmail(opts.Committer),
		"GIT_COMMITTER_DATE=" + opts.CommitterDate.UTC().Format(time.RFC3339),
	}
	// --allow-empty so an empty-state export against a fresh repo still
	// produces a commit. Useful for "doSource attach" baseline commits.
	if err := RunGit(repoPath, env, "commit", "--allow-empty", "-m", msg); err != nil {
		return "", fmt.Errorf("export: git commit: %w", err)
	}

	// Capture SHA.
	sha, err := RunGitOutput(repoPath, nil, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("export: rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(sha), nil
}

// validateOpts checks required fields on CommitOptions.
func validateOpts(opts CommitOptions) error {
	if opts.Subject == "" {
		return ErrMissingSubject
	}
	if opts.AuthorDate.IsZero() || opts.CommitterDate.IsZero() {
		return ErrMissingDates
	}
	return nil
}

// validateRepoPath asserts repoPath is a git repository.
func validateRepoPath(repoPath string) error {
	gitDir := filepath.Join(repoPath, ".git")
	st, err := os.Stat(gitDir)
	if err != nil {
		return fmt.Errorf("%w: %s (%v)", ErrNotAGitRepo, repoPath, err)
	}
	// .git may be a directory (regular repo) or a file (worktree).
	if !st.IsDir() && st.Size() == 0 {
		return fmt.Errorf("%w: %s (.git is empty)", ErrNotAGitRepo, repoPath)
	}
	return nil
}

// validateStateKeys rejects path-traversal-unsafe state keys.
func validateStateKeys(state projector.State) error {
	for k := range state {
		if k == "" {
			return fmt.Errorf("%w: empty key", ErrPathTraversal)
		}
		if filepath.IsAbs(k) {
			return fmt.Errorf("%w: absolute path %q", ErrPathTraversal, k)
		}
		clean := filepath.Clean(k)
		if strings.HasPrefix(clean, "..") || strings.Contains(clean, string(filepath.Separator)+".."+string(filepath.Separator)) {
			return fmt.Errorf("%w: %q resolves outside repo", ErrPathTraversal, k)
		}
	}
	return nil
}

// fileModeForContent returns the filesystem mode a materialized lifeboat file
// should get. doSource state has no stored mode, so executability is inferred
// from a leading shebang (`#!`) — the origin-independent signal that survives
// into a true DR where the source git repo no longer exists. Scripts → 0755;
// everything else → 0644. (A handful of non-script files committed +x upstream
// lose the bit, which is harmless: they are never exec'd.)
func fileModeForContent(data []byte) os.FileMode {
	if len(data) >= 2 && data[0] == '#' && data[1] == '!' {
		return 0o755
	}
	return 0o644
}

// materializeState makes repoPath's working-tree contents match state:
// files in state are written (creating parent dirs as needed); tracked
// files not in state are removed.
func materializeState(repoPath string, state projector.State) error {
	// Sort keys for deterministic write order — helps SHA stability
	// when filesystem timing matters.
	keys := make([]string, 0, len(state))
	for k := range state {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// Write each file in state.
	want := make(map[string]struct{}, len(state))
	for _, k := range keys {
		full := filepath.Join(repoPath, k)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("mkdir for %s: %w", k, err)
		}
		// Infer the executable bit from a shebang. doSource's content-addressed
		// state carries NO file mode (the /commit-detail manifest exposes none —
		// see divergence.go), so without this every file lands 0644 and a DR
		// rebuild from the lifeboat FAILS the instant a build step execs a script
		// directly (caught 2026-06-24: the Swift "Bootstrap bundled Node.js
		// toolchain" phase hit `Permission denied`; 251 of 255 tracked-executable
		// files are shebang scripts). os.WriteFile's mode is a no-op when the file
		// already exists, so Chmod explicitly to also fix re-drains.
		mode := fileModeForContent(state[k])
		if err := os.WriteFile(full, state[k], mode); err != nil {
			return fmt.Errorf("write %s: %w", k, err)
		}
		if err := os.Chmod(full, mode); err != nil {
			return fmt.Errorf("chmod %s: %w", k, err)
		}
		want[k] = struct{}{}
	}

	// Remove tracked files not in state.
	tracked, err := RunGitOutput(repoPath, nil, "ls-files")
	if err != nil {
		// First commit on fresh repo: ls-files returns empty + no error.
		// Any other error is real.
		return fmt.Errorf("ls-files: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(tracked), "\n") {
		if line == "" {
			continue
		}
		if _, keep := want[line]; keep {
			continue
		}
		full := filepath.Join(repoPath, line)
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", line, err)
		}
	}
	return nil
}

// buildCommitMessage formats Subject + Body + structured trailers.
//
// Format:
//
//	<subject>
//
//	<body, if any>
//
//	X-DoSource-Operation: <OpType>(<key1>=<val1>, ...)
//	X-DoSource-Operation: <OpType>(...)
//	...
func buildCommitMessage(opts CommitOptions, envelopes []*operation.Envelope) string {
	var b bytes.Buffer
	b.WriteString(opts.Subject)
	if opts.Body != "" {
		b.WriteString("\n\n")
		b.WriteString(opts.Body)
	}
	if len(envelopes) > 0 {
		b.WriteString("\n\n")
		for _, env := range envelopes {
			if env == nil {
				continue
			}
			b.WriteString(formatTrailer(env))
			b.WriteString("\n")
		}
	}
	return b.String()
}

// formatTrailer builds one trailer line for an envelope. Format is
// human-readable AND parseable: "X-DoSource-Operation: <kind>(...)"
// where the parenthesized payload is the op's most-identifying fields.
func formatTrailer(env *operation.Envelope) string {
	op, err := env.Decode()
	if err != nil {
		// Fall back to a minimal trailer if Decode fails (shouldn't
		// happen for envelopes produced by Seal, but be robust).
		return fmt.Sprintf("X-DoSource-Operation: %s(op_id=%s)", env.OpType, env.OpID)
	}
	return fmt.Sprintf("X-DoSource-Operation: %s(%s) [op_id=%s]",
		env.OpType, identitySummary(op), env.OpID)
}

// identitySummary returns the "what does this op uniquely target" tuple
// rendered as key=value pairs. Used in the trailer string.
func identitySummary(op operation.Operation) string {
	switch o := op.(type) {
	case *operation.AddFile:
		return fmt.Sprintf("path=%q", o.Path)
	case *operation.DeleteFile:
		return fmt.Sprintf("path=%q", o.Path)
	case *operation.AddDecl:
		return fmt.Sprintf("path=%q, kind=%s, name=%q", o.Path, o.DeclKind, o.Name)
	case *operation.EditDecl:
		return fmt.Sprintf("path=%q, kind=%s, name=%q", o.Path, o.DeclKind, o.Name)
	case *operation.DeleteDecl:
		return fmt.Sprintf("path=%q, kind=%s, name=%q", o.Path, o.DeclKind, o.Name)
	case *operation.RenameSymbol:
		return fmt.Sprintf("path=%q, %s->%s", o.Path, o.OldName, o.NewName)
	case *operation.AddFunction:
		return fmt.Sprintf("path=%q, name=%q, lang=%s", o.Path, o.Name, o.Language)
	case *operation.DeleteFunction:
		return fmt.Sprintf("path=%q, name=%q", o.Path, o.Name)
	case *operation.RewriteFunction:
		return fmt.Sprintf("path=%q, name=%q", o.Path, o.Name)
	case *operation.EditStatement:
		return fmt.Sprintf("path=%q, func=%q, range=[%d,%d)", o.Path, o.FuncRef, o.StmtRange.Start, o.StmtRange.End)
	case *operation.AddImport:
		return fmt.Sprintf("path=%q, module=%q", o.Path, o.Module)
	case *operation.RemoveImport:
		return fmt.Sprintf("path=%q, module=%q", o.Path, o.Module)
	case *operation.EditImport:
		return fmt.Sprintf("path=%q, %s->%s", o.Path, o.OldModule, o.NewModule)
	case *operation.AddCell:
		return fmt.Sprintf("notebook=%q, idx=%d, kind=%s", o.Notebook, o.CellIdx, o.Kind_)
	case *operation.EditCell:
		return fmt.Sprintf("notebook=%q, idx=%d", o.Notebook, o.CellRef.Index)
	case *operation.DeleteCell:
		return fmt.Sprintf("notebook=%q, idx=%d", o.Notebook, o.CellRef.Index)
	case *operation.RewriteRegion:
		return fmt.Sprintf("path=%q, range=[%d,%d)", o.Path, o.ByteRange.Start, o.ByteRange.End)
	default:
		return fmt.Sprintf("kind=%s", op.Kind())
	}
}

// RunGit runs `git <args...>` in repoPath with optional extra env.
// stderr is captured and folded into the returned error on failure.
func RunGit(repoPath string, env []string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// RunGitOutput runs `git <args...>` in repoPath and returns stdout.
func RunGitOutput(repoPath string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repoPath
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out.String(), nil
}

// parseName splits "Name <email>" → "Name". Returns "doSource" if
// the input is empty or malformed.
func parseName(s string) string {
	if s == "" {
		return "doSource"
	}
	if i := strings.LastIndex(s, "<"); i > 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

// parseEmail splits "Name <email>" → "email". Returns
// "ops@dosource.local" if the input is empty or malformed.
func parseEmail(s string) string {
	if s == "" {
		return "ops@dosource.local"
	}
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return s[i+1 : i+j]
		}
	}
	return "ops@dosource.local"
}
