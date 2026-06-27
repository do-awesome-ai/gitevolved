// ast_handlers_test.go — thesis-driven tests for Phase 1.1 AST-based
// projector handlers.
//
// # Thesis claims proven here
//
//	T11. AddDecl inserts at file end, rejects duplicates
//	T12. EditDecl locates + replaces a named declaration's body
//	T13. DeleteDecl removes lines of a named declaration
//	T14. RenameSymbol replaces all occurrences within file scope
//	T15. AddFunction appends a new function, rejects duplicates
//	T16. DeleteFunction removes matching function block
//	T17. RewriteFunction replaces function body while preserving sig
//	T18. EditStatement splices byte range within function body
//	T19. AddImport inserts into existing import block
//	T20. RemoveImport deletes the import line
//	T21. EditImport renames the module in the import line
//	T22. HandlerCoverage — all 17 OpKinds (14 handlers + 3 unsupported) wired
package projector

import (
	"errors"
	"strings"
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// -----------------------------------------------------------------
// T11. AddDecl — inserts at file end
// -----------------------------------------------------------------

func TestThesis_AddDecl_InsertsAtFileEnd(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n\nvar x = 1\n"),
	}
	op := &operation.AddDecl{
		Path:     "main.go",
		DeclKind: operation.DeclKindStruct,
		Name:     "User",
		Source:   "type User struct {\n\tName string\n}",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["main.go"])
	if !strings.Contains(content, "type User struct {") {
		t.Errorf("expected User struct in output, got:\n%s", content)
	}
	// The original content should still be present.
	if !strings.Contains(content, "var x = 1") {
		t.Errorf("original content lost, got:\n%s", content)
	}
}

func TestThesis_AddDecl_RejectsDuplicate(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n\ntype User struct {\n\tName string\n}\n"),
	}
	op := &operation.AddDecl{
		Path:     "main.go",
		DeclKind: operation.DeclKindStruct,
		Name:     "User",
		Source:   "type User struct {\n\tAge int\n}",
	}

	_, err := ApplyOp(state, op)
	if !errors.Is(err, ErrDuplicateDecl) {
		t.Errorf("expected ErrDuplicateDecl, got %v", err)
	}
}

// -----------------------------------------------------------------
// T12. EditDecl — replaces body
// -----------------------------------------------------------------

func TestThesis_EditDecl_ReplacesBody(t *testing.T) {
	state := State{
		"models.go": []byte("package models\n\ntype Config struct {\n\tHost string\n}\n"),
	}
	op := &operation.EditDecl{
		Path:      "models.go",
		DeclKind:  operation.DeclKindStruct,
		Name:      "Config",
		NewSource: "type Config struct {\n\tHost string\n\tPort int\n}",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["models.go"])
	if !strings.Contains(content, "Port int") {
		t.Errorf("expected Port int in output, got:\n%s", content)
	}
	// Should NOT contain the old single-field version.
	lines := strings.Split(content, "\n")
	hostCount := 0
	for _, l := range lines {
		if strings.Contains(l, "Host string") {
			hostCount++
		}
	}
	if hostCount != 1 {
		t.Errorf("expected exactly 1 'Host string' line, got %d in:\n%s", hostCount, content)
	}
}

func TestThesis_EditDecl_ErrorOnMissing(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n"),
	}
	op := &operation.EditDecl{
		Path:      "main.go",
		DeclKind:  operation.DeclKindStruct,
		Name:      "Ghost",
		NewSource: "type Ghost struct {}",
	}

	_, err := ApplyOp(state, op)
	if !errors.Is(err, ErrDeclNotFound) {
		t.Errorf("expected ErrDeclNotFound, got %v", err)
	}
}

// -----------------------------------------------------------------
// T13. DeleteDecl — removes from state
// -----------------------------------------------------------------

func TestThesis_DeleteDecl_RemovesFromState(t *testing.T) {
	state := State{
		"types.go": []byte("package types\n\ntype Old struct {\n\tA int\n}\n\ntype Keep struct {\n\tB int\n}\n"),
	}
	op := &operation.DeleteDecl{
		Path:     "types.go",
		DeclKind: operation.DeclKindStruct,
		Name:     "Old",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["types.go"])
	if strings.Contains(content, "type Old") {
		t.Errorf("Old struct should have been removed, got:\n%s", content)
	}
	if !strings.Contains(content, "type Keep") {
		t.Errorf("Keep struct should remain, got:\n%s", content)
	}
}

// -----------------------------------------------------------------
// T14. RenameSymbol — all occurrences
// -----------------------------------------------------------------

func TestThesis_RenameSymbol_AllOccurrences(t *testing.T) {
	state := State{
		"auth.go": []byte("package auth\n\nfunc getUser() User {\n\treturn User{}\n}\n\ntype User struct{}\n"),
	}
	op := &operation.RenameSymbol{
		Path:    "auth.go",
		OldName: "User",
		NewName: "Account",
		Scope:   operation.ScopeRef{Path: "auth.go"},
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["auth.go"])
	if strings.Contains(content, "User") {
		t.Errorf("expected all 'User' replaced, got:\n%s", content)
	}
	if !strings.Contains(content, "Account") {
		t.Errorf("expected 'Account' present, got:\n%s", content)
	}
	// Count occurrences.
	if c := strings.Count(content, "Account"); c < 3 {
		t.Errorf("expected at least 3 'Account' occurrences, got %d in:\n%s", c, content)
	}
}

func TestThesis_RenameSymbol_ErrorOnMissing(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n"),
	}
	op := &operation.RenameSymbol{
		Path:    "main.go",
		OldName: "Ghost",
		NewName: "Spirit",
		Scope:   operation.ScopeRef{Path: "main.go"},
	}

	_, err := ApplyOp(state, op)
	if err == nil {
		t.Fatal("expected error for missing symbol, got nil")
	}
}

// -----------------------------------------------------------------
// T15. AddFunction — appends to file
// -----------------------------------------------------------------

func TestThesis_AddFunction_AppendsToFile(t *testing.T) {
	state := State{
		"svc.go": []byte("package svc\n\nfunc existing() {}\n"),
	}
	op := &operation.AddFunction{
		Path:      "svc.go",
		Name:      "newFunc",
		Signature: "func newFunc(x int) error",
		Body:      "\treturn nil",
		Language:  operation.LanguageGo,
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["svc.go"])
	if !strings.Contains(content, "func newFunc(x int) error {") {
		t.Errorf("expected function signature, got:\n%s", content)
	}
	if !strings.Contains(content, "return nil") {
		t.Errorf("expected function body, got:\n%s", content)
	}
}

func TestThesis_AddFunction_RejectsDuplicate(t *testing.T) {
	state := State{
		"svc.go": []byte("package svc\n\nfunc existing() {}\n"),
	}
	op := &operation.AddFunction{
		Path:      "svc.go",
		Name:      "existing",
		Signature: "func existing()",
		Language:  operation.LanguageGo,
	}

	_, err := ApplyOp(state, op)
	if !errors.Is(err, ErrDuplicateFunction) {
		t.Errorf("expected ErrDuplicateFunction, got %v", err)
	}
}

// -----------------------------------------------------------------
// T16. DeleteFunction — removes matching func
// -----------------------------------------------------------------

func TestThesis_DeleteFunction_RemovesMatchingFunc(t *testing.T) {
	state := State{
		"svc.go": []byte("package svc\n\nfunc keep() {\n\t// keep\n}\n\nfunc remove() {\n\t// remove\n}\n"),
	}
	op := &operation.DeleteFunction{
		Path: "svc.go",
		Name: "remove",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["svc.go"])
	if strings.Contains(content, "func remove") {
		t.Errorf("remove function should be gone, got:\n%s", content)
	}
	if !strings.Contains(content, "func keep") {
		t.Errorf("keep function should remain, got:\n%s", content)
	}
}

func TestThesis_DeleteFunction_ErrorOnMissing(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n"),
	}
	op := &operation.DeleteFunction{
		Path: "main.go",
		Name: "ghost",
	}

	_, err := ApplyOp(state, op)
	if !errors.Is(err, ErrFunctionNotFound) {
		t.Errorf("expected ErrFunctionNotFound, got %v", err)
	}
}

// -----------------------------------------------------------------
// T17. RewriteFunction — replaces body
// -----------------------------------------------------------------

func TestThesis_RewriteFunction_ReplacesBody(t *testing.T) {
	state := State{
		"handler.go": []byte("package handler\n\nfunc process() {\n\told := true\n\t_ = old\n}\n"),
	}
	op := &operation.RewriteFunction{
		Path:    "handler.go",
		Name:    "process",
		NewBody: "\tnewLogic := false\n\t_ = newLogic",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["handler.go"])
	if !strings.Contains(content, "newLogic") {
		t.Errorf("expected new body, got:\n%s", content)
	}
	if strings.Contains(content, "old := true") {
		t.Errorf("old body should be gone, got:\n%s", content)
	}
	// Signature should be preserved.
	if !strings.Contains(content, "func process()") {
		t.Errorf("signature should be preserved, got:\n%s", content)
	}
}

// -----------------------------------------------------------------
// T18. EditStatement — splices in function
// -----------------------------------------------------------------

func TestThesis_EditStatement_SplicesInFunction(t *testing.T) {
	state := State{
		"calc.go": []byte("package calc\n\nfunc add(a, b int) int {\n\treturn a + b\n}\n"),
	}
	// The function body is "\treturn a + b\n" — replace "a + b" with "a - b".
	// Body starts at byte 0 of the body content after the `{` line.
	body := "\treturn a + b\n"
	start := strings.Index(body, "a + b")
	end := start + len("a + b")

	op := &operation.EditStatement{
		Path:      "calc.go",
		FuncRef:   "add",
		StmtRange: operation.Range{Start: start, End: end},
		NewText:   "a - b",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["calc.go"])
	if !strings.Contains(content, "return a - b") {
		t.Errorf("expected 'return a - b', got:\n%s", content)
	}
	if strings.Contains(content, "return a + b") {
		t.Errorf("old text should be replaced, got:\n%s", content)
	}
}

func TestThesis_EditStatement_OutOfBounds(t *testing.T) {
	state := State{
		"calc.go": []byte("package calc\n\nfunc tiny() {\n\tx := 1\n}\n"),
	}
	op := &operation.EditStatement{
		Path:      "calc.go",
		FuncRef:   "tiny",
		StmtRange: operation.Range{Start: 0, End: 999},
		NewText:   "y := 2",
	}

	_, err := ApplyOp(state, op)
	if !errors.Is(err, ErrRangeOutOfBounds) {
		t.Errorf("expected ErrRangeOutOfBounds, got %v", err)
	}
}

// -----------------------------------------------------------------
// T19. AddImport — inserts into block
// -----------------------------------------------------------------

func TestThesis_AddImport_InsertsIntoBlock(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n\nimport (\n\t\"fmt\"\n)\n\nfunc main() {}\n"),
	}
	op := &operation.AddImport{
		Path:   "main.go",
		Module: "os",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["main.go"])
	if !strings.Contains(content, "\"os\"") {
		t.Errorf("expected 'os' import, got:\n%s", content)
	}
	// Original import should still be there.
	if !strings.Contains(content, "\"fmt\"") {
		t.Errorf("expected 'fmt' to remain, got:\n%s", content)
	}
}

func TestThesis_AddImport_RejectsDuplicate(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n\nimport (\n\t\"fmt\"\n)\n"),
	}
	op := &operation.AddImport{
		Path:   "main.go",
		Module: "fmt",
	}

	_, err := ApplyOp(state, op)
	if !errors.Is(err, ErrDuplicateImport) {
		t.Errorf("expected ErrDuplicateImport, got %v", err)
	}
}

// -----------------------------------------------------------------
// T20. RemoveImport — deletes line
// -----------------------------------------------------------------

func TestThesis_RemoveImport_DeletesLine(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n\nimport (\n\t\"fmt\"\n\t\"os\"\n)\n"),
	}
	op := &operation.RemoveImport{
		Path:   "main.go",
		Module: "os",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["main.go"])
	if strings.Contains(content, "\"os\"") {
		t.Errorf("expected 'os' removed, got:\n%s", content)
	}
	if !strings.Contains(content, "\"fmt\"") {
		t.Errorf("expected 'fmt' to remain, got:\n%s", content)
	}
}

func TestThesis_RemoveImport_ErrorOnMissing(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n"),
	}
	op := &operation.RemoveImport{
		Path:   "main.go",
		Module: "ghost",
	}

	_, err := ApplyOp(state, op)
	if !errors.Is(err, ErrImportNotFound) {
		t.Errorf("expected ErrImportNotFound, got %v", err)
	}
}

// -----------------------------------------------------------------
// T21. EditImport — replaces module
// -----------------------------------------------------------------

func TestThesis_EditImport_ReplacesModule(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n\nimport (\n\t\"old/pkg\"\n)\n"),
	}
	op := &operation.EditImport{
		Path:      "main.go",
		OldModule: "old/pkg",
		NewModule: "new/pkg",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	content := string(got["main.go"])
	if !strings.Contains(content, "\"new/pkg\"") {
		t.Errorf("expected new/pkg, got:\n%s", content)
	}
	if strings.Contains(content, "\"old/pkg\"") {
		t.Errorf("old/pkg should be replaced, got:\n%s", content)
	}
}

func TestThesis_EditImport_ErrorOnMissing(t *testing.T) {
	state := State{
		"main.go": []byte("package main\n"),
	}
	op := &operation.EditImport{
		Path:      "main.go",
		OldModule: "ghost",
		NewModule: "spirit",
	}

	_, err := ApplyOp(state, op)
	if !errors.Is(err, ErrImportNotFound) {
		t.Errorf("expected ErrImportNotFound, got %v", err)
	}
}

// -----------------------------------------------------------------
// T22. HandlerCoverage — all OpKinds wired (updated for Phase 1.2)
// -----------------------------------------------------------------

func TestThesis_HandlerCoverage_AllKindsWired(t *testing.T) {
	for _, kind := range operation.AllOpKinds() {
		if _, ok := handlers[kind]; !ok {
			t.Errorf("OpKind %q is in operation.AllOpKinds() but missing from handlers map — wire a handler", kind)
		}
	}

	// Phase 1.2 assertion: all 17 OpKinds have concrete handlers.
	wantHandlers := 17
	if got := len(handlers); got != wantHandlers {
		t.Errorf("handlers count = %d, want %d", got, wantHandlers)
	}
}
