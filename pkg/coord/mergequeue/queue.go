// queue.go — the in-memory merge queue that orders, conflict-checks,
// and lands committed bundles from parallel sessions.
//
// # Threading model
//
// Queue is protected by a sync.Mutex. Enqueue and Drain are safe for
// concurrent use from multiple goroutines. Drain is the only method
// that mutates headSHA — it advances HEAD after each successfully
// landed bundle within a single Drain pass.
//
// # Causal ordering within Drain
//
// Bundles are processed in enqueue order. A bundle whose ParentSHA
// matches the current headSHA is a fast-forward candidate. A bundle
// whose ParentSHA is stale (doesn't match HEAD) requires rebase via
// the Rebase function in rebase.go. Bundles that conflict after
// rebase attempt are returned with OutcomeConflict.
package mergequeue

import (
	"sync"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// Queue is the in-memory merge queue. Create with NewQueue.
type Queue struct {
	mu      sync.Mutex
	pending []BundleRef
	headSHA string

	// landedOps accumulates ops from bundles that have landed during
	// the current Drain pass. Used to detect conflicts between a
	// pending bundle and already-landed bundles within the same Drain.
	landedOps []operation.Operation
}

// NewQueue creates a merge queue initialized at the given branch HEAD.
func NewQueue(headSHA string) *Queue {
	return &Queue{
		headSHA: headSHA,
	}
}

// Enqueue adds a bundle to the merge queue. Returns its 0-based
// position in the pending list.
func (q *Queue) Enqueue(bundle BundleRef) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pending = append(q.pending, bundle)
	return len(q.pending) - 1
}

// Drain processes all pending bundles in causal order and returns
// a MergeResult for each. After Drain completes, the pending list
// is empty.
//
// For each bundle:
//  1. If ParentSHA == headSHA → fast-forward candidate. Check
//     conflicts against already-landed ops in this Drain pass.
//     Land if clean.
//  2. If ParentSHA != headSHA → attempt rebase (validate bundle ops
//     against intervening ops via conflict.Detect).
//  3. If conflicts detected at any stage → OutcomeConflict with
//     diagnostic reason.
//
// state is the current projected state of the branch at headSHA.
// It is used by Rebase to validate ops against the materialized
// content.
func (q *Queue) Drain(state projector.State) []MergeResult {
	q.mu.Lock()
	defer q.mu.Unlock()

	if len(q.pending) == 0 {
		return nil
	}

	results := make([]MergeResult, 0, len(q.pending))
	q.landedOps = nil

	for _, bundle := range q.pending {
		result := q.processBundle(bundle, state)
		if result.Outcome == OutcomeLanded || result.Outcome == OutcomeRebased {
			// Advance HEAD and accumulate landed ops for future
			// conflict checks within this Drain pass.
			q.headSHA = result.NewSHA
			q.landedOps = append(q.landedOps, bundle.Ops...)

			// Advance state by projecting landed ops.
			newState, err := projector.Project(state, bundle.Ops)
			if err == nil {
				state = newState
			}
			// On projection error we keep the old state — the bundle
			// already passed conflict detection, so we land it and let
			// the stale state be a conservative check for subsequent
			// bundles.
		}
		results = append(results, result)
	}

	q.pending = nil
	q.landedOps = nil
	return results
}

// processBundle handles a single bundle within a Drain pass.
// Caller holds q.mu.
func (q *Queue) processBundle(bundle BundleRef, state projector.State) MergeResult {
	// Validate bundle.
	if len(bundle.Ops) == 0 {
		return MergeResult{
			BundleID: bundle.BundleID,
			Outcome:  OutcomeRejected,
			Reason:   "bundle has no operations",
		}
	}

	if bundle.ParentSHA == q.headSHA {
		// Fast-forward path: check conflicts against ops landed
		// earlier in this Drain pass.
		if err := checkConflictsAgainst(bundle.Ops, q.landedOps); err != nil {
			return MergeResult{
				BundleID: bundle.BundleID,
				Outcome:  OutcomeConflict,
				Reason:   err.Error(),
			}
		}
		newSHA := deriveSHA(q.headSHA, bundle.Ops)
		return MergeResult{
			BundleID: bundle.BundleID,
			Outcome:  OutcomeLanded,
			Reason:   "fast-forward merge — ParentSHA matched HEAD",
			NewSHA:   newSHA,
		}
	}

	// Stale parent: attempt rebase.
	if err := Rebase(bundle, state, q.landedOps); err != nil {
		return MergeResult{
			BundleID: bundle.BundleID,
			Outcome:  OutcomeConflict,
			Reason:   "rebase failed: " + err.Error(),
		}
	}
	newSHA := deriveSHA(q.headSHA, bundle.Ops)
	return MergeResult{
		BundleID: bundle.BundleID,
		Outcome:  OutcomeRebased,
		Reason:   "rebased onto current HEAD — ops validated against intervening changes",
		NewSHA:   newSHA,
	}
}

// Pending returns a copy of the bundles waiting to land.
func (q *Queue) Pending() []BundleRef {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]BundleRef, len(q.pending))
	copy(out, q.pending)
	return out
}

// HeadSHA returns the current branch HEAD.
func (q *Queue) HeadSHA() string {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.headSHA
}
