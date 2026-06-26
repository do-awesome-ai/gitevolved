// Package mergequeue is the doSource merge engine. It receives
// committed bundles from parallel sessions and lands them on the
// shared branch in causal order, using the conflict detector to
// gate or auto-land.
//
// # Why this exists
//
// In event-sourced doSource, parallel sessions emit operations
// independently and commit them as bundles. Each bundle carries a
// ParentSHA — the branch HEAD the session was working against when
// it built the bundle. The merge queue is the substrate that:
//
//  1. Receives "this session's bundle is ready to merge" signals
//  2. Orders them by causal dependency (ParentSHA → child SHA)
//  3. Detects conflicts between pending bundles using the conflict
//     package
//  4. Applies merge-on-green: no conflicts → auto-land; conflicts →
//     escalate
//
// # Open/closed split (D4)
//
// This is the OPEN half of the doSource merge-queue seam — the open half (the
// closed platform has the durable one): the FREE, offline, single-machine
// merge engine — the local Queue (Drain/Rebase, the provisional same-machine
// referee), MemEnqueuer, and the BundleRef/MergeResult/Enqueuer contract — that
// runs in the gitevolved local daemon. The closed platform ships the durable,
// fleet-global, cross-machine DDBStore (the PAID cloud Enqueuer), which imports
// this package and satisfies the Enqueuer interface (inverted dep: closed→open).
//
// # Relationship to DDB storage
//
// The closed DDB enqueuer stores MQ#<enqueuedAt> entries. This package is the
// in-memory processing engine — it receives deserialized BundleRefs, orders and
// conflict-checks them, and returns MergeResults. The durable DDB read/write is
// handled by the closed platform (wiring layer).
//
// # Design decisions
//
//   - Drain is synchronous and processes all pending bundles in one
//     pass. Async/event-driven drain is Phase 2 (Lambda trigger from
//     DDB Streams).
//   - Rebase re-validates bundle ops against intervening ops using the
//     conflict detector. If any pair is SemanticConflict, the rebase
//     fails and the bundle escalates.
//   - HeadSHA advances after each landed bundle within a Drain call,
//     so subsequent bundles in the same Drain see the updated HEAD.
//
// # Pointers
//
// - conflict detector: github.com/do-awesome-ai/gitevolved/pkg/conflict/
// - projector (State type): github.com/do-awesome-ai/gitevolved/pkg/projector/
// - operation types: github.com/do-awesome-ai/gitevolved/pkg/operation/
// - DDB storage layout: internal/dosource/storage/
package mergequeue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// BundleRef is a reference to a committed bundle ready to merge.
// It carries the ops and causal context needed for conflict
// detection and rebase.
type BundleRef struct {
	// SessionID identifies the session that produced this bundle.
	SessionID string

	// BundleID is the content-addressed bundle identifier.
	BundleID string

	// ParentSHA is the branch HEAD this bundle was built against.
	// Used to determine whether a fast-forward or rebase is needed.
	ParentSHA string

	// Ops is the ordered sequence of operations in this bundle.
	Ops []operation.Operation

	// EnqueuedAt is wall-clock time when the bundle entered the queue.
	EnqueuedAt time.Time
}

// MergeResult is the outcome of attempting to land a single bundle.
type MergeResult struct {
	// BundleID identifies which bundle this result pertains to.
	BundleID string

	// Outcome classifies what happened.
	Outcome Outcome

	// Reason is a human-readable explanation. Programs should branch
	// on Outcome, not parse Reason.
	Reason string

	// NewSHA is populated only when Outcome is OutcomeLanded or
	// OutcomeRebased — it is the new branch HEAD after this bundle
	// was applied.
	NewSHA string
}

// Outcome is the discriminator for merge attempt results.
type Outcome string

const (
	// OutcomeLanded — bundle's ParentSHA matched HEAD, no conflicts
	// detected, auto-merged via fast-forward.
	OutcomeLanded Outcome = "landed"

	// OutcomeRebased — bundle's ParentSHA was stale (behind HEAD),
	// but all ops were validated against the intervening ops and
	// found to be Independent or Sequenceable. Rebased and landed.
	OutcomeRebased Outcome = "rebased"

	// OutcomeConflict — semantic conflict detected between this
	// bundle's ops and either the current state or another pending
	// bundle's ops. Escalated to operator.
	OutcomeConflict Outcome = "conflict"

	// OutcomeRejected — validation failure (e.g., empty ops, nil
	// bundle, duplicate BundleID).
	OutcomeRejected Outcome = "rejected"
)

// deriveSHA computes a synthetic SHA for a new state after landing
// ops on top of a parent SHA. This is a content-addressed hash over
// the parent + the bundle's op kinds and bodies, giving each landed
// state a unique identifier.
//
// In production, this would be replaced by the actual git SHA from
// the committed tree. For the in-memory merge queue, this synthetic
// derivation is sufficient to track HEAD advancement.
func deriveSHA(parentSHA string, ops []operation.Operation) string {
	h := sha256.New()
	h.Write([]byte(parentSHA))
	for _, op := range ops {
		h.Write([]byte(string(op.Kind())))
		body, err := marshalOp(op)
		if err == nil {
			h.Write(body)
		}
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// marshalOp extracts a canonical body from an operation for SHA
// derivation. Falls back to kind-only hashing on error.
func marshalOp(op operation.Operation) ([]byte, error) {
	var env operation.Envelope
	env.CausalParents = nil
	if err := env.Seal(op); err != nil {
		return nil, fmt.Errorf("marshalOp: %w", err)
	}
	return env.Body, nil
}
