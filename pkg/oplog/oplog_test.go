// oplog_test.go — proves the local op-log's contract: durable append order,
// decode round-trip, unsealed rejection, fail-loud torn-tail reads, concurrency
// safety, and the end-to-end extract → oplog → projector local pipeline.
package oplog_test

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/extract"
	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/oplog"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// sealAddFile builds + seals an AddFile envelope for test data.
func sealAddFile(t *testing.T, path, content string, turn int) *operation.Envelope {
	t.Helper()
	env := &operation.Envelope{
		SourceSession: "test-sess",
		SourceTurn:    turn,
		EmittedAt:     time.Date(2026, 6, 26, 12, 0, turn, 0, time.UTC),
	}
	if err := env.Seal(&operation.AddFile{Path: path, Content: []byte(content)}); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	return env
}

func TestAppend_thenEnvelopes_preservesOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.jsonl")
	log, err := oplog.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	want := []string{"a.go", "b.go", "c.go"}
	for i, p := range want {
		if err := log.Append(sealAddFile(t, p, "package main\n", i+1)); err != nil {
			t.Fatalf("Append %s: %v", p, err)
		}
	}
	envs, err := log.Envelopes()
	if err != nil {
		t.Fatalf("Envelopes: %v", err)
	}
	if len(envs) != len(want) {
		t.Fatalf("got %d envelopes, want %d", len(envs), len(want))
	}
	for i, env := range envs {
		op, derr := env.Decode()
		if derr != nil {
			t.Fatalf("Decode %d: %v", i, derr)
		}
		af, ok := op.(*operation.AddFile)
		if !ok {
			t.Fatalf("entry %d is %T, want *operation.AddFile", i, op)
		}
		if af.Path != want[i] {
			t.Errorf("entry %d path = %q, want %q (append order not preserved)", i, af.Path, want[i])
		}
	}
}

func TestAppend_rejectsUnsealed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.jsonl")
	log, _ := oplog.Open(path)
	// A fresh envelope with no Seal has empty OpID.
	err := log.Append(&operation.Envelope{SourceSession: "s"})
	if err != oplog.ErrUnsealed {
		t.Fatalf("Append(unsealed) = %v, want ErrUnsealed", err)
	}
	// Nothing should have been written.
	n, _ := log.Len()
	if n != 0 {
		t.Fatalf("unsealed append wrote %d entries, want 0", n)
	}
}

func TestOpen_existingFile_appendsNotTruncates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.jsonl")
	log1, _ := oplog.Open(path)
	if err := log1.Append(sealAddFile(t, "a.go", "x", 1)); err != nil {
		t.Fatalf("append1: %v", err)
	}
	// Re-Open the same path — existing entry must survive, new appends extend.
	log2, err := oplog.Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	if err := log2.Append(sealAddFile(t, "b.go", "y", 2)); err != nil {
		t.Fatalf("append2: %v", err)
	}
	n, _ := log2.Len()
	if n != 2 {
		t.Fatalf("after re-open + append, Len=%d, want 2 (Open must not truncate)", n)
	}
}

func TestEnvelopes_tornTail_failsLoud(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.jsonl")
	log, _ := oplog.Open(path)
	if err := log.Append(sealAddFile(t, "good.go", "package main\n", 1)); err != nil {
		t.Fatalf("append: %v", err)
	}
	// Simulate a crash mid-Append: a half-written JSON object with no closing.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open for torn write: %v", err)
	}
	if _, err := f.WriteString(`{"op_id":"truncated","op_type":"AddFile","body":`); err != nil {
		t.Fatalf("torn write: %v", err)
	}
	f.Close()

	_, err = log.Envelopes()
	if err == nil {
		t.Fatal("Envelopes() returned nil error on a torn tail — must fail loud, never return a silent prefix")
	}
	// The good prefix must NOT be returned as if complete.
	t.Logf("torn-tail error (expected): %v", err)
}

func TestConcurrentAppends_allLand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.jsonl")
	log, _ := oplog.Open(path)
	const n = 40
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			if err := log.Append(sealAddFile(t, fmt.Sprintf("f%d.go", i), "package main\n", i+1)); err != nil {
				t.Errorf("concurrent Append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	got, err := log.Len()
	if err != nil {
		t.Fatalf("Len: %v", err)
	}
	if got != n {
		t.Fatalf("concurrent appends landed %d entries, want %d (lost writes / interleaving)", got, n)
	}
	// And the log is still parseable end-to-end (no interleaved/torn lines).
	if _, err := log.Operations(); err != nil {
		t.Fatalf("Operations after concurrent appends: %v (a partial/interleaved write corrupted framing)", err)
	}
}

// TestPipeline_extract_oplog_projector proves the full local pipeline composes:
// an edit → extract.Extract → seal → oplog.Append → oplog.Operations →
// projector.Project materializes the expected file content.
func TestPipeline_extract_oplog_projector(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops.jsonl")
	log, _ := oplog.Open(path)

	// Edit 1: create main.go (pre=nil → AddFile).
	op1, err := extract.Extract("main.go", nil, []byte("package main\n"))
	if err != nil {
		t.Fatalf("Extract add: %v", err)
	}
	// Edit 2: rewrite main.go (pre!=nil, post!=nil → RewriteRegion).
	op2, err := extract.Extract("main.go", []byte("package main\n"), []byte("package main\n\nfunc main() {}\n"))
	if err != nil {
		t.Fatalf("Extract rewrite: %v", err)
	}
	for i, op := range []operation.Operation{op1, op2} {
		env := &operation.Envelope{SourceSession: "s", SourceTurn: i + 1, EmittedAt: time.Now().UTC()}
		if err := env.Seal(op); err != nil {
			t.Fatalf("Seal %d: %v", i, err)
		}
		if err := log.Append(env); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}

	ops, err := log.Operations()
	if err != nil {
		t.Fatalf("Operations: %v", err)
	}
	state, err := projector.Project(projector.State{}, ops)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}
	got := string(state["main.go"])
	want := "package main\n\nfunc main() {}\n"
	if got != want {
		t.Fatalf("projected main.go = %q, want %q", got, want)
	}
}
