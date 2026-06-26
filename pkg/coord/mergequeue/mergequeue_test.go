// mergequeue_test.go — thesis tests for the doSource merge queue.
//
// Each test name follows the TestThesis_ convention: it asserts one
// load-bearing behavioral claim about the merge engine.
package mergequeue

import (
	"testing"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// helper: build a BundleRef with AddFile ops targeting distinct paths.
func makeBundle(sessionID, bundleID, parentSHA string, paths ...string) BundleRef {
	ops := make([]operation.Operation, len(paths))
	for i, p := range paths {
		ops[i] = &operation.AddFile{
			Path:    p,
			Content: []byte("content of " + p),
		}
	}
	return BundleRef{
		SessionID: sessionID,
		BundleID:  bundleID,
		ParentSHA: parentSHA,
		Ops:       ops,
		EnqueuedAt: time.Now(),
	}
}

// helper: build a BundleRef with a single AddFunction op. The body
// includes the sessionID so two sessions adding the same function
// name produce non-identical ops (triggering the symbol-name
// collision rule rather than the identical-op idempotent rule).
func makeFuncBundle(sessionID, bundleID, parentSHA, path, funcName string) BundleRef {
	return BundleRef{
		SessionID: sessionID,
		BundleID:  bundleID,
		ParentSHA: parentSHA,
		Ops: []operation.Operation{
			&operation.AddFunction{
				Path:      path,
				Name:      funcName,
				Signature: "func " + funcName + "()",
				Body:      "{ /* " + sessionID + " */ }",
				Language:  operation.LanguageGo,
			},
		},
		EnqueuedAt: time.Now(),
	}
}

func TestThesis_Enqueue_IncrementsPosition(t *testing.T) {
	q := NewQueue("sha-head-0")

	pos0 := q.Enqueue(makeBundle("s1", "b1", "sha-head-0", "a.go"))
	pos1 := q.Enqueue(makeBundle("s2", "b2", "sha-head-0", "b.go"))
	pos2 := q.Enqueue(makeBundle("s3", "b3", "sha-head-0", "c.go"))

	if pos0 != 0 {
		t.Fatalf("expected first enqueue at position 0, got %d", pos0)
	}
	if pos1 != 1 {
		t.Fatalf("expected second enqueue at position 1, got %d", pos1)
	}
	if pos2 != 2 {
		t.Fatalf("expected third enqueue at position 2, got %d", pos2)
	}

	pending := q.Pending()
	if len(pending) != 3 {
		t.Fatalf("expected 3 pending bundles, got %d", len(pending))
	}
}

func TestThesis_Drain_SingleBundle_FastForward_Lands(t *testing.T) {
	headSHA := "sha-head-1"
	q := NewQueue(headSHA)
	q.Enqueue(makeBundle("s1", "b1", headSHA, "new-file.go"))

	state := projector.State{}
	results := q.Drain(state)

	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	r := results[0]
	if r.BundleID != "b1" {
		t.Errorf("expected BundleID=b1, got %q", r.BundleID)
	}
	if r.Outcome != OutcomeLanded {
		t.Errorf("expected OutcomeLanded, got %q", r.Outcome)
	}
	if r.NewSHA == "" {
		t.Error("expected non-empty NewSHA on landed bundle")
	}
	if r.NewSHA == headSHA {
		t.Error("expected NewSHA to differ from original headSHA")
	}
}

func TestThesis_Drain_TwoBundles_NoConflict_BothLand(t *testing.T) {
	headSHA := "sha-head-2"
	q := NewQueue(headSHA)

	// Two bundles touching different files — no conflict possible.
	q.Enqueue(makeBundle("s1", "b1", headSHA, "alpha.go"))
	q.Enqueue(makeBundle("s2", "b2", headSHA, "beta.go"))

	state := projector.State{}
	results := q.Drain(state)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// First lands via fast-forward.
	if results[0].Outcome != OutcomeLanded {
		t.Errorf("bundle b1: expected OutcomeLanded, got %q (reason: %s)",
			results[0].Outcome, results[0].Reason)
	}

	// Second bundle's ParentSHA matches the ORIGINAL headSHA, not
	// the new one after b1 landed. So it goes through rebase path.
	// But since the ops target different files, rebase succeeds.
	if results[1].Outcome != OutcomeRebased {
		t.Errorf("bundle b2: expected OutcomeRebased, got %q (reason: %s)",
			results[1].Outcome, results[1].Reason)
	}

	// Both should have non-empty NewSHA.
	if results[0].NewSHA == "" || results[1].NewSHA == "" {
		t.Error("expected non-empty NewSHA on both landed bundles")
	}

	// SHAs should differ from each other (different ops).
	if results[0].NewSHA == results[1].NewSHA {
		t.Error("expected different NewSHA for different bundles")
	}
}

func TestThesis_Drain_TwoBundles_Conflict_SecondEscalates(t *testing.T) {
	headSHA := "sha-head-3"
	q := NewQueue(headSHA)

	// Both bundles add a function with the SAME name to the SAME file.
	// This triggers VerdictSemanticConflict in the conflict detector
	// (intra-file symbol-name collision rule).
	q.Enqueue(makeFuncBundle("s1", "b1", headSHA, "auth.go", "validateToken"))
	q.Enqueue(makeFuncBundle("s2", "b2", headSHA, "auth.go", "validateToken"))

	state := projector.State{}
	results := q.Drain(state)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	// First bundle lands (no prior ops to conflict with).
	if results[0].Outcome != OutcomeLanded {
		t.Errorf("bundle b1: expected OutcomeLanded, got %q", results[0].Outcome)
	}

	// Second bundle conflicts with the first (same name, same path).
	if results[1].Outcome != OutcomeConflict {
		t.Errorf("bundle b2: expected OutcomeConflict, got %q (reason: %s)",
			results[1].Outcome, results[1].Reason)
	}
	if results[1].NewSHA != "" {
		t.Error("expected empty NewSHA on conflicting bundle")
	}
}

func TestThesis_Drain_StaleParent_RebasesSuccessfully(t *testing.T) {
	headSHA := "sha-head-4"
	q := NewQueue(headSHA)

	// First bundle fast-forwards and advances HEAD.
	q.Enqueue(makeBundle("s1", "b1", headSHA, "file-a.go"))

	// Second bundle was built against a DIFFERENT (stale) parent.
	// Its ops target a different file, so rebase should succeed.
	q.Enqueue(makeBundle("s2", "b2", "sha-stale-parent", "file-b.go"))

	state := projector.State{}
	results := q.Drain(state)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].Outcome != OutcomeLanded {
		t.Errorf("bundle b1: expected OutcomeLanded, got %q", results[0].Outcome)
	}
	if results[1].Outcome != OutcomeRebased {
		t.Errorf("bundle b2: expected OutcomeRebased, got %q (reason: %s)",
			results[1].Outcome, results[1].Reason)
	}
	if results[1].NewSHA == "" {
		t.Error("expected non-empty NewSHA on rebased bundle")
	}
}

func TestThesis_Drain_StaleParent_RebaseFails_OnConflict(t *testing.T) {
	headSHA := "sha-head-5"
	q := NewQueue(headSHA)

	// First bundle adds validateToken to auth.go and lands.
	q.Enqueue(makeFuncBundle("s1", "b1", headSHA, "auth.go", "validateToken"))

	// Second bundle was built against a stale parent and ALSO adds
	// validateToken to auth.go. Rebase should detect the conflict
	// with the intervening b1 ops.
	q.Enqueue(makeFuncBundle("s2", "b2", "sha-ancient-parent", "auth.go", "validateToken"))

	state := projector.State{}
	results := q.Drain(state)

	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}

	if results[0].Outcome != OutcomeLanded {
		t.Errorf("bundle b1: expected OutcomeLanded, got %q", results[0].Outcome)
	}
	if results[1].Outcome != OutcomeConflict {
		t.Errorf("bundle b2: expected OutcomeConflict, got %q (reason: %s)",
			results[1].Outcome, results[1].Reason)
	}
}

func TestThesis_Drain_EmptyQueue_NoResults(t *testing.T) {
	q := NewQueue("sha-head-6")

	state := projector.State{}
	results := q.Drain(state)

	if results != nil {
		t.Errorf("expected nil results for empty queue, got %v", results)
	}

	// Pending should also be empty.
	if len(q.Pending()) != 0 {
		t.Errorf("expected 0 pending, got %d", len(q.Pending()))
	}
}

func TestThesis_HeadSHA_AdvancesAfterLand(t *testing.T) {
	headSHA := "sha-head-7"
	q := NewQueue(headSHA)

	// Verify initial HEAD.
	if got := q.HeadSHA(); got != headSHA {
		t.Fatalf("initial HeadSHA: expected %q, got %q", headSHA, got)
	}

	// Enqueue and drain a bundle.
	q.Enqueue(makeBundle("s1", "b1", headSHA, "x.go"))
	state := projector.State{}
	results := q.Drain(state)

	if len(results) != 1 || results[0].Outcome != OutcomeLanded {
		t.Fatalf("expected single landed result, got %v", results)
	}

	// HEAD should have advanced.
	newHead := q.HeadSHA()
	if newHead == headSHA {
		t.Error("HeadSHA did not advance after landing a bundle")
	}
	if newHead != results[0].NewSHA {
		t.Errorf("HeadSHA %q != landed NewSHA %q", newHead, results[0].NewSHA)
	}

	// Enqueue and drain a second bundle against the new HEAD.
	q.Enqueue(makeBundle("s2", "b2", newHead, "y.go"))
	results2 := q.Drain(state)

	if len(results2) != 1 || results2[0].Outcome != OutcomeLanded {
		t.Fatalf("expected single landed result for b2, got %v", results2)
	}

	finalHead := q.HeadSHA()
	if finalHead == newHead {
		t.Error("HeadSHA did not advance after landing second bundle")
	}
	if finalHead != results2[0].NewSHA {
		t.Errorf("final HeadSHA %q != b2 NewSHA %q", finalHead, results2[0].NewSHA)
	}
}
