// Package projector folds a sequence of typed operations over a
// baseline project state to produce a materialized State (path →
// content map). It is the substrate that makes the doSource thesis
// "files are projections from operations" implementable.
//
// # Why this exists
//
// In event-sourced doSource, the operation log is the durable
// record. Files are derived. The projector is the derivation
// function: Project(baseline_state, ops) → new_state.
//
// # Scope (Phase 1.0 → 1.1 → 1.2, all shipped)
//
// All 17 OpKinds in operation.AllOpKinds() have concrete handlers:
//
//	Phase 1.0: AddFile, DeleteFile, RewriteRegion (byte-level)
//	Phase 1.1: AddDecl, EditDecl, DeleteDecl, RenameSymbol,
//	           AddFunction, DeleteFunction, RewriteFunction,
//	           EditStatement, AddImport, RemoveImport, EditImport
//	           (line-based heuristic AST)
//	Phase 1.2: AddCell, EditCell, DeleteCell (Jupyter notebook JSON)
//
// The thesis test TestHandlerCoverage asserts every OpKind in
// operation.AllOpKinds() has a handler, so adding a new op kind to
// operation/ without wiring it here trips a test failure rather
// than silently dropping the op.
//
// # State immutability contract
//
// Project returns a FRESH State. The input state is never mutated.
// Callers can re-use the same baseline across multiple Project
// invocations without aliasing concerns. The thesis test
// TestProjectStateImmutable asserts this invariant.
//
// # Partial-state on error
//
// If an op fails mid-sequence, Project returns:
//
//   - the state UP TO BUT NOT INCLUDING the failing op
//   - the error wrapping the failing op's index + kind
//
// This lets callers (extractor, conflict detector) inspect the last
// good state without re-folding from the start.
//
// # Why no formatter integration at v1.0
//
// By design the projector emits canonical-form source and
// runs gofmt/prettier/swift-format INSIDE doSource's internal store.
// v1.0 ships the structural projection (Add/Delete/RewriteRegion);
// formatter wiring lands with the first AST-based handler (Phase 1.1)
// because that's the first point a formatter actually changes
// observable behavior.
package projector

import (
	"errors"
	"fmt"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// State is the materialized projection: relative path → file content.
// Maps are reference types in Go; the projector takes care to never
// alias caller state — see the State.clone() helper and the
// TestProjectStateImmutable thesis test.
type State map[string][]byte

// clone returns a deep copy of s. Used as the FIRST step of every
// Project call so caller state is untouched.
//
// Deep copy semantics: both the map and each []byte value are
// copied. Otherwise a handler that does `state[path] = append(...)`
// could surprise the caller through shared backing arrays.
func (s State) clone() State {
	out := make(State, len(s))
	for k, v := range s {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

// Project folds ops in sequence over a copy of initial. Returns
// fresh State; initial is never mutated.
//
// On any op error: returns the partial state up to (but not
// including) the failing op + a wrapped error naming the op index
// and kind. Callers that want all-or-nothing semantics should
// discard the partial state on error.
func Project(initial State, ops []operation.Operation) (State, error) {
	state := initial.clone()
	for i, op := range ops {
		if op == nil {
			return state, fmt.Errorf("projector.Project: ops[%d] is nil", i)
		}
		next, err := applyOp(state, op)
		if err != nil {
			return state, fmt.Errorf("projector.Project: ops[%d] (%s): %w", i, op.Kind(), err)
		}
		state = next
	}
	return state, nil
}

// ApplyOp applies a single op to state and returns the new state.
// Public for callers (extractor, conflict detector) that want
// per-op observability rather than batch projection.
//
// Like Project, this method does NOT mutate state — it clones,
// applies, and returns the clone.
func ApplyOp(state State, op operation.Operation) (State, error) {
	if op == nil {
		return state, errors.New("projector.ApplyOp: nil Operation")
	}
	next := state.clone()
	return applyOp(next, op)
}

// applyOp is the internal dispatcher. Assumes state is already a
// safe-to-mutate copy. Returns the (possibly-modified) state map
// — handlers may add / delete / replace entries in place since the
// caller (Project, ApplyOp) handed in a clone.
func applyOp(state State, op operation.Operation) (State, error) {
	kind := op.Kind()
	if h, ok := handlers[kind]; ok {
		return h(state, op)
	}
	// Should be unreachable if TestHandlerCoverage stays green —
	// every OpKind in operation.AllOpKinds() must be in handlers.
	// If a new kind ships in operation/ without wiring here, the
	// coverage test trips first.
	return state, fmt.Errorf("%w: %s (no projector wiring; this is a bug)", ErrUnknownOp, kind)
}

// Named errors.
var (
	// ErrUnsupportedOp is returned when an op kind is known to the
	// projector but is not yet implemented at this build's tier.
	// As of Phase 1.2 all ops have handlers, but the sentinel is
	// retained for API compatibility — external callers may still
	// check errors.Is(err, ErrUnsupportedOp).
	ErrUnsupportedOp = errors.New("projector: op not yet supported at this projector tier")

	// ErrUnknownOp is returned for OpKinds that aren't in the
	// handlers map. Should never fire in practice —
	// TestHandlerCoverage asserts this branch is unreachable. If
	// it does fire, treat it as a structural bug: the operation/
	// package added a kind without projector wiring.
	ErrUnknownOp = errors.New("projector: unknown op kind (missing handler wiring)")

	// ErrPathNotFound is returned when an op targets a file that
	// isn't in the current projection state.
	ErrPathNotFound = errors.New("projector: target path not in state")

	// ErrPathAlreadyExists is returned by AddFile when the target
	// path is already in state. AddFile is strict; callers wanting
	// upsert semantics should emit DeleteFile + AddFile or a
	// RewriteRegion covering the whole file.
	ErrPathAlreadyExists = errors.New("projector: target path already in state")

	// ErrRangeOutOfBounds is returned when a RewriteRegion ByteRange
	// extends past the current file length.
	ErrRangeOutOfBounds = errors.New("projector: byte range out of bounds")
)
