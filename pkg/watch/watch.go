// Package watch is the pure change-detection engine of the gitevolved local
// daemon: it answers "which files under this root changed (or were deleted)
// since I last looked?" against a remembered snapshot, with zero dependencies
// beyond the Go standard library.
//
// # Why this exists
//
// The daemon's job is edit → typed op: notice a file changed, hand the new bytes
// to localrepo.RecordFile. The "notice a file changed" half is the only genuinely
// stateful piece, and it is pure business logic — a snapshot diff — that deserves
// to live apart from the daemon's transport (the ticker, signals, process setup)
// so it can be unit-tested without timers or a real watch API.
//
// # Why polling (and why an interface boundary)
//
// Poller walks the tree and compares (modtime, size) per path. That is O(files)
// per poll — fine for a single repo, deliberately dependency-free (no fsnotify),
// and the seam the daemon depends on is the small Snapshot/Poll surface, not the
// walk strategy. A future event-driven backend (fsnotify/FSEvents) can satisfy the
// same "give me the changed paths" contract without the daemon changing — the
// interface keeps that door open rather than baking the polling choice in.
//
// This is an OPEN component of the free local client: pure stdlib, no cloud,
// no do-core, no AWS SDK — no closed-source dependencies.
package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// fileMeta is the cheap fingerprint used to detect a change without reading file
// contents: modification time (unix nanos) + size. A content edit changes at
// least the modtime; a same-size same-mtime write is treated as unchanged (the
// op-log's own no-op drop is the backstop if a change slips through).
type fileMeta struct {
	modNanos int64
	size     int64
}

// Poller walks a root directory and reports which files changed or were deleted
// since the previous Poll. It is NOT safe for concurrent use — the daemon calls
// Poll from a single loop. Construct with NewPoller.
type Poller struct {
	root   string
	ignore map[string]bool // absolute paths to skip (e.g. the op-log file)
	seen   map[string]fileMeta
}

// NewPoller returns a Poller rooted at root. ignorePaths are filesystem paths
// (relative or absolute) that must never be reported — typically the op-log file
// itself (recording it would feed the daemon its own writes) and anything else
// the caller manages. The .git directory is always skipped. The first Poll after
// construction reports every existing file as changed (initial sync).
func NewPoller(root string, ignorePaths ...string) *Poller {
	ignore := make(map[string]bool, len(ignorePaths))
	for _, p := range ignorePaths {
		if abs, err := filepath.Abs(p); err == nil {
			ignore[abs] = true
		}
	}
	return &Poller{root: root, ignore: ignore, seen: map[string]fileMeta{}}
}

// Poll walks the root and returns the repo-relative paths (forward-slash) that
// are new or modified since the last call, plus those that were present before
// and are now gone. The internal snapshot is updated to the current tree, so a
// path is reported at most once per actual change. Results are sorted for
// deterministic processing/testing.
func (p *Poller) Poll() (changed, deleted []string, err error) {
	current := map[string]fileMeta{}
	walkErr := filepath.WalkDir(p.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		abs, aerr := filepath.Abs(path)
		if aerr == nil && p.ignore[abs] {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			// A file that vanished between WalkDir listing it and Info() — treat
			// as not-present this cycle; it'll show as changed or deleted next time.
			if os.IsNotExist(ierr) {
				return nil
			}
			return ierr
		}
		rel, rerr := filepath.Rel(p.root, path)
		if rerr != nil {
			return rerr
		}
		key := filepath.ToSlash(rel)
		current[key] = fileMeta{modNanos: info.ModTime().UnixNano(), size: info.Size()}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}

	for key, meta := range current {
		prev, ok := p.seen[key]
		if !ok || prev != meta {
			changed = append(changed, key)
		}
	}
	for key := range p.seen {
		if _, ok := current[key]; !ok {
			deleted = append(deleted, key)
		}
	}
	p.seen = current
	sort.Strings(changed)
	sort.Strings(deleted)
	return changed, deleted, nil
}
