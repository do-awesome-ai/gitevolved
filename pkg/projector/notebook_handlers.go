// notebook_handlers.go — Phase 1.2 projector handlers for Jupyter
// notebook (.ipynb) cell operations: AddCell, EditCell, DeleteCell.
//
// # Why a separate file
//
// Notebook ops operate on structured JSON (the Jupyter v4 .ipynb
// format) rather than raw text or line-based patterns. The parse →
// modify → serialize cycle is distinct enough from the line-scanning
// handlers in ast_handlers.go to warrant its own file. Keeps each
// handler group scan-friendly.
//
// # Notebook JSON contract (Jupyter v4)
//
// State stores notebook content as the raw .ipynb JSON bytes. These
// handlers parse the JSON into the notebook struct, modify the cells
// array, and re-serialize. Metadata, nbformat, and nbformat_minor
// fields are preserved through the round-trip.
//
// The source field in Jupyter v4 cells is an array of strings (one
// per line, including trailing newlines). The operation's Source field
// is a single string; these handlers split it into the line-array
// form on write and join on read.
//
// # Error sentinels
//
// ErrNotNotebook — state content at the notebook path doesn't parse
// as valid Jupyter v4 JSON.
//
// ErrCellIndexOutOfBounds — CellIdx or CellRef.Index is negative or
// past the end of the cells array (for AddCell, past len(cells) is
// the append sentinel, so > len(cells) is the error boundary).
package projector

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// -----------------------------------------------------------------
// Sentinel errors for Phase 1.2 notebook handlers
// -----------------------------------------------------------------

var (
	// ErrNotNotebook is returned when state content at the notebook
	// path does not parse as valid Jupyter v4 notebook JSON.
	ErrNotNotebook = errors.New("projector: content is not a valid notebook")

	// ErrCellIndexOutOfBounds is returned when a cell index is
	// negative or past the valid range for the operation.
	ErrCellIndexOutOfBounds = errors.New("projector: cell index out of bounds")
)

// -----------------------------------------------------------------
// Notebook JSON types (Jupyter v4 minimal shape)
// -----------------------------------------------------------------

// notebookCell is the minimal JSON shape for a Jupyter v4 cell.
// Fields beyond the four modeled here (execution_count, id, etc.)
// survive the round-trip because they land in the Extras map via
// json.RawMessage — but v1.2 does not read or write them explicitly.
type notebookCell struct {
	CellType string         `json:"cell_type"`
	Source   []string       `json:"source"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Outputs  []any          `json:"outputs,omitempty"`
}

// notebook is the top-level Jupyter v4 .ipynb structure. Only the
// fields the projector reads/writes are modeled; the rest survive
// via the Metadata catch-all.
type notebook struct {
	Cells         []notebookCell `json:"cells"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	NBFormat      int            `json:"nbformat"`
	NBFormatMinor int            `json:"nbformat_minor"`
}

// -----------------------------------------------------------------
// Parse / serialize helpers
// -----------------------------------------------------------------

// parseNotebook deserializes state bytes into a notebook struct.
func parseNotebook(data []byte) (*notebook, error) {
	var nb notebook
	if err := json.Unmarshal(data, &nb); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotNotebook, err)
	}
	// Minimal structural validation: nbformat must be present.
	if nb.NBFormat == 0 {
		return nil, fmt.Errorf("%w: missing nbformat field", ErrNotNotebook)
	}
	return &nb, nil
}

// serializeNotebook re-serializes a notebook to indented JSON
// matching the standard .ipynb convention (1-space indent).
func serializeNotebook(nb *notebook) ([]byte, error) {
	data, err := json.MarshalIndent(nb, "", " ")
	if err != nil {
		return nil, fmt.Errorf("projector: notebook serialization failed: %w", err)
	}
	// .ipynb files conventionally end with a trailing newline.
	data = append(data, '\n')
	return data, nil
}

// sourceToLines splits a single source string into the Jupyter
// line-array format. Each line except the last gets a trailing \n.
func sourceToLines(source string) []string {
	if source == "" {
		return []string{}
	}
	raw := strings.Split(source, "\n")
	lines := make([]string, len(raw))
	for i, line := range raw {
		if i < len(raw)-1 {
			lines[i] = line + "\n"
		} else {
			lines[i] = line
		}
	}
	return lines
}

// -----------------------------------------------------------------
// Concrete handlers (Phase 1.2)
// -----------------------------------------------------------------

// handleAddCell inserts a new cell at CellIdx in the notebook's
// cells array. CellIdx == len(cells) is the append-at-end sentinel.
// CellIdx > len(cells) or < 0 returns ErrCellIndexOutOfBounds.
func handleAddCell(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.AddCell)
	if !ok {
		return state, fmt.Errorf("projector.handleAddCell: type assertion failed for %T", op)
	}
	current, exists := state[o.Notebook]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Notebook)
	}

	nb, err := parseNotebook(current)
	if err != nil {
		return state, err
	}

	if o.CellIdx < 0 || o.CellIdx > len(nb.Cells) {
		return state, fmt.Errorf("%w: index %d, notebook has %d cells",
			ErrCellIndexOutOfBounds, o.CellIdx, len(nb.Cells))
	}

	cell := notebookCell{
		CellType: string(o.Kind_),
		Source:   sourceToLines(o.Source),
		Metadata: map[string]any{},
	}
	// Code cells get an empty outputs array; markdown cells omit it.
	if o.Kind_ == operation.CellKindCode {
		cell.Outputs = []any{}
	}

	// Insert at CellIdx.
	nb.Cells = append(nb.Cells, notebookCell{}) // grow by 1
	copy(nb.Cells[o.CellIdx+1:], nb.Cells[o.CellIdx:])
	nb.Cells[o.CellIdx] = cell

	data, err := serializeNotebook(nb)
	if err != nil {
		return state, err
	}
	state[o.Notebook] = data
	return state, nil
}

// handleEditCell replaces the source of an existing cell at
// CellRef.Index. The cell type and metadata are preserved.
func handleEditCell(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.EditCell)
	if !ok {
		return state, fmt.Errorf("projector.handleEditCell: type assertion failed for %T", op)
	}
	current, exists := state[o.Notebook]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Notebook)
	}

	nb, err := parseNotebook(current)
	if err != nil {
		return state, err
	}

	idx := o.CellRef.Index
	if idx < 0 || idx >= len(nb.Cells) {
		return state, fmt.Errorf("%w: index %d, notebook has %d cells",
			ErrCellIndexOutOfBounds, idx, len(nb.Cells))
	}

	nb.Cells[idx].Source = sourceToLines(o.NewSource)

	data, err := serializeNotebook(nb)
	if err != nil {
		return state, err
	}
	state[o.Notebook] = data
	return state, nil
}

// handleDeleteCell removes the cell at CellRef.Index from the
// notebook's cells array.
func handleDeleteCell(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.DeleteCell)
	if !ok {
		return state, fmt.Errorf("projector.handleDeleteCell: type assertion failed for %T", op)
	}
	current, exists := state[o.Notebook]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Notebook)
	}

	nb, err := parseNotebook(current)
	if err != nil {
		return state, err
	}

	idx := o.CellRef.Index
	if idx < 0 || idx >= len(nb.Cells) {
		return state, fmt.Errorf("%w: index %d, notebook has %d cells",
			ErrCellIndexOutOfBounds, idx, len(nb.Cells))
	}

	// Remove the cell at idx.
	nb.Cells = append(nb.Cells[:idx], nb.Cells[idx+1:]...)

	data, err := serializeNotebook(nb)
	if err != nil {
		return state, err
	}
	state[o.Notebook] = data
	return state, nil
}
