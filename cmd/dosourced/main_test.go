// main_test.go — proves the daemon's record loop without timers: pollOnce over a
// real Poller + localrepo records adds, rewrites, and deletions into the op-log,
// and serve does a final sweep when its context is cancelled. A per-file read
// error is skipped, not fatal.
package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/localrepo"
	"github.com/do-awesome-ai/gitevolved/pkg/watch"
)

func newRepo(t *testing.T, root string) *localrepo.Repo {
	t.Helper()
	r, err := localrepo.Open(filepath.Join(root, "ops.jsonl"), "daemon-test")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r.SetClock(func() time.Time { return time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC) })
	return r
}

func TestPollOnce_recordsAddsRewritesDeletes(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root)
	opLog := filepath.Join(root, "ops.jsonl")
	poller := watch.NewPoller(root, opLog)
	var logw bytes.Buffer

	// Add two files, poll → both recorded.
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pollOnce(poller, repo, root, &logw); err != nil {
		t.Fatalf("pollOnce add: %v", err)
	}
	state, _ := repo.Materialize()
	if len(state) != 2 {
		t.Fatalf("after add, projection has %d files, want 2", len(state))
	}

	// Rewrite a.go (newer mtime), poll → reflected.
	later := time.Now().Add(time.Second)
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(root, "a.go"), later, later)
	if err := pollOnce(poller, repo, root, &logw); err != nil {
		t.Fatalf("pollOnce rewrite: %v", err)
	}
	state, _ = repo.Materialize()
	if got := string(state["a.go"]); got != "package a\n\nfunc A() {}\n" {
		t.Fatalf("after rewrite a.go = %q", got)
	}

	// Delete b.go, poll → gone from projection.
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	if err := pollOnce(poller, repo, root, &logw); err != nil {
		t.Fatalf("pollOnce delete: %v", err)
	}
	state, _ = repo.Materialize()
	if _, ok := state["b.go"]; ok {
		t.Fatalf("b.go still present after delete: %v", state)
	}
	if len(state) != 1 {
		t.Fatalf("after delete, projection has %d files, want 1", len(state))
	}
}

// fakeRecorder lets us assert a per-file record error is skipped, not fatal.
type fakeRecorder struct{ deletes int }

func (f *fakeRecorder) RecordFile(string, []byte) (bool, error) { return false, errBoom }
func (f *fakeRecorder) RecordDelete(string) (bool, error)       { f.deletes++; return true, nil }

var errBoom = &os.PathError{Op: "boom", Path: "x", Err: os.ErrInvalid}

func TestRecordChanges_perFileErrorSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logw bytes.Buffer
	fr := &fakeRecorder{}
	// RecordFile errors for a.go; recordChanges must log + continue (not panic/return).
	recordChanges(fr, root, []string{"a.go"}, []string{"gone.go"}, &logw)
	if fr.deletes != 1 {
		t.Fatalf("deletion not processed after a record error (deletes=%d)", fr.deletes)
	}
	if !bytes.Contains(logw.Bytes(), []byte("record a.go")) {
		t.Fatalf("record error not logged: %q", logw.String())
	}
}

func TestServe_finalSweepOnCancel(t *testing.T) {
	root := t.TempDir()
	repo := newRepo(t, root)
	poller := watch.NewPoller(root, filepath.Join(root, "ops.jsonl"))
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Already-cancelled context: serve must still do exactly one final sweep and
	// return, recording the pre-existing file.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := serve(ctx, poller, repo, root, time.Hour, &bytes.Buffer{}); err != nil {
		t.Fatalf("serve: %v", err)
	}
	state, _ := repo.Materialize()
	if _, ok := state["a.go"]; !ok {
		t.Fatalf("final sweep did not record a.go: %v", state)
	}
}
