// crossfile_test.go — tests for the Phase-1d advisory cross-file
// conflict heuristic (crossfile.go).
//
// # Thesis claims proven here
//
//	X1. RenameVsRemoteReference     — RenameSymbol(getUser) in auth.go +
//	                                   EditStatement referencing getUser in
//	                                   session.go → CrossFileSuspect
//	X2. DeleteFunctionVsReference   — DeleteFunction(getUser) + remote
//	                                   AddFunction body calling getUser →
//	                                   CrossFileSuspect
//	X3. DeleteDeclVsReference       — DeleteDecl(Config) + remote EditDecl
//	                                   referencing Config → CrossFileSuspect
//	X4. SubstringDoesNotMatch       — getUser change vs remote body that only
//	                                   contains getUserProfile → Independent
//	X5. SameFileUnaffected          — same-file pairs never reach the
//	                                   cross-file path; existing verdicts hold
//	X6. Symmetric                   — Detect(a,b) == Detect(b,a) for the new
//	                                   cross-file pairs
//	X7. NonReferencingStaysIndependent — identity change + unrelated remote
//	                                      body → Independent
//	X8. AddRewriteNotIdentityChange — AddFunction / RewriteFunction are NOT
//	                                   identity changers (documented limit)
package conflict

import (
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// helpers --------------------------------------------------------------

func renameGetUser(path string) *operation.RenameSymbol {
	return &operation.RenameSymbol{
		Path: path, OldName: "getUser", NewName: "fetchUser",
		Scope: operation.ScopeRef{Path: path},
	}
}

func editStmtRef(path, funcRef, text string) *operation.EditStatement {
	return &operation.EditStatement{
		Path:      path,
		FuncRef:   funcRef,
		StmtRange: operation.Range{Start: 0, End: 5},
		NewText:   text,
	}
}

// X1 — rename in one file, remote reference in another → suspect.
func TestCrossFile_RenameVsRemoteReference(t *testing.T) {
	a := renameGetUser("auth.go")
	b := editStmtRef("session.go", "handleSession", "user := getUser(id)")
	r := Detect(a, b)
	if r.Verdict != VerdictCrossFileSuspect {
		t.Fatalf("verdict = %s (%s), want CrossFileSuspect", r.Verdict, r.Reason)
	}
}

// X2 — delete-function in one file, remote AddFunction body references
// the deleted name → suspect.
func TestCrossFile_DeleteFunctionVsReference(t *testing.T) {
	a := &operation.DeleteFunction{Path: "auth.go", Name: "getUser"}
	b := &operation.AddFunction{
		Path: "session.go", Name: "loadSession",
		Signature: "func loadSession(id string) Session",
		Body:      "{ return getUser(id).Session }",
		Language:  operation.LanguageGo,
	}
	r := Detect(a, b)
	if r.Verdict != VerdictCrossFileSuspect {
		t.Fatalf("verdict = %s (%s), want CrossFileSuspect", r.Verdict, r.Reason)
	}
}

// X3 — delete-decl in one file, remote EditDecl new-source references
// the deleted name → suspect.
func TestCrossFile_DeleteDeclVsReference(t *testing.T) {
	a := &operation.DeleteDecl{Path: "config.go", DeclKind: operation.DeclKindStruct, Name: "Config"}
	b := &operation.EditDecl{
		Path: "server.go", DeclKind: operation.DeclKindStruct, Name: "Server",
		NewSource: "type Server struct { cfg Config }",
	}
	r := Detect(a, b)
	if r.Verdict != VerdictCrossFileSuspect {
		t.Fatalf("verdict = %s (%s), want CrossFileSuspect", r.Verdict, r.Reason)
	}
}

// X4 — identifier-boundary, NOT substring: getUser change vs a remote
// body that only contains getUserProfile must NOT fire.
func TestCrossFile_SubstringDoesNotMatch(t *testing.T) {
	a := renameGetUser("auth.go")
	b := editStmtRef("session.go", "handleSession", "p := getUserProfile(id)")
	r := Detect(a, b)
	if r.Verdict != VerdictIndependent {
		t.Fatalf("verdict = %s (%s), want Independent (substring must not match)", r.Verdict, r.Reason)
	}
}

// X5 — same-file pairs never reach the cross-file path. A rename and a
// same-file EditStatement on a different symbol stays on the same-file
// rules (different symbols in same file → Independent), NOT suspect.
func TestCrossFile_SameFileUnaffected(t *testing.T) {
	a := renameGetUser("auth.go")
	b := editStmtRef("auth.go", "handleAuth", "user := getUser(id)")
	r := Detect(a, b)
	if r.Verdict == VerdictCrossFileSuspect {
		t.Fatalf("same-file pair produced CrossFileSuspect (%s) — cross-file path must not fire for same path", r.Reason)
	}
}

// X6 — symmetry for the new cross-file pairs.
func TestCrossFile_Symmetric(t *testing.T) {
	cases := []struct {
		name string
		a, b operation.Operation
	}{
		{
			"rename vs remote ref",
			renameGetUser("auth.go"),
			editStmtRef("session.go", "handleSession", "user := getUser(id)"),
		},
		{
			"delete-func vs remote AddFunction body",
			&operation.DeleteFunction{Path: "auth.go", Name: "getUser"},
			&operation.AddFunction{Path: "session.go", Name: "loadSession", Signature: "func loadSession()", Body: "{ getUser() }", Language: operation.LanguageGo},
		},
		{
			"substring non-match",
			renameGetUser("auth.go"),
			editStmtRef("session.go", "handleSession", "p := getUserProfile(id)"),
		},
		{
			"non-referencing remote body",
			renameGetUser("auth.go"),
			editStmtRef("session.go", "handleSession", "x := computeThing(id)"),
		},
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

// X7 — identity change + a remote op whose body does NOT reference the
// changed name stays Independent.
func TestCrossFile_NonReferencingStaysIndependent(t *testing.T) {
	a := renameGetUser("auth.go")
	b := editStmtRef("session.go", "handleSession", "total := sum(a, b)")
	r := Detect(a, b)
	if r.Verdict != VerdictIndependent {
		t.Fatalf("verdict = %s (%s), want Independent (no reference to changed name)", r.Verdict, r.Reason)
	}
}

// X8 — AddFunction / RewriteFunction are NOT identity changers
// (documented limit). A remote body referencing a name they introduce
// must NOT be flagged as a cross-file suspect.
func TestCrossFile_AddRewriteNotIdentityChange(t *testing.T) {
	// AddFunction introduces "newHelper"; a remote op references it.
	// Adding a symbol cannot dangle an existing remote reference, so no
	// suspicion.
	add := &operation.AddFunction{
		Path: "helpers.go", Name: "newHelper",
		Signature: "func newHelper()", Body: "{}", Language: operation.LanguageGo,
	}
	ref := editStmtRef("caller.go", "doWork", "newHelper()")
	if r := Detect(add, ref); r.Verdict != VerdictIndependent {
		t.Fatalf("AddFunction treated as identity change: %s (%s)", r.Verdict, r.Reason)
	}

	// RewriteFunction preserves its signature by contract → not an
	// identity change.
	rw := &operation.RewriteFunction{Path: "helpers.go", Name: "existingHelper", NewBody: "{ return 2 }"}
	ref2 := editStmtRef("caller.go", "doWork", "existingHelper()")
	if r := Detect(rw, ref2); r.Verdict != VerdictIndependent {
		t.Fatalf("RewriteFunction treated as identity change: %s (%s)", r.Verdict, r.Reason)
	}
}
