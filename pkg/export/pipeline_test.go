// pipeline_test.go — integration tests exercising the full projector →
// export pipeline: typed operations → projected State → git commit.
//
// # Why this file exists
//
// The projector and exporter each have unit-level thesis tests
// (projector_test.go, git_test.go). This file proves they compose
// correctly end-to-end: a realistic sequence of typed operations is
// projected to file state, then exported as a git commit with
// structured trailers. The resulting commit is verified by reading
// the git tree back and byte-comparing against the projected state.
//
// # Thesis claims proven here
//
//	TP1. SingleSessionExport       project 3 ops → export → git tree matches
//	TP3. EmptyOps                  empty ops → empty state → --allow-empty commit
//	TP4. DeleteFile                AddFile + DeleteFile → export → file absent in HEAD
//
// TP2 (MultiSessionComposition) lives platform-side at
// internal/dosource/opdag/export_pipeline_integration_test.go — it composes the
// closed opdag causal-ordering with this open module, so it can't live in this
// import-closed package. That test doubles as the boundary-closeout proof that
// the closed platform builds + tests against the published open module.
package export

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// -----------------------------------------------------------------
// TP1. SingleSessionExport — project 3 ops → export → git tree matches
// -----------------------------------------------------------------
//
// Exercises the headline pipeline: AddFile creates a file, AddFunction
// appends a function to it, EditStatement splices within that function's
// body. The projected State is exported to git; the git tree is read
// back and byte-compared against the projection.
func TestThesis_Pipeline_SingleSessionExport(t *testing.T) {
	// Phase 1: build 3 ops forming a realistic single-session sequence.
	//
	// AddFile creates main.go with a package clause.
	// AddFunction appends func Hello() with a body containing
	//   `fmt.Println("hello")`.
	// EditStatement splices the function body to replace "hello" with
	//   "world" by targeting the byte range inside the body.
	addFile := &operation.AddFile{
		Path:    "main.go",
		Content: []byte("package main\n"),
	}
	addFunc := &operation.AddFunction{
		Path:      "main.go",
		Name:      "Hello",
		Signature: "func Hello()",
		Body:      "\tfmt.Println(\"hello\")",
		Language:  operation.LanguageGo,
	}

	// Project the first two ops to determine the body layout for
	// EditStatement's byte range. The projector materializes
	// AddFunction as: `func Hello() {\n\tfmt.Println("hello")\n}\n`
	// appended after a blank-line separator. Body starts after the
	// signature line + opening brace.
	intermediateState, err := projector.Project(projector.State{}, []operation.Operation{addFile, addFunc})
	if err != nil {
		t.Fatalf("intermediate projection: %v", err)
	}
	// Verify intermediate state has main.go (sanity check).
	if _, ok := intermediateState["main.go"]; !ok {
		t.Fatal("main.go missing from intermediate projection")
	}

	// Find the body content so we can compute the splice range.
	// The body (between { and }) contains: `\tfmt.Println("hello")`
	// EditStatement range is relative to the function body start.
	// The body is: `\tfmt.Println("hello")\n`
	// "hello" starts at byte 15 in the body (\tfmt.Println(" = 15 bytes).
	// "hello" is 5 bytes.
	bodyContent := "\tfmt.Println(\"hello\")\n"
	helloStart := strings.Index(bodyContent, "hello")
	if helloStart < 0 {
		t.Fatal("could not find 'hello' in expected body")
	}

	editStmt := &operation.EditStatement{
		Path:      "main.go",
		FuncRef:   "Hello",
		StmtRange: operation.Range{Start: helloStart, End: helloStart + 5},
		NewText:   "world",
	}

	// Phase 2: project all 3 ops from empty state.
	ops := []operation.Operation{addFile, addFunc, editStmt}
	state, err := projector.Project(projector.State{}, ops)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	// Verify the projected state contains the expected content.
	mainGo, ok := state["main.go"]
	if !ok {
		t.Fatal("main.go missing from projected state")
	}
	if !strings.Contains(string(mainGo), "world") {
		t.Errorf("projected main.go should contain 'world', got:\n%s", mainGo)
	}
	if strings.Contains(string(mainGo), "hello") {
		t.Errorf("projected main.go should NOT contain 'hello' (EditStatement should have replaced it), got:\n%s", mainGo)
	}

	// Phase 3: export to git.
	repo := initRepo(t)
	envs := sealOps(t, ops...)
	authorDate, committerDate := fixedDates()
	sha, err := ExportCommit(repo, state, envs, CommitOptions{
		Subject:       "dosource: single-session pipeline test",
		AuthorDate:    authorDate,
		CommitterDate: committerDate,
	})
	if err != nil {
		t.Fatalf("ExportCommit: %v", err)
	}

	// Phase 4: assertions.
	if sha == "" {
		t.Fatal("ExportCommit returned empty SHA")
	}

	// git log --oneline shows exactly 1 commit.
	logOut, err := exec.Command("git", "-C", repo, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logOut)), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 commit in log, got %d: %s", len(lines), logOut)
	}

	// git show HEAD:main.go matches projected state.
	showOut, err := exec.Command("git", "-C", repo, "show", "HEAD:main.go").Output()
	if err != nil {
		t.Fatalf("git show HEAD:main.go: %v", err)
	}
	if string(showOut) != string(mainGo) {
		t.Errorf("git tree content mismatch:\n  projected = %q\n  git show  = %q", mainGo, showOut)
	}

	// Commit message has structured trailers for all 3 ops.
	msgOut, err := exec.Command("git", "-C", repo, "log", "-1", "--format=%B", sha).Output()
	if err != nil {
		t.Fatalf("git log message: %v", err)
	}
	msg := string(msgOut)
	trailerCount := strings.Count(msg, "X-DoSource-Operation:")
	if trailerCount != 3 {
		t.Errorf("expected 3 X-DoSource-Operation trailers, got %d\n--- message ---\n%s", trailerCount, msg)
	}

	// Verify trailer content references the ops.
	for _, want := range []string{"AddFile(", "AddFunction(", "EditStatement("} {
		if !strings.Contains(msg, want) {
			t.Errorf("commit message missing trailer containing %q\n--- message ---\n%s", want, msg)
		}
	}
}

// -----------------------------------------------------------------
// TP3. EmptyOps — empty ops → empty state → commit exists (--allow-empty)
// -----------------------------------------------------------------
//
// An empty operation slice produces an empty State. ExportCommit uses
// --allow-empty, so the commit must still exist. This covers the
// "doSource attach" baseline-commit use case.
func TestThesis_Pipeline_EmptyOps(t *testing.T) {
	// Project with no ops from empty state.
	state, err := projector.Project(projector.State{}, nil)
	if err != nil {
		t.Fatalf("Project empty: %v", err)
	}
	if len(state) != 0 {
		t.Errorf("expected empty state, got %d entries", len(state))
	}

	// Export to git with empty state and no envelopes.
	repo := initRepo(t)
	authorDate, committerDate := fixedDates()
	sha, err := ExportCommit(repo, state, nil, CommitOptions{
		Subject:       "dosource: baseline (empty)",
		AuthorDate:    authorDate,
		CommitterDate: committerDate,
	})
	if err != nil {
		t.Fatalf("ExportCommit empty: %v", err)
	}
	if sha == "" {
		t.Fatal("ExportCommit returned empty SHA for empty commit")
	}

	// Commit exists.
	logOut, err := exec.Command("git", "-C", repo, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(logOut)), "\n")
	if len(lines) != 1 {
		t.Errorf("expected 1 commit, got %d", len(lines))
	}

	// No files in the tree.
	treeOut, err := exec.Command("git", "-C", repo, "ls-tree", "--name-only", sha).Output()
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	if strings.TrimSpace(string(treeOut)) != "" {
		t.Errorf("expected empty tree, got: %s", treeOut)
	}
}

// -----------------------------------------------------------------
// TP4. DeleteFile — AddFile + DeleteFile → file absent in HEAD
// -----------------------------------------------------------------
//
// Proves the full pipeline handles deletion: an AddFile followed by a
// DeleteFile for the same path produces a state with no files. After
// export, the git tree must not contain the file.
func TestThesis_Pipeline_DeleteFile(t *testing.T) {
	addOp := &operation.AddFile{
		Path:    "ephemeral.go",
		Content: []byte("package ephemeral\n"),
	}
	deleteOp := &operation.DeleteFile{
		Path: "ephemeral.go",
	}

	ops := []operation.Operation{addOp, deleteOp}
	state, err := projector.Project(projector.State{}, ops)
	if err != nil {
		t.Fatalf("Project: %v", err)
	}

	// Projected state must not contain the deleted file.
	if _, ok := state["ephemeral.go"]; ok {
		t.Fatal("ephemeral.go should not be in projected state after DeleteFile")
	}

	// Export to git.
	repo := initRepo(t)
	envs := sealOps(t, ops...)
	authorDate, committerDate := fixedDates()
	sha, err := ExportCommit(repo, state, envs, CommitOptions{
		Subject:       "dosource: delete pipeline test",
		AuthorDate:    authorDate,
		CommitterDate: committerDate,
	})
	if err != nil {
		t.Fatalf("ExportCommit: %v", err)
	}
	if sha == "" {
		t.Fatal("ExportCommit returned empty SHA")
	}

	// File must not be tracked in HEAD.
	treeOut, err := exec.Command("git", "-C", repo, "ls-tree", "--name-only", "-r", sha).Output()
	if err != nil {
		t.Fatalf("git ls-tree: %v", err)
	}
	if strings.Contains(string(treeOut), "ephemeral.go") {
		t.Errorf("ephemeral.go still tracked in HEAD after delete pipeline:\n%s", treeOut)
	}
}
