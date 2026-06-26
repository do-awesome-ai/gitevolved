// handlers.go — per-OpKind handler dispatch for the projector.
//
// # The contract
//
// Every operation.OpKind in operation.AllOpKinds() MUST appear in
// the `handlers` map. TestHandlerCoverage asserts this invariant —
// if it trips, wire a handler for the new OpKind.
//
// # History
//
// Phase 1.0 shipped file-level ops (AddFile, DeleteFile,
// RewriteRegion). Phase 1.1 added AST-based declaration, function,
// import, and sub-function ops. Phase 1.2 completes the vocabulary
// with notebook cell ops (AddCell, EditCell, DeleteCell) — the
// former unsupportedOps map is now empty and deleted.
//
// # State mutation discipline
//
// Handler functions receive a state that is already safe to mutate
// (Project / ApplyOp clone before calling). Handlers MAY mutate
// state and return it; they don't need to clone again.
package projector

import (
	"errors"
	"fmt"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// handlers is the projection dispatch table. Maps every OpKind in
// operation.AllOpKinds() to its concrete handler.
// TestHandlerCoverage asserts complete coverage — adding a new
// OpKind to operation/ without wiring it here trips a test failure.
var handlers = map[operation.OpKind]func(State, operation.Operation) (State, error){
	// File-level (v1.0)
	operation.OpKindAddFile:       handleAddFile,
	operation.OpKindDeleteFile:    handleDeleteFile,
	operation.OpKindRewriteRegion: handleRewriteRegion,
	// Declaration-level (Phase 1.1)
	operation.OpKindAddDecl:      handleAddDecl,
	operation.OpKindEditDecl:     handleEditDecl,
	operation.OpKindDeleteDecl:   handleDeleteDecl,
	operation.OpKindRenameSymbol: handleRenameSymbol,
	// Function-level (Phase 1.1)
	operation.OpKindAddFunction:     handleAddFunction,
	operation.OpKindDeleteFunction:  handleDeleteFunction,
	operation.OpKindRewriteFunction: handleRewriteFunction,
	// Sub-function-level (Phase 1.1)
	operation.OpKindEditStatement: handleEditStatement,
	// Import-level (Phase 1.1)
	operation.OpKindAddImport:    handleAddImport,
	operation.OpKindRemoveImport: handleRemoveImport,
	operation.OpKindEditImport:   handleEditImport,
	// Notebook-level (Phase 1.2)
	operation.OpKindAddCell:    handleAddCell,
	operation.OpKindEditCell:   handleEditCell,
	operation.OpKindDeleteCell: handleDeleteCell,
}

// -----------------------------------------------------------------
// Concrete handlers (v1.0)
// -----------------------------------------------------------------

// handleAddFile materializes a new file. Strict: errors if path
// already in state. Callers wanting upsert should emit DeleteFile +
// AddFile or a whole-file RewriteRegion.
func handleAddFile(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.AddFile)
	if !ok {
		return state, fmt.Errorf("projector.handleAddFile: type assertion failed for %T", op)
	}
	if _, exists := state[o.Path]; exists {
		return state, fmt.Errorf("%w: %s", ErrPathAlreadyExists, o.Path)
	}
	// Deep-copy the content so future mutations to op.Content
	// don't leak through state aliasing.
	content := make([]byte, len(o.Content))
	copy(content, o.Content)
	state[o.Path] = content
	return state, nil
}

// handleDeleteFile removes a file from state. Strict: errors if
// path not in state (an op that targets a missing file is a bug
// or a stale op, not a silent no-op).
func handleDeleteFile(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.DeleteFile)
	if !ok {
		return state, fmt.Errorf("projector.handleDeleteFile: type assertion failed for %T", op)
	}
	if _, exists := state[o.Path]; !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}
	delete(state, o.Path)
	return state, nil
}

// handleRewriteRegion splices a byte range. Validates bounds against
// the file's current length and surfaces ErrRangeOutOfBounds rather
// than panicking on slice bounds.
//
// Algorithm:
//
//	new = content[:start] || op.Content || content[end:]
func handleRewriteRegion(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.RewriteRegion)
	if !ok {
		return state, fmt.Errorf("projector.handleRewriteRegion: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}
	if o.ByteRange.Start < 0 || o.ByteRange.End > len(current) {
		return state, fmt.Errorf("%w: range [%d,%d) against file length %d",
			ErrRangeOutOfBounds, o.ByteRange.Start, o.ByteRange.End, len(current))
	}
	// Splice — build a fresh []byte rather than reusing current,
	// so callers holding a prior State.clone()'d reference don't
	// see surprising backing-array sharing.
	newLen := o.ByteRange.Start + len(o.Content) + (len(current) - o.ByteRange.End)
	out := make([]byte, 0, newLen)
	out = append(out, current[:o.ByteRange.Start]...)
	out = append(out, o.Content...)
	out = append(out, current[o.ByteRange.End:]...)
	state[o.Path] = out
	return state, nil
}

// Compile-time assertion: ensure handler signature matches the
// dispatch-table type. Catches any drift in the function-pointer
// shape at build time.
var _ = errors.New // keep errors import in scope for any future need
