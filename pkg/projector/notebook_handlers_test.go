// notebook_handlers_test.go — thesis-driven tests for Phase 1.2
// notebook cell projector handlers.
//
// # Thesis claims proven here
//
//	T23. AddCell inserts a cell at the specified index
//	T24. AddCell appends when CellIdx == len(cells)
//	T25. EditCell replaces cell source at CellRef.Index
//	T26. DeleteCell removes cell from the middle
//	T27. DeleteCell on single-cell notebook leaves empty cells array
//	T28. Notebook round-trip preserves metadata + nbformat fields
//	T29. AddCell with index > len(cells) returns ErrCellIndexOutOfBounds
//	T30. EditCell with invalid CellRef returns ErrCellIndexOutOfBounds
//	T31. HandlersComplete — every OpKind in AllOpKinds() is in handlers
package projector

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// testNotebook3Cells returns a 3-cell notebook JSON and the path it
// should be stored at in state. Used as the baseline for most tests.
func testNotebook3Cells() (string, []byte) {
	const path = "analysis.ipynb"
	nb := notebook{
		Cells: []notebookCell{
			{
				CellType: "markdown",
				Source:   []string{"# Introduction\n"},
				Metadata: map[string]any{},
			},
			{
				CellType: "code",
				Source:   []string{"import pandas as pd\n"},
				Metadata: map[string]any{},
				Outputs:  []any{},
			},
			{
				CellType: "code",
				Source:   []string{"df = pd.read_csv('data.csv')\n"},
				Metadata: map[string]any{},
				Outputs:  []any{},
			},
		},
		Metadata: map[string]any{
			"kernelspec": map[string]any{
				"display_name": "Python 3",
				"language":     "python",
				"name":         "python3",
			},
		},
		NBFormat:      4,
		NBFormatMinor: 2,
	}
	data, _ := json.MarshalIndent(nb, "", " ")
	data = append(data, '\n')
	return path, data
}

// parseNotebookFromState is a test helper that parses notebook JSON
// from state, failing the test on error.
func parseNotebookFromState(t *testing.T, state State, path string) *notebook {
	t.Helper()
	data, ok := state[path]
	if !ok {
		t.Fatalf("path %q not in state", path)
	}
	var nb notebook
	if err := json.Unmarshal(data, &nb); err != nil {
		t.Fatalf("parse notebook: %v", err)
	}
	return &nb
}

// -----------------------------------------------------------------
// T23. AddCell — inserts at specified index
// -----------------------------------------------------------------

func TestThesis_AddCell_InsertsAtIndex(t *testing.T) {
	path, data := testNotebook3Cells()
	state := State{path: data}

	op := &operation.AddCell{
		Notebook: path,
		CellIdx:  1,
		Kind_:    operation.CellKindCode,
		Source:   "x = 42",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	nb := parseNotebookFromState(t, got, path)
	if len(nb.Cells) != 4 {
		t.Fatalf("expected 4 cells, got %d", len(nb.Cells))
	}
	// The new cell should be at index 1.
	if nb.Cells[1].CellType != "code" {
		t.Errorf("cell[1].cell_type = %q, want %q", nb.Cells[1].CellType, "code")
	}
	if len(nb.Cells[1].Source) == 0 || nb.Cells[1].Source[0] != "x = 42" {
		t.Errorf("cell[1].source = %v, want [\"x = 42\"]", nb.Cells[1].Source)
	}
	// Original cell at index 1 should have shifted to index 2.
	if nb.Cells[2].CellType != "code" {
		t.Errorf("cell[2] should be the original code cell, got type %q", nb.Cells[2].CellType)
	}
}

// -----------------------------------------------------------------
// T24. AddCell — appends at end when CellIdx == len(cells)
// -----------------------------------------------------------------

func TestThesis_AddCell_AppendsAtEnd(t *testing.T) {
	path, data := testNotebook3Cells()
	state := State{path: data}

	op := &operation.AddCell{
		Notebook: path,
		CellIdx:  3, // == len(cells)
		Kind_:    operation.CellKindMarkdown,
		Source:   "## Conclusion",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	nb := parseNotebookFromState(t, got, path)
	if len(nb.Cells) != 4 {
		t.Fatalf("expected 4 cells, got %d", len(nb.Cells))
	}
	last := nb.Cells[3]
	if last.CellType != "markdown" {
		t.Errorf("last cell type = %q, want %q", last.CellType, "markdown")
	}
	if len(last.Source) == 0 || last.Source[0] != "## Conclusion" {
		t.Errorf("last cell source = %v, want [\"## Conclusion\"]", last.Source)
	}
}

// -----------------------------------------------------------------
// T25. EditCell — replaces source
// -----------------------------------------------------------------

func TestThesis_EditCell_ReplacesSource(t *testing.T) {
	path, data := testNotebook3Cells()
	state := State{path: data}

	op := &operation.EditCell{
		Notebook:  path,
		CellRef:   operation.CellRef{Index: 0},
		NewSource: "# Updated Title\nWith a second line",
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	nb := parseNotebookFromState(t, got, path)
	if len(nb.Cells) != 3 {
		t.Fatalf("expected 3 cells, got %d", len(nb.Cells))
	}
	// Source should be split into lines with \n on non-last lines.
	want := []string{"# Updated Title\n", "With a second line"}
	if len(nb.Cells[0].Source) != len(want) {
		t.Fatalf("cell[0].source len = %d, want %d: %v", len(nb.Cells[0].Source), len(want), nb.Cells[0].Source)
	}
	for i, w := range want {
		if nb.Cells[0].Source[i] != w {
			t.Errorf("cell[0].source[%d] = %q, want %q", i, nb.Cells[0].Source[i], w)
		}
	}
}

// -----------------------------------------------------------------
// T26. DeleteCell — removes from middle
// -----------------------------------------------------------------

func TestThesis_DeleteCell_RemovesFromMiddle(t *testing.T) {
	path, data := testNotebook3Cells()
	state := State{path: data}

	op := &operation.DeleteCell{
		Notebook: path,
		CellRef:  operation.CellRef{Index: 1},
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	nb := parseNotebookFromState(t, got, path)
	if len(nb.Cells) != 2 {
		t.Fatalf("expected 2 cells, got %d", len(nb.Cells))
	}
	// Cell 0 should still be markdown, cell 1 should be the former cell 2.
	if nb.Cells[0].CellType != "markdown" {
		t.Errorf("cell[0] type = %q, want markdown", nb.Cells[0].CellType)
	}
	if nb.Cells[1].CellType != "code" {
		t.Errorf("cell[1] type = %q, want code", nb.Cells[1].CellType)
	}
	if len(nb.Cells[1].Source) == 0 || nb.Cells[1].Source[0] != "df = pd.read_csv('data.csv')\n" {
		t.Errorf("cell[1] should be the original cell[2], got source %v", nb.Cells[1].Source)
	}
}

// -----------------------------------------------------------------
// T27. DeleteCell — single cell leaves empty array
// -----------------------------------------------------------------

func TestThesis_DeleteCell_LastCell_LeavesEmpty(t *testing.T) {
	const path = "single.ipynb"
	nb := notebook{
		Cells: []notebookCell{
			{
				CellType: "code",
				Source:   []string{"print('hello')\n"},
				Metadata: map[string]any{},
				Outputs:  []any{},
			},
		},
		NBFormat:      4,
		NBFormatMinor: 2,
	}
	data, _ := json.MarshalIndent(nb, "", " ")
	data = append(data, '\n')
	state := State{path: data}

	op := &operation.DeleteCell{
		Notebook: path,
		CellRef:  operation.CellRef{Index: 0},
	}

	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	result := parseNotebookFromState(t, got, path)
	if len(result.Cells) != 0 {
		t.Errorf("expected 0 cells, got %d", len(result.Cells))
	}
}

// -----------------------------------------------------------------
// T28. NotebookRoundTrip — preserves metadata + nbformat
// -----------------------------------------------------------------

func TestThesis_NotebookRoundTrip_PreservesMetadata(t *testing.T) {
	path, data := testNotebook3Cells()
	state := State{path: data}

	// Parse the original to capture metadata.
	var origNB notebook
	if err := json.Unmarshal(data, &origNB); err != nil {
		t.Fatalf("parse original: %v", err)
	}

	// Apply a benign edit (change cell 0 source).
	op := &operation.EditCell{
		Notebook:  path,
		CellRef:   operation.CellRef{Index: 0},
		NewSource: "# Changed",
	}
	got, err := ApplyOp(state, op)
	if err != nil {
		t.Fatalf("ApplyOp: %v", err)
	}

	result := parseNotebookFromState(t, got, path)

	// nbformat fields must survive.
	if result.NBFormat != origNB.NBFormat {
		t.Errorf("nbformat = %d, want %d", result.NBFormat, origNB.NBFormat)
	}
	if result.NBFormatMinor != origNB.NBFormatMinor {
		t.Errorf("nbformat_minor = %d, want %d", result.NBFormatMinor, origNB.NBFormatMinor)
	}

	// Top-level metadata must survive.
	ks, ok := result.Metadata["kernelspec"]
	if !ok {
		t.Fatal("metadata.kernelspec missing after round-trip")
	}
	ksMap, ok := ks.(map[string]any)
	if !ok {
		t.Fatalf("kernelspec type = %T, want map[string]any", ks)
	}
	if ksMap["display_name"] != "Python 3" {
		t.Errorf("kernelspec.display_name = %v, want %q", ksMap["display_name"], "Python 3")
	}
}

// -----------------------------------------------------------------
// T29. AddCell — index out of bounds errors
// -----------------------------------------------------------------

func TestThesis_AddCell_IndexOutOfBounds_Errors(t *testing.T) {
	path, data := testNotebook3Cells() // 3 cells
	state := State{path: data}

	cases := []struct {
		name string
		idx  int
	}{
		{"negative index", -1},
		{"past end", 4}, // len(cells) is 3, so 4 > 3
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := &operation.AddCell{
				Notebook: path,
				CellIdx:  c.idx,
				Kind_:    operation.CellKindCode,
				Source:   "x = 1",
			}
			_, err := ApplyOp(state, op)
			if !errors.Is(err, ErrCellIndexOutOfBounds) {
				t.Errorf("expected ErrCellIndexOutOfBounds, got %v", err)
			}
		})
	}
}

// -----------------------------------------------------------------
// T30. EditCell — invalid ref errors
// -----------------------------------------------------------------

func TestThesis_EditCell_InvalidRef_Errors(t *testing.T) {
	path, data := testNotebook3Cells() // 3 cells
	state := State{path: data}

	cases := []struct {
		name string
		idx  int
	}{
		{"past end", 3}, // cells are 0,1,2 — index 3 is OOB
		{"way past", 99},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := &operation.EditCell{
				Notebook:  path,
				CellRef:   operation.CellRef{Index: c.idx},
				NewSource: "new content",
			}
			_, err := ApplyOp(state, op)
			if !errors.Is(err, ErrCellIndexOutOfBounds) {
				t.Errorf("expected ErrCellIndexOutOfBounds, got %v", err)
			}
		})
	}
}

// -----------------------------------------------------------------
// T31. HandlersComplete — every OpKind in AllOpKinds is in handlers
// -----------------------------------------------------------------
//
// This is the Phase 1.2 completeness assertion: unsupportedOps is
// gone, every OpKind dispatches to a concrete handler.

func TestThesis_HandlersComplete_AllOpKindsWired(t *testing.T) {
	allKinds := operation.AllOpKinds()
	for _, kind := range allKinds {
		if _, ok := handlers[kind]; !ok {
			t.Errorf("OpKind %q missing from handlers — unsupportedOps should be empty", kind)
		}
	}
	if len(handlers) != len(allKinds) {
		t.Errorf("handlers has %d entries, AllOpKinds() has %d — mismatch", len(handlers), len(allKinds))
	}
}
