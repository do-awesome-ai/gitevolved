// localrepo_test.go — proves the local engine composes extract+oplog+projector+
// export correctly: edits record as ops, no-op saves are dropped, Materialize
// reflects the op sequence, ExportToGit mints a git commit whose tree byte-
// matches the projection, turns are monotonic across re-open, and concurrent
// RecordEdits all land.
package localrepo_test

import (
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/localrepo"
)

func openRepo(t *testing.T) *localrepo.Repo {
	t.Helper()
	r, err := localrepo.Open(filepath.Join(t.TempDir(), "ops.jsonl"), "sess-test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.SetClock(func() time.Time { return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC) })
	return r
}

func TestRecordEdit_thenMaterialize(t *testing.T) {
	r := openRepo(t)
	// Add, then rewrite.
	if rec, err := r.RecordEdit("main.go", nil, []byte("package main\n")); err != nil || !rec {
		t.Fatalf("RecordEdit add: rec=%v err=%v, want true/nil", rec, err)
	}
	if rec, err := r.RecordEdit("main.go", []byte("package main\n"), []byte("package main\n\nfunc main() {}\n")); err != nil || !rec {
		t.Fatalf("RecordEdit rewrite: rec=%v err=%v, want true/nil", rec, err)
	}
	state, err := r.Materialize()
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if got, want := string(state["main.go"]), "package main\n\nfunc main() {}\n"; got != want {
		t.Fatalf("Materialize main.go = %q, want %q", got, want)
	}
}

func TestRecordEdit_noChange_isDroppedNotErrored(t *testing.T) {
	r := openRepo(t)
	same := []byte("package main\n")
	rec, err := r.RecordEdit("main.go", same, same)
	if err != nil {
		t.Fatalf("RecordEdit(no-change) erred: %v (a no-op save must not error)", err)
	}
	if rec {
		t.Fatal("RecordEdit(no-change) returned recorded=true, want false (must not log a no-op)")
	}
	// The log must not have grown — Materialize is empty.
	state, _ := r.Materialize()
	if len(state) != 0 {
		t.Fatalf("no-op save grew the projection to %d files, want 0", len(state))
	}
	if r.Turn() != 0 {
		t.Fatalf("no-op save bumped turn to %d, want 0", r.Turn())
	}
}

func TestExportToGit_treeMatchesMaterialize(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	r := openRepo(t)
	if _, err := r.RecordEdit("a.go", nil, []byte("package a\n")); err != nil {
		t.Fatalf("record a: %v", err)
	}
	if _, err := r.RecordEdit("dir/b.go", nil, []byte("package b\n")); err != nil {
		t.Fatalf("record b: %v", err)
	}

	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	sha, err := r.ExportToGit(repo, "dosource: local export test")
	if err != nil {
		t.Fatalf("ExportToGit: %v", err)
	}
	if sha == "" {
		t.Fatal("ExportToGit returned empty SHA")
	}

	state, _ := r.Materialize()
	for path, content := range state {
		out, err := exec.Command("git", "-C", repo, "show", "HEAD:"+path).Output()
		if err != nil {
			t.Errorf("git show HEAD:%s: %v", path, err)
			continue
		}
		if string(out) != string(content) {
			t.Errorf("%s: git tree %q != materialized %q", path, out, content)
		}
	}
}

func TestTurn_monotonicAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ops.jsonl")

	r1, err := localrepo.Open(path, "s")
	if err != nil {
		t.Fatalf("open1: %v", err)
	}
	_, _ = r1.RecordEdit("a.go", nil, []byte("x"))
	_, _ = r1.RecordEdit("b.go", nil, []byte("y"))
	if r1.Turn() != 2 {
		t.Fatalf("after 2 edits Turn=%d, want 2", r1.Turn())
	}

	// Re-open the same log — turn must resume past the logged max, not reset.
	r2, err := localrepo.Open(path, "s")
	if err != nil {
		t.Fatalf("open2: %v", err)
	}
	if r2.Turn() != 2 {
		t.Fatalf("reopened Turn=%d, want 2 (turn must resume from the log, not reset)", r2.Turn())
	}
	if _, err := r2.RecordEdit("c.go", nil, []byte("z")); err != nil {
		t.Fatalf("record after reopen: %v", err)
	}
	if r2.Turn() != 3 {
		t.Fatalf("after reopen+edit Turn=%d, want 3", r2.Turn())
	}
}

func TestConcurrentRecordEdits_allLand(t *testing.T) {
	r := openRepo(t)
	const n = 30
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			p := filepath.Join("pkg", string(rune('a'+i%26))+".go")
			// Distinct paths so each is an independent AddFile (no overwrite races
			// in the assertion); the point is the engine serializes appends safely.
			_, err := r.RecordEdit(p+string(rune('0'+i/26)), nil, []byte("package x\n"))
			if err != nil {
				t.Errorf("concurrent RecordEdit %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if r.Turn() != n {
		t.Fatalf("after %d concurrent edits Turn=%d, want %d", n, r.Turn(), n)
	}
	state, err := r.Materialize()
	if err != nil {
		t.Fatalf("Materialize after concurrent edits: %v", err)
	}
	if len(state) != n {
		t.Fatalf("projected %d files, want %d (lost/corrupted concurrent appends)", len(state), n)
	}
}
