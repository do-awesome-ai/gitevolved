// rebase.go — validates a stale-parent bundle's ops against
// intervening ops to determine if auto-rebase is safe.
//
// # Why this exists
//
// When a bundle's ParentSHA doesn't match the current branch HEAD,
// the bundle was built against a stale snapshot. Before landing it,
// we must verify that none of its ops semantically conflict with the
// ops that landed between the bundle's parent and the current HEAD.
//
// # Algorithm
//
// For each op in the bundle, check it against every intervening op
// using conflict.Detect. If ANY pair returns VerdictSemanticConflict,
// the rebase fails and the bundle must be escalated.
//
// VerdictIndependent and VerdictSequenceable are both safe for
// rebase — Independent means no interaction at all; Sequenceable
// means the ops are composable in either order (idempotent or
// deterministically serial).
//
// # Pointers
//
// - conflict.Detect: github.com/do-awesome-ai/gitevolved/pkg/conflict/conflict.go
// - Queue.Drain (caller): queue.go
package mergequeue

import (
	"fmt"

	"github.com/do-awesome-ai/gitevolved/pkg/conflict"
	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// Rebase validates a bundle's ops against the current state and
// intervening ops. Returns nil if all ops are safe to rebase
// (Independent or Sequenceable against every intervening op).
// Returns an error describing the first semantic conflict found.
//
// The state parameter is the current projected state at HEAD. It is
// available for future use (e.g., validating that a RewriteRegion's
// byte range is still valid against the current file content) but
// v1 uses only the op-vs-op conflict check.
func Rebase(bundle BundleRef, _ projector.State, interveningOps []operation.Operation) error {
	return checkConflictsAgainst(bundle.Ops, interveningOps)
}

// checkConflictsAgainst checks every op in bundleOps against every
// op in targetOps using conflict.Detect. Returns the first
// SemanticConflict found, or nil if all pairs are safe.
func checkConflictsAgainst(bundleOps, targetOps []operation.Operation) error {
	for i, bOp := range bundleOps {
		for j, tOp := range targetOps {
			result := conflict.Detect(bOp, tOp)
			if result.Verdict == conflict.VerdictSemanticConflict {
				return fmt.Errorf(
					"semantic conflict: bundle op[%d] (%s) vs landed op[%d] (%s): %s",
					i, bOp.Kind(), j, tOp.Kind(), result.Reason,
				)
			}
		}
	}
	return nil
}
