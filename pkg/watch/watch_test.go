// watch_test.go — proves the Poller's snapshot diff: initial sync reports all
// files, a content edit reports the changed path, a deletion reports it once,
// steady state reports nothing, and the .git dir + ignored op-log are skipped.
// No timers — Poll is called directly, so the test is deterministic and fast.
package watch_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/watch"
)

// write writes content to <root>/<rel>, creating parent dirs, and bumps the
// modtime forward so a same-size rewrite is still detected (filesystems with
// coarse mtime resolution would otherwise hide a fast same-size edit).
func write(t *testing.T, root, rel, content string, mtime time.Time) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestPoller_lifecycle(t *testing.T) {
	root := t.TempDir()
	t0 := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	write(t, root, "a.go", "package a\n", t0)
	write(t, root, "sub/b.go", "package b\n", t0)

	p := watch.NewPoller(root)

	// Initial poll: everything is "changed" (initial sync), nothing deleted.
	changed, deleted, err := p.Poll()
	if err != nil {
		t.Fatalf("poll1: %v", err)
	}
	if want := []string{"a.go", "sub/b.go"}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("initial changed = %v, want %v", changed, want)
	}
	if len(deleted) != 0 {
		t.Fatalf("initial deleted = %v, want none", deleted)
	}

	// Steady state: no edits → nothing reported.
	changed, deleted, _ = p.Poll()
	if len(changed) != 0 || len(deleted) != 0 {
		t.Fatalf("steady-state poll reported changed=%v deleted=%v, want both empty", changed, deleted)
	}

	// Edit a.go (newer mtime) → only a.go is changed.
	write(t, root, "a.go", "package a\n\nfunc A() {}\n", t0.Add(time.Second))
	changed, deleted, _ = p.Poll()
	if want := []string{"a.go"}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("after edit changed = %v, want %v", changed, want)
	}
	if len(deleted) != 0 {
		t.Fatalf("after edit deleted = %v, want none", deleted)
	}

	// Delete sub/b.go → reported as deleted once, then never again.
	if err := os.Remove(filepath.Join(root, "sub/b.go")); err != nil {
		t.Fatal(err)
	}
	changed, deleted, _ = p.Poll()
	if want := []string{"sub/b.go"}; !reflect.DeepEqual(deleted, want) {
		t.Fatalf("after delete deleted = %v, want %v", deleted, want)
	}
	changed, deleted, _ = p.Poll()
	if len(changed) != 0 || len(deleted) != 0 {
		t.Fatalf("delete reported twice: changed=%v deleted=%v", changed, deleted)
	}
}

func TestPoller_ignoresGitDirAndOpLog(t *testing.T) {
	root := t.TempDir()
	t0 := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	write(t, root, "a.go", "package a\n", t0)
	write(t, root, ".git/HEAD", "ref: refs/heads/main\n", t0)
	write(t, root, "ops.jsonl", `{"op_id":"x"}`+"\n", t0)

	opLog := filepath.Join(root, "ops.jsonl")
	p := watch.NewPoller(root, opLog)

	changed, _, err := p.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if want := []string{"a.go"}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("changed = %v, want %v (.git and the op-log must be skipped)", changed, want)
	}
}
