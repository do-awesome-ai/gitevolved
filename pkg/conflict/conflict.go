// Package conflict detects whether two doSource operations can land
// in the same op-log without semantic clash.
//
// # Why this exists
//
// In event-sourced doSource, parallel sessions emit operations
// independently. Most pairs of operations from sibling sessions are
// genuinely independent (different files, different symbols, different
// scopes) — a file-coordination design wrongly treats those
// as collisions to coordinate around. The event-sourced model
// recognizes that the only real collisions are:
//
//   - Spatial overlap: two ops touching overlapping byte ranges in
//     the same file
//   - Symbol dedup: two AddFunction / AddDecl with the same name in
//     the same file
//   - Intent clash: two RenameSymbol on the same OldName to different
//     NewName values
//   - Resource clash: two AddFile to the same path with different
//     content
//   - File-state clash: DeleteFile after Add/Rewrite on same path,
//     or interleaved Delete + later edits
//
// Everything else is Independent — the parallel-session "collisions"
// that motivated Phase 1 mostly disappear once the unit of work is
// typed operations instead of file diffs.
//
// # Cross-file resolution deferred to Phase 2
//
// Some real conflicts require LSP-class symbol resolution to detect:
// session A renames `getUser` in auth.go; session B adds a call to
// `getUser` from session.go. The rename invalidates B's call site.
// v1 conflict detector returns Independent for this pair because both
// ops target different files; the actual conflict surfaces at
// projector-time when B's projection fails to resolve the symbol.
// Phase 2 LSP integration makes this detection precise at conflict
// time, not at projection time.
//
// # API
//
// One pure function:
//
//	conflict.Detect(a, b operation.Operation) Result
//
// Verdicts:
//
//   - VerdictIndependent      ops are commutative; can apply in either
//     order without semantic effect
//   - VerdictSequenceable     ops are NOT commutative but composing
//     them does not produce conflict (e.g.,
//     identical Add — second is a no-op)
//   - VerdictSemanticConflict ops cannot both land — operator (or
//     merge-on-green substrate) must pick
//
// # Symmetry contract
//
// Detect(a, b) == Detect(b, a). The thesis test TestThesis_Symmetric
// asserts this across every op-pair class. Asymmetric verdicts would
// make conflict detection order-dependent, which the merge-on-green
// substrate cannot tolerate.
package conflict

import (
	"reflect"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// Verdict names the relationship between two operations.
type Verdict string

const (
	// VerdictIndependent — ops touch disjoint resources; can apply in
	// either order, both will land, no conflict at any layer.
	VerdictIndependent Verdict = "independent"

	// VerdictSequenceable — ops are related but composable. Typically
	// idempotent (two identical Add/Delete) or strictly serial (Delete
	// then Add of same path). One winner emerges per op-log ordering;
	// the other is a no-op or a deterministic continuation.
	VerdictSequenceable Verdict = "sequenceable"

	// VerdictSemanticConflict — ops cannot both land cleanly. Merge-
	// on-green substrate must escalate (operator picks) or the
	// escalation contract auto-resolves via reformulation.
	VerdictSemanticConflict Verdict = "semantic_conflict"

	// VerdictCrossFileSuspect — ADVISORY, NON-blocking. One op changes
	// a definition's identity (rename / delete-function / delete-decl)
	// in one file while the other op, in a DIFFERENT file, textually
	// references that definition's name at an identifier boundary. The
	// remote reference MAY dangle after the identity-changing op lands.
	//
	// Unlike VerdictSemanticConflict, this NEVER blocks a merge. It is a
	// Phase-1d heuristic hint for operator surfaces + telemetry, emitted
	// in place of the bare "different files → Independent" short-circuit
	// when the cross-file textual signal is present. The projector
	// remains the authoritative cross-file safety net at projection
	// time; this verdict just surfaces the suspicion earlier. The
	// heuristic is deliberately textual (no LSP scope resolution — still
	// Phase 2), so it tolerates name-collision false positives precisely
	// BECAUSE it is advisory. See crossfile.go for the full rationale +
	// documented limits.
	VerdictCrossFileSuspect Verdict = "cross_file_suspect"
)

// Result is what Detect returns: the verdict plus a human-readable
// reason. Reason is for operator-facing surfaces (UI tooltips, log
// lines) and for explainable telemetry. Programs should branch on
// Verdict, not parse Reason.
type Result struct {
	Verdict Verdict
	Reason  string
}

// Detect classifies the relationship between two operations.
// Symmetric: Detect(a, b).Verdict == Detect(b, a).Verdict.
//
// nil arguments are treated defensively: nil-vs-anything returns
// Independent with a diagnostic reason. Callers should not pass nil
// — but the API doesn't panic.
func Detect(a, b operation.Operation) Result {
	if a == nil || b == nil {
		return Result{Verdict: VerdictIndependent, Reason: "nil operation (caller bug; treated as independent)"}
	}

	// Reflexive case: same logical op (structural equality).
	// Content-addressed op-ids would dedup these in the op-log, but
	// the detector takes concrete Operations so we compare structure.
	if reflect.DeepEqual(a, b) {
		return Result{Verdict: VerdictSequenceable, Reason: "identical operations — idempotent under content-addressed op-id"}
	}

	ta := extractTarget(a)
	tb := extractTarget(b)

	// Disjoint paths → independent, EXCEPT for the Phase-1d advisory
	// cross-file heuristic: an identity-changing op (rename / delete-
	// function / delete-decl) in one file whose symbol name is textually
	// referenced (identifier-boundary) in a DIFFERENT file's op body.
	// detectCrossFile is symmetric and NON-blocking; it returns a zero
	// Result (Verdict == "") when no suspicion applies, in which case we
	// fall through to Independent exactly as before. Precise cross-file
	// resolution (scope-aware, signature-aware) is still Phase 2 LSP.
	if ta.Path != "" && tb.Path != "" && ta.Path != tb.Path {
		if cf := detectCrossFile(a, b); cf.Verdict != "" {
			return cf
		}
		return Result{Verdict: VerdictIndependent, Reason: "different files (cross-file conflict resolution is Phase 2)"}
	}

	// Same path: check the op-pair class.
	return detectSamePath(a, b, ta, tb)
}

// target is the conflict-relevant "what does this op affect" tuple,
// extracted once per op so the rule matrix below stays compact.
type target struct {
	Path      string
	Name      string // function / decl / symbol name; "" if N/A
	ByteRange *operation.Range
	Module    string // import module; "" if N/A
	Notebook  string
	CellIdx   int // -1 if N/A
}

func extractTarget(op operation.Operation) target {
	t := target{CellIdx: -1}
	switch o := op.(type) {
	case *operation.AddFile:
		t.Path = o.Path
	case *operation.DeleteFile:
		t.Path = o.Path
	case *operation.AddDecl:
		t.Path = o.Path
		t.Name = o.Name
	case *operation.EditDecl:
		t.Path = o.Path
		t.Name = o.Name
	case *operation.DeleteDecl:
		t.Path = o.Path
		t.Name = o.Name
	case *operation.RenameSymbol:
		t.Path = o.Path
		t.Name = o.OldName
	case *operation.AddFunction:
		t.Path = o.Path
		t.Name = o.Name
	case *operation.DeleteFunction:
		t.Path = o.Path
		t.Name = o.Name
	case *operation.RewriteFunction:
		t.Path = o.Path
		t.Name = o.Name
	case *operation.EditStatement:
		t.Path = o.Path
		t.Name = o.FuncRef
		r := o.StmtRange
		t.ByteRange = &r
	case *operation.AddImport:
		t.Path = o.Path
		t.Module = o.Module
	case *operation.RemoveImport:
		t.Path = o.Path
		t.Module = o.Module
	case *operation.EditImport:
		t.Path = o.Path
		t.Module = o.OldModule
	case *operation.AddCell:
		t.Notebook = o.Notebook
		t.Path = o.Notebook
		t.CellIdx = o.CellIdx
	case *operation.EditCell:
		t.Notebook = o.Notebook
		t.Path = o.Notebook
		t.CellIdx = o.CellRef.Index
	case *operation.DeleteCell:
		t.Notebook = o.Notebook
		t.Path = o.Notebook
		t.CellIdx = o.CellRef.Index
	case *operation.RewriteRegion:
		t.Path = o.Path
		r := o.ByteRange
		t.ByteRange = &r
	}
	return t
}

// detectSamePath dispatches conflict rules for the case where both
// ops touch the same file. Caller guarantees ta.Path == tb.Path.
//
// Rule precedence: file-state clashes (Add+Add, Delete+anything)
// before symbol-name clashes before byte-range overlap before
// "same name, different intent" before "different name, same file"
// (independent).
func detectSamePath(a, b operation.Operation, ta, tb target) Result {
	// File-state: two AddFile to same path with different content.
	if addA, ok := a.(*operation.AddFile); ok {
		if addB, ok := b.(*operation.AddFile); ok {
			if reflect.DeepEqual(addA.Content, addB.Content) {
				return Result{VerdictSequenceable, "two identical AddFile — second is a no-op under content-addressed op-id"}
			}
			return Result{VerdictSemanticConflict, "two AddFile to same path with different content"}
		}
	}

	// File-state: DeleteFile vs anything-on-path (other than another
	// DeleteFile, which would be reflex-equal and caught earlier).
	if _, ok := a.(*operation.DeleteFile); ok {
		if _, ok := b.(*operation.DeleteFile); ok {
			// Two DeleteFiles with same path are structurally equal
			// and would have been caught by the reflexive check.
			// Reach here only if some non-Path field differs, which
			// shouldn't happen for DeleteFile (Path is the only
			// field). Treat as sequenceable.
			return Result{VerdictSequenceable, "two DeleteFile on same path"}
		}
		return Result{VerdictSemanticConflict, "DeleteFile vs other op on same path — file existence ambiguous"}
	}
	if _, ok := b.(*operation.DeleteFile); ok {
		return Result{VerdictSemanticConflict, "DeleteFile vs other op on same path — file existence ambiguous"}
	}

	// Symbol-name dedup: two Add ops with same (path, name) from
	// different sessions. Issue 1.6 from architect review — the
	// canonical case is "both sessions add validateToken to auth.go."
	if isAdditive(a) && isAdditive(b) && ta.Name != "" && ta.Name == tb.Name {
		return Result{VerdictSemanticConflict, "two additive ops with same (path, name) — intra-file symbol-name collision"}
	}

	// Rename clash: same OldName, different NewName.
	if renA, ok := a.(*operation.RenameSymbol); ok {
		if renB, ok := b.(*operation.RenameSymbol); ok {
			if renA.OldName == renB.OldName && renA.NewName != renB.NewName {
				return Result{VerdictSemanticConflict, "two RenameSymbol on same OldName to different NewName"}
			}
		}
	}

	// Byte-range overlap: only meaningful for ops that target a range.
	if ta.ByteRange != nil && tb.ByteRange != nil {
		if rangesOverlap(*ta.ByteRange, *tb.ByteRange) {
			return Result{VerdictSemanticConflict, "overlapping byte ranges in same file"}
		}
		return Result{VerdictIndependent, "non-overlapping byte ranges in same file"}
	}

	// Import-level: same import op (add+add, remove+remove) of same
	// module would be reflex-equal. Different module → independent.
	// Different op kind on same module: not currently classed as
	// conflict at v1 — they're sequenceable (Remove then Add of same
	// module is a no-op pair).
	if ta.Module != "" && tb.Module != "" {
		if ta.Module == tb.Module {
			return Result{VerdictSequenceable, "import ops on same module — sequenceable"}
		}
		return Result{VerdictIndependent, "import ops on different modules"}
	}

	// Notebook cell: same notebook + same cell index → semantic
	// conflict; different cell indices → independent.
	if ta.CellIdx >= 0 && tb.CellIdx >= 0 {
		if ta.CellIdx == tb.CellIdx {
			return Result{VerdictSemanticConflict, "two cell ops on same notebook + cell index"}
		}
		return Result{VerdictIndependent, "cell ops on different cell indices"}
	}

	// Same path, different names (e.g., AddFunction(f) vs AddFunction(g)):
	// independent at file scope. Cross-file/cross-symbol effects are
	// Phase 2 (LSP) territory.
	if ta.Name != "" && tb.Name != "" && ta.Name != tb.Name {
		return Result{VerdictIndependent, "different symbols in same file"}
	}

	// Fallback for op pairs not classified above (e.g., AddFunction +
	// EditStatement targeting different funcs but no name extracted).
	// Default to Independent for v1 — the projector will fail loudly
	// at projection-time if the assumption is wrong, surfacing the
	// real conflict via PartialStateOnError. Better than false-
	// positive conflicts blocking legitimate parallel work.
	return Result{VerdictIndependent, "same path, no detected conflict at v1 rule tier (projection-time check is the safety net)"}
}

// isAdditive reports whether op is one of the "add a thing" kinds
// that participate in intra-file symbol-name dedup.
func isAdditive(op operation.Operation) bool {
	switch op.(type) {
	case *operation.AddDecl, *operation.AddFunction:
		return true
	}
	return false
}

// rangesOverlap reports whether two half-open ranges [A.Start,A.End)
// and [B.Start,B.End) share any byte. Touching boundaries (A.End ==
// B.Start) do NOT overlap — half-open semantics.
func rangesOverlap(a, b operation.Range) bool {
	return a.Start < b.End && b.Start < a.End
}
