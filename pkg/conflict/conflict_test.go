// conflict_test.go — thesis-driven tests for the v1.0 op-pair
// collision detector.
//
// # Thesis claims proven here
//
//	T1. Symmetric                — Detect(a,b) == Detect(b,a) across every pair class
//	T2. ReflexiveIsSequenceable  — Detect(a,a) == Sequenceable (idempotent)
//	T3. DisjointPathsIndependent — different files → Independent (no Phase 2 LSP check)
//	T4. ByteRangeOverlapIsConflict — overlapping ranges → SemanticConflict
//	T5. ByteRangeDisjointIndependent — non-overlapping ranges on same file → Independent
//	T6. ByteRangeTouchingDisjoint — half-open boundaries (A.End==B.Start) → Independent
//	T7. IntraFileNameDedup        — two AddFunction(name=X) on same file → SemanticConflict
//	T8. DifferentSymbolsIndependent — AddFunction(f) vs AddFunction(g) on same file → Independent
//	T9. RenameClash               — RenameSymbol(X→Y) vs RenameSymbol(X→Z) → SemanticConflict (architect's core case)
//	T10. RenameIdentical          — RenameSymbol(X→Y) vs RenameSymbol(X→Y) → Sequenceable
//	T11. AddSamePathDiffContent   — AddFile(a, c1) vs AddFile(a, c2) → SemanticConflict
//	T12. AddSamePathSameContent   — AddFile(a, c) vs AddFile(a, c) → Sequenceable
//	T13. DeleteVsAnythingConflict — DeleteFile + other-op on same path → SemanticConflict
//	T14. CellOpsByIndex           — same notebook+cellIdx → SemanticConflict; different idx → Independent
package conflict

import (
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// -----------------------------------------------------------------
// T1. Symmetric — Detect(a,b) == Detect(b,a)
// -----------------------------------------------------------------
//
// Symmetry is the load-bearing API contract. Without it, conflict
// detection becomes order-dependent and the merge-on-green substrate
// can't trust the verdict.
func TestThesis_Symmetric(t *testing.T) {
	cases := []struct {
		name string
		a, b operation.Operation
	}{
		{"AddFile/AddFile same path", &operation.AddFile{Path: "a", Content: []byte("x")}, &operation.AddFile{Path: "a", Content: []byte("y")}},
		{"AddFile/AddFile diff path", &operation.AddFile{Path: "a", Content: []byte("x")}, &operation.AddFile{Path: "b", Content: []byte("x")}},
		{"RewriteRegion overlap", &operation.RewriteRegion{Path: "a", ByteRange: operation.Range{Start: 0, End: 10}, Content: []byte("x")}, &operation.RewriteRegion{Path: "a", ByteRange: operation.Range{Start: 5, End: 15}, Content: []byte("y")}},
		{"RewriteRegion disjoint", &operation.RewriteRegion{Path: "a", ByteRange: operation.Range{Start: 0, End: 5}, Content: []byte("x")}, &operation.RewriteRegion{Path: "a", ByteRange: operation.Range{Start: 10, End: 15}, Content: []byte("y")}},
		{"AddFunction same name", &operation.AddFunction{Path: "a", Name: "f", Signature: "func f()", Language: operation.LanguageGo}, &operation.AddFunction{Path: "a", Name: "f", Signature: "func f()", Body: "{}", Language: operation.LanguageGo}},
		{"RenameSymbol clash", &operation.RenameSymbol{Path: "a", OldName: "X", NewName: "Y", Scope: operation.ScopeRef{Path: "a"}}, &operation.RenameSymbol{Path: "a", OldName: "X", NewName: "Z", Scope: operation.ScopeRef{Path: "a"}}},
		{"DeleteFile vs AddFile", &operation.DeleteFile{Path: "a"}, &operation.AddFile{Path: "a", Content: []byte("x")}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ab := Detect(c.a, c.b)
			ba := Detect(c.b, c.a)
			if ab.Verdict != ba.Verdict {
				t.Errorf("symmetry violated: ab=%s ba=%s\n  reason ab: %s\n  reason ba: %s",
					ab.Verdict, ba.Verdict, ab.Reason, ba.Reason)
			}
		})
	}
}

// -----------------------------------------------------------------
// T2. ReflexiveIsSequenceable — Detect(a,a) is Sequenceable
// -----------------------------------------------------------------
//
// Structural equality means content-addressed identity (same op-id).
// The op-log naturally dedupes, so the detector treats identical-vs-
// identical as Sequenceable (one wins; the other is a no-op).
func TestThesis_ReflexiveIsSequenceable(t *testing.T) {
	op := &operation.AddFunction{Path: "a.go", Name: "f", Signature: "func f()", Language: operation.LanguageGo}
	r := Detect(op, op)
	if r.Verdict != VerdictSequenceable {
		t.Errorf("Detect(a,a) = %s (%s), want Sequenceable", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T3. DisjointPathsIndependent — different files = Independent
// -----------------------------------------------------------------
//
// v1 explicitly defers cross-file conflict resolution to Phase 2 LSP.
// Any pair targeting different paths is Independent at this tier.
// The projector's PartialStateOnError catches cross-file failures at
// projection-time if the assumption is wrong.
func TestThesis_DisjointPathsIndependent(t *testing.T) {
	a := &operation.AddFunction{Path: "auth.go", Name: "validateToken", Signature: "func validateToken()", Language: operation.LanguageGo}
	b := &operation.AddFunction{Path: "session.go", Name: "validateToken", Signature: "func validateToken()", Language: operation.LanguageGo}
	r := Detect(a, b)
	if r.Verdict != VerdictIndependent {
		t.Errorf("disjoint paths verdict = %s (%s), want Independent", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T4. ByteRangeOverlapIsConflict
// -----------------------------------------------------------------
//
// Two RewriteRegion ops with overlapping ranges in the same file
// cannot both land cleanly — one would partially overwrite the other.
func TestThesis_ByteRangeOverlapIsConflict(t *testing.T) {
	a := &operation.RewriteRegion{Path: "a.go", ByteRange: operation.Range{Start: 0, End: 10}, Content: []byte("x")}
	b := &operation.RewriteRegion{Path: "a.go", ByteRange: operation.Range{Start: 5, End: 15}, Content: []byte("y")}
	r := Detect(a, b)
	if r.Verdict != VerdictSemanticConflict {
		t.Errorf("overlap verdict = %s (%s), want SemanticConflict", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T5. ByteRangeDisjointIndependent
// -----------------------------------------------------------------
//
// Non-overlapping ranges in the same file → independent. Note: byte
// offsets shift after the first op applies, but at conflict-detection
// time we compare INTENT, not realization.
func TestThesis_ByteRangeDisjointIndependent(t *testing.T) {
	a := &operation.RewriteRegion{Path: "a.go", ByteRange: operation.Range{Start: 0, End: 5}, Content: []byte("x")}
	b := &operation.RewriteRegion{Path: "a.go", ByteRange: operation.Range{Start: 100, End: 200}, Content: []byte("y")}
	r := Detect(a, b)
	if r.Verdict != VerdictIndependent {
		t.Errorf("disjoint range verdict = %s (%s), want Independent", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T6. ByteRangeTouchingDisjoint — half-open semantics
// -----------------------------------------------------------------
//
// Half-open ranges [A.Start, A.End) and [B.Start, B.End) where
// A.End == B.Start DO NOT overlap. This is the boundary case the
// detector must get right.
func TestThesis_ByteRangeTouchingDisjoint(t *testing.T) {
	a := &operation.RewriteRegion{Path: "a.go", ByteRange: operation.Range{Start: 0, End: 10}, Content: []byte("x")}
	b := &operation.RewriteRegion{Path: "a.go", ByteRange: operation.Range{Start: 10, End: 20}, Content: []byte("y")}
	r := Detect(a, b)
	if r.Verdict != VerdictIndependent {
		t.Errorf("touching ranges verdict = %s (%s), want Independent (half-open)", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T7. IntraFileNameDedup — the architect's canonical case
// -----------------------------------------------------------------
//
// Issue 1.6 from architect review: two sessions both add validateToken
// to auth.go. File-scope byte-range disjointness would mark these
// Independent, which is wrong — once projected, only one validateToken
// can exist.
func TestThesis_IntraFileNameDedup(t *testing.T) {
	a := &operation.AddFunction{Path: "auth.go", Name: "validateToken", Signature: "func validateToken() error", Body: "return nil", Language: operation.LanguageGo}
	b := &operation.AddFunction{Path: "auth.go", Name: "validateToken", Signature: "func validateToken(t string) error", Body: "return errors.New(\"unimplemented\")", Language: operation.LanguageGo}
	r := Detect(a, b)
	if r.Verdict != VerdictSemanticConflict {
		t.Errorf("intra-file dedup verdict = %s (%s), want SemanticConflict", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T8. DifferentSymbolsIndependent
// -----------------------------------------------------------------
//
// Two AddFunction in the same file but with different names — the
// classic "parallel sessions both adding to auth.go" case that the
// design was designed to handle WITHOUT collision. This test
// proves the design's headline claim.
func TestThesis_DifferentSymbolsIndependent(t *testing.T) {
	a := &operation.AddFunction{Path: "auth.go", Name: "validateToken", Signature: "func validateToken()", Language: operation.LanguageGo}
	b := &operation.AddFunction{Path: "auth.go", Name: "hashPassword", Signature: "func hashPassword()", Language: operation.LanguageGo}
	r := Detect(a, b)
	if r.Verdict != VerdictIndependent {
		t.Errorf("different-symbols verdict = %s (%s), want Independent (headline design claim)", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T9. RenameClash — same OldName, different NewName
// -----------------------------------------------------------------
//
// The architect specifically called this out as the canonical
// "genuine semantic conflict, detected at emit-time not at text-merge."
func TestThesis_RenameClash(t *testing.T) {
	a := &operation.RenameSymbol{Path: "auth.go", OldName: "getUser", NewName: "fetchUser", Scope: operation.ScopeRef{Path: "auth.go"}}
	b := &operation.RenameSymbol{Path: "auth.go", OldName: "getUser", NewName: "loadUser", Scope: operation.ScopeRef{Path: "auth.go"}}
	r := Detect(a, b)
	if r.Verdict != VerdictSemanticConflict {
		t.Errorf("rename clash verdict = %s (%s), want SemanticConflict", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T10. RenameIdentical — both want same rename
// -----------------------------------------------------------------
func TestThesis_RenameIdentical(t *testing.T) {
	a := &operation.RenameSymbol{Path: "auth.go", OldName: "getUser", NewName: "fetchUser", Scope: operation.ScopeRef{Path: "auth.go"}}
	b := &operation.RenameSymbol{Path: "auth.go", OldName: "getUser", NewName: "fetchUser", Scope: operation.ScopeRef{Path: "auth.go"}}
	r := Detect(a, b)
	if r.Verdict != VerdictSequenceable {
		t.Errorf("identical rename verdict = %s (%s), want Sequenceable (reflexive equality)", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T11. AddSamePathDiffContent
// -----------------------------------------------------------------
func TestThesis_AddSamePathDiffContent(t *testing.T) {
	a := &operation.AddFile{Path: "new.go", Content: []byte("package new\n// from A\n")}
	b := &operation.AddFile{Path: "new.go", Content: []byte("package new\n// from B\n")}
	r := Detect(a, b)
	if r.Verdict != VerdictSemanticConflict {
		t.Errorf("AddFile clash verdict = %s (%s), want SemanticConflict", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T12. AddSamePathSameContent
// -----------------------------------------------------------------
func TestThesis_AddSamePathSameContent(t *testing.T) {
	a := &operation.AddFile{Path: "new.go", Content: []byte("package new\n")}
	b := &operation.AddFile{Path: "new.go", Content: []byte("package new\n")}
	r := Detect(a, b)
	if r.Verdict != VerdictSequenceable {
		t.Errorf("identical AddFile verdict = %s (%s), want Sequenceable", r.Verdict, r.Reason)
	}
}

// -----------------------------------------------------------------
// T13. DeleteVsAnythingConflict
// -----------------------------------------------------------------
//
// DeleteFile against any other op on the same path is a file-state
// clash: one session wants the file gone, another wants it edited
// or recreated. Operator (or escalation contract) must decide.
func TestThesis_DeleteVsAnythingConflict(t *testing.T) {
	cases := []struct {
		name  string
		other operation.Operation
	}{
		{"vs AddFile", &operation.AddFile{Path: "a.go", Content: []byte("x")}},
		{"vs RewriteRegion", &operation.RewriteRegion{Path: "a.go", ByteRange: operation.Range{Start: 0, End: 1}, Content: []byte("x")}},
		{"vs AddFunction", &operation.AddFunction{Path: "a.go", Name: "f", Signature: "func f()", Language: operation.LanguageGo}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Detect(&operation.DeleteFile{Path: "a.go"}, c.other)
			if r.Verdict != VerdictSemanticConflict {
				t.Errorf("Delete %s verdict = %s (%s), want SemanticConflict", c.name, r.Verdict, r.Reason)
			}
		})
	}
}

// -----------------------------------------------------------------
// T14. CellOpsByIndex
// -----------------------------------------------------------------
//
// Same notebook + same cell index → conflict. Different cell indices
// → independent. v1 uses index as cell identity; Phase 2 will add
// stable cell IDs.
func TestThesis_CellOpsByIndex(t *testing.T) {
	sameCell := Detect(
		&operation.EditCell{Notebook: "n.ipynb", CellRef: operation.CellRef{Index: 3}, NewSource: "print(1)"},
		&operation.EditCell{Notebook: "n.ipynb", CellRef: operation.CellRef{Index: 3}, NewSource: "print(2)"},
	)
	if sameCell.Verdict != VerdictSemanticConflict {
		t.Errorf("same cell verdict = %s (%s), want SemanticConflict", sameCell.Verdict, sameCell.Reason)
	}

	diffCell := Detect(
		&operation.EditCell{Notebook: "n.ipynb", CellRef: operation.CellRef{Index: 3}, NewSource: "print(1)"},
		&operation.EditCell{Notebook: "n.ipynb", CellRef: operation.CellRef{Index: 4}, NewSource: "print(2)"},
	)
	if diffCell.Verdict != VerdictIndependent {
		t.Errorf("different cells verdict = %s (%s), want Independent", diffCell.Verdict, diffCell.Reason)
	}
}

// -----------------------------------------------------------------
// Defensive: nil handling
// -----------------------------------------------------------------

func TestNilOpDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Detect(nil, op) panicked: %v", r)
		}
	}()
	r := Detect(nil, &operation.AddFile{Path: "a", Content: []byte("x")})
	if r.Verdict != VerdictIndependent {
		t.Errorf("nil verdict = %s, want Independent (defensive)", r.Verdict)
	}
}
