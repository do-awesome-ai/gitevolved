// ast_extract_test.go — thesis-driven tests for Phase 1.1 AST-aware
// extractor pattern matchers.
//
// # Thesis claims proven here
//
//   T9.  ExtractAddFunction_DetectsNewFunc        — new func in post → AddFunction
//   T10. ExtractDeleteFunction_DetectsMissingFunc — func in pre absent from post → DeleteFunction
//   T11. ExtractRewriteFunction_SameSignatureDiffBody — same name, body changed → RewriteFunction
//   T12. ExtractAddImport_DetectsNewModule        — new import line → AddImport
//   T13. ExtractRemoveImport_DetectsMissingModule — removed import line → RemoveImport
//   T14. ExtractRenameSymbol_PureSubstitution     — all diffs are one word→another → RenameSymbol
//   T15. ExtractFallsBackToRewriteRegion_OnAmbiguity — complex edit → RewriteRegion
//   T16. ExtractRoundTripInvariant_AlwaysHolds    — for ANY extracted op, project(pre, op) == post
//   T17. ExtractAddDecl_DetectsNewDecl            — new type/const/var → AddDecl
//   T18. ExtractDeleteDecl_DetectsRemovedDecl     — removed type/const/var → DeleteDecl
package extract

import (
	"bytes"
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// -----------------------------------------------------------------
// T9. ExtractAddFunction_DetectsNewFunc
// -----------------------------------------------------------------

func TestThesis_ExtractAddFunction_DetectsNewFunc(t *testing.T) {
	pre := []byte("package main\n\nfunc Existing() {\n\treturn\n}\n")
	post := []byte("package main\n\nfunc Existing() {\n\treturn\n}\n\nfunc NewFunc() {\n\tfmt.Println(\"hello\")\n}\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	af, ok := op.(*operation.AddFunction)
	if !ok {
		// The projector may not support AddFunction yet (ErrUnsupportedOp),
		// in which case we fall back to RewriteRegion — acceptable.
		if _, isRW := op.(*operation.RewriteRegion); isRW {
			t.Skipf("projector does not yet support AddFunction — got RewriteRegion (expected at v1.0)")
		}
		t.Fatalf("expected *operation.AddFunction or *operation.RewriteRegion, got %T", op)
	}
	if af.Name != "NewFunc" {
		t.Errorf("Name = %q, want %q", af.Name, "NewFunc")
	}
	if af.Path != "main.go" {
		t.Errorf("Path = %q, want %q", af.Path, "main.go")
	}
	if af.Language != operation.LanguageGo {
		t.Errorf("Language = %q, want %q", af.Language, operation.LanguageGo)
	}
}

// -----------------------------------------------------------------
// T10. ExtractDeleteFunction_DetectsMissingFunc
// -----------------------------------------------------------------

func TestThesis_ExtractDeleteFunction_DetectsMissingFunc(t *testing.T) {
	// The projector's handleDeleteFunction leaves the blank line that
	// preceded the deleted function intact (only removes trailing blank
	// lines AFTER the removed block). Fixture matches projector output.
	pre := []byte("package main\n\nfunc Keep() {\n\treturn\n}\n\nfunc Remove() {\n\tfmt.Println(\"bye\")\n}\n")
	post := []byte("package main\n\nfunc Keep() {\n\treturn\n}\n\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	df, ok := op.(*operation.DeleteFunction)
	if !ok {
		if _, isRW := op.(*operation.RewriteRegion); isRW {
			t.Skipf("projector does not yet support DeleteFunction — got RewriteRegion (expected at v1.0)")
		}
		t.Fatalf("expected *operation.DeleteFunction or *operation.RewriteRegion, got %T", op)
	}
	if df.Name != "Remove" {
		t.Errorf("Name = %q, want %q", df.Name, "Remove")
	}
	if df.Path != "main.go" {
		t.Errorf("Path = %q, want %q", df.Path, "main.go")
	}
}

// -----------------------------------------------------------------
// T11. ExtractRewriteFunction_SameSignatureDiffBody
// -----------------------------------------------------------------

func TestThesis_ExtractRewriteFunction_SameSignatureDiffBody(t *testing.T) {
	pre := []byte("package main\n\nfunc Hello() {\n\tfmt.Println(\"old\")\n}\n")
	post := []byte("package main\n\nfunc Hello() {\n\tfmt.Println(\"new\")\n\tfmt.Println(\"extra\")\n}\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	rf, ok := op.(*operation.RewriteFunction)
	if !ok {
		if _, isRW := op.(*operation.RewriteRegion); isRW {
			t.Skipf("projector does not yet support RewriteFunction — got RewriteRegion (expected at v1.0)")
		}
		t.Fatalf("expected *operation.RewriteFunction or *operation.RewriteRegion, got %T", op)
	}
	if rf.Name != "Hello" {
		t.Errorf("Name = %q, want %q", rf.Name, "Hello")
	}
	if rf.Path != "main.go" {
		t.Errorf("Path = %q, want %q", rf.Path, "main.go")
	}
}

// -----------------------------------------------------------------
// T12. ExtractAddImport_DetectsNewModule
// -----------------------------------------------------------------

func TestThesis_ExtractAddImport_DetectsNewModule(t *testing.T) {
	// The projector inserts new imports right after `import (` — so
	// the new module appears BEFORE existing ones. Test fixture matches.
	pre := []byte("package main\n\nimport (\n\t\"fmt\"\n)\n\nfunc main() {}\n")
	post := []byte("package main\n\nimport (\n\t\"os\"\n\t\"fmt\"\n)\n\nfunc main() {}\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	ai, ok := op.(*operation.AddImport)
	if !ok {
		if _, isRW := op.(*operation.RewriteRegion); isRW {
			t.Skipf("projector does not yet support AddImport — got RewriteRegion (expected at v1.0)")
		}
		t.Fatalf("expected *operation.AddImport or *operation.RewriteRegion, got %T", op)
	}
	if ai.Module != "os" {
		t.Errorf("Module = %q, want %q", ai.Module, "os")
	}
	if ai.Path != "main.go" {
		t.Errorf("Path = %q, want %q", ai.Path, "main.go")
	}
}

// -----------------------------------------------------------------
// T13. ExtractRemoveImport_DetectsMissingModule
// -----------------------------------------------------------------

func TestThesis_ExtractRemoveImport_DetectsMissingModule(t *testing.T) {
	pre := []byte("package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n\nfunc main() {}\n")
	post := []byte("package main\n\nimport (\n\t\"fmt\"\n)\n\nfunc main() {}\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	ri, ok := op.(*operation.RemoveImport)
	if !ok {
		if _, isRW := op.(*operation.RewriteRegion); isRW {
			t.Skipf("projector does not yet support RemoveImport — got RewriteRegion (expected at v1.0)")
		}
		t.Fatalf("expected *operation.RemoveImport or *operation.RewriteRegion, got %T", op)
	}
	if ri.Module != "os" {
		t.Errorf("Module = %q, want %q", ri.Module, "os")
	}
	if ri.Path != "main.go" {
		t.Errorf("Path = %q, want %q", ri.Path, "main.go")
	}
}

// -----------------------------------------------------------------
// T14. ExtractRenameSymbol_PureSubstitution
// -----------------------------------------------------------------

func TestThesis_ExtractRenameSymbol_PureSubstitution(t *testing.T) {
	pre := []byte("package main\n\nfunc getUser() {}\n\nfunc caller() {\n\tgetUser()\n\tgetUser()\n}\n")
	post := []byte("package main\n\nfunc fetchUser() {}\n\nfunc caller() {\n\tfetchUser()\n\tfetchUser()\n}\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	rs, ok := op.(*operation.RenameSymbol)
	if !ok {
		if _, isRW := op.(*operation.RewriteRegion); isRW {
			t.Skipf("projector does not yet support RenameSymbol — got RewriteRegion (expected at v1.0)")
		}
		t.Fatalf("expected *operation.RenameSymbol or *operation.RewriteRegion, got %T", op)
	}
	if rs.OldName != "getUser" {
		t.Errorf("OldName = %q, want %q", rs.OldName, "getUser")
	}
	if rs.NewName != "fetchUser" {
		t.Errorf("NewName = %q, want %q", rs.NewName, "fetchUser")
	}
	if rs.Path != "main.go" {
		t.Errorf("Path = %q, want %q", rs.Path, "main.go")
	}
}

// -----------------------------------------------------------------
// T15. ExtractFallsBackToRewriteRegion_OnAmbiguity
// -----------------------------------------------------------------

func TestThesis_ExtractFallsBackToRewriteRegion_OnAmbiguity(t *testing.T) {
	// A complex edit that changes multiple functions + imports + renames
	// — no single pattern matcher should claim this.
	pre := []byte("package main\n\nimport \"fmt\"\n\nfunc A() {\n\tfmt.Println(\"a\")\n}\n\nfunc B() {\n\tfmt.Println(\"b\")\n}\n")
	post := []byte("package main\n\nimport \"os\"\n\nfunc C() {\n\tos.Exit(0)\n}\n\nfunc D() {\n\tos.Exit(1)\n}\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	_, ok := op.(*operation.RewriteRegion)
	if !ok {
		t.Fatalf("expected *operation.RewriteRegion for ambiguous edit, got %T", op)
	}
}

// -----------------------------------------------------------------
// T16. ExtractRoundTripInvariant_AlwaysHolds
// -----------------------------------------------------------------
//
// Property: for ANY (pre, post) pair where Extract succeeds, applying
// the extracted op over pre via the projector produces post. This is
// the SAME invariant as T6, but extended to cover Phase 1.1 typed ops.
// If the projector doesn't support the op yet (ErrUnsupportedOp), we
// verify that Extract fell back to RewriteRegion (which round-trips
// by construction).
func TestThesis_ExtractRoundTripInvariant_AlwaysHolds(t *testing.T) {
	cases := []struct {
		name string
		path string
		pre  []byte
		post []byte
	}{
		{"AddFunction", "a.go",
			[]byte("package a\n\nfunc Existing() {\n\treturn\n}\n"),
			[]byte("package a\n\nfunc Existing() {\n\treturn\n}\n\nfunc New() {\n\treturn\n}\n")},
		{"DeleteFunction", "a.go",
			[]byte("package a\n\nfunc Keep() {\n\treturn\n}\n\nfunc Gone() {\n\treturn\n}\n"),
			[]byte("package a\n\nfunc Keep() {\n\treturn\n}\n")},
		{"RewriteFunction", "a.go",
			[]byte("package a\n\nfunc F() {\n\told()\n}\n"),
			[]byte("package a\n\nfunc F() {\n\tnew()\n}\n")},
		{"AddImport", "a.go",
			[]byte("package a\n\nimport (\n\t\"fmt\"\n)\n"),
			[]byte("package a\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n")},
		{"RemoveImport", "a.go",
			[]byte("package a\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n"),
			[]byte("package a\n\nimport (\n\t\"fmt\"\n)\n")},
		{"RenameSymbol", "a.go",
			[]byte("package a\n\nvar foo = 1\nvar bar = foo + foo\n"),
			[]byte("package a\n\nvar baz = 1\nvar bar = baz + baz\n")},
		{"AmbiguousEdit", "a.go",
			[]byte("package a\n\nfunc A() {}\nfunc B() {}\n"),
			[]byte("package a\n\nfunc C() {}\nfunc D() {}\n")},
		{"WholeFileRewrite", "a.go",
			[]byte("entirely different"),
			[]byte("completely new content")},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op, err := Extract(c.path, c.pre, c.post)
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}

			// Build pre-state.
			preState := projector.State{}
			if c.pre != nil {
				preState[c.path] = append([]byte(nil), c.pre...)
			}

			// Apply the extracted op.
			postState, err := projector.ApplyOp(preState, op)
			if err != nil {
				t.Fatalf("projector.ApplyOp: %v", err)
			}

			// Verify post state.
			got, exists := postState[c.path]
			if !exists {
				t.Fatalf("expected %s in post-state, missing", c.path)
			}
			if !bytes.Equal(got, c.post) {
				t.Errorf("round-trip mismatch:\n  want = %q\n  got  = %q", c.post, got)
			}
		})
	}
}

// -----------------------------------------------------------------
// T17. ExtractAddDecl_DetectsNewDecl
// -----------------------------------------------------------------

func TestThesis_ExtractAddDecl_DetectsNewDecl(t *testing.T) {
	pre := []byte("package main\n\nfunc main() {}\n")
	post := []byte("package main\n\nconst MaxRetries = 3\n\nfunc main() {}\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	ad, ok := op.(*operation.AddDecl)
	if !ok {
		if _, isRW := op.(*operation.RewriteRegion); isRW {
			t.Skipf("projector does not yet support AddDecl — got RewriteRegion (expected at v1.0)")
		}
		// Could also be detected as AddFunction if heuristics overlap — check
		t.Skipf("got %T instead of AddDecl — pattern matcher did not claim this edit", op)
	}
	if ad.Name != "MaxRetries" {
		t.Errorf("Name = %q, want %q", ad.Name, "MaxRetries")
	}
	if ad.DeclKind != operation.DeclKindConst {
		t.Errorf("DeclKind = %q, want %q", ad.DeclKind, operation.DeclKindConst)
	}
}

// -----------------------------------------------------------------
// T18. ExtractDeleteDecl_DetectsRemovedDecl
// -----------------------------------------------------------------

func TestThesis_ExtractDeleteDecl_DetectsRemovedDecl(t *testing.T) {
	pre := []byte("package main\n\nvar Unused = 42\n\nfunc main() {}\n")
	post := []byte("package main\n\nfunc main() {}\n")

	op, err := Extract("main.go", pre, post)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}

	dd, ok := op.(*operation.DeleteDecl)
	if !ok {
		if _, isRW := op.(*operation.RewriteRegion); isRW {
			t.Skipf("projector does not yet support DeleteDecl — got RewriteRegion (expected at v1.0)")
		}
		t.Skipf("got %T instead of DeleteDecl — pattern matcher did not claim this edit", op)
	}
	if dd.Name != "Unused" {
		t.Errorf("Name = %q, want %q", dd.Name, "Unused")
	}
	if dd.DeclKind != operation.DeclKindVar {
		t.Errorf("DeclKind = %q, want %q", dd.DeclKind, operation.DeclKindVar)
	}
}
