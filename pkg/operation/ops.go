// ops.go — the 19 concrete Operation types that make up doSource's
// v1 operation vocabulary.
//
// # Grouping
//
// File-level → Decl-level → Function-level → Sub-function-level →
// Import-level → Notebook-level → Honest fallback. Within each
// group, ops appear in the order Add → Edit → Delete → Rename
// where applicable.
//
// # Pattern per op
//
// Every op is a struct with JSON tags + three methods:
//
//   - Kind() OpKind             returns the registered discriminator
//   - Validate() error          schema + semantic self-check
//   - canonicalBody() ([]byte, error)  deterministic JSON for op_id derivation
//
// Plus an init() in the same file group registering the factory.
//
// # Why no constructor helpers
//
// Callers build the struct literal directly. Adding NewAddFile()
// helpers would just be wrappers around struct construction and
// would require parallel maintenance whenever a field is added.
// The struct literal form is also what the extractor and the
// API handlers naturally produce from JSON, so keep that as the
// primary path.
package operation

import (
	"fmt"
)

// -----------------------------------------------------------------
// File-level ops
// -----------------------------------------------------------------

// AddFile records the creation of a file with initial content.
// Content is the full bytes; for very large files the extractor
// should split into a separate Add + sequence of RewriteRegion ops,
// but v1 does not enforce a size cap.
type AddFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

func (o *AddFile) Kind() OpKind { return OpKindAddFile }
func (o *AddFile) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: AddFile.Path", ErrEmptyField)
	}
	return nil
}
func (o *AddFile) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// DeleteFile records file removal. Content is intentionally absent
// — projection looks up the prior content from baseline.
type DeleteFile struct {
	Path string `json:"path"`
}

func (o *DeleteFile) Kind() OpKind { return OpKindDeleteFile }
func (o *DeleteFile) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: DeleteFile.Path", ErrEmptyField)
	}
	return nil
}
func (o *DeleteFile) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

func init() {
	register(OpKindAddFile, func() Operation { return &AddFile{} })
	register(OpKindDeleteFile, func() Operation { return &DeleteFile{} })
}

// -----------------------------------------------------------------
// Declaration-level ops (struct / enum / type / const / var)
// -----------------------------------------------------------------

// AddDecl records a new top-level declaration. Source is the full
// declaration text including any leading comments — projection
// inserts it at the appropriate place in the file, governed by the
// projector's per-language insertion policy.
type AddDecl struct {
	Path     string   `json:"path"`
	DeclKind DeclKind `json:"decl_kind"`
	Name     string   `json:"name"`
	Source   string   `json:"source"`
}

func (o *AddDecl) Kind() OpKind { return OpKindAddDecl }
func (o *AddDecl) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: AddDecl.Path", ErrEmptyField)
	}
	if o.Name == "" {
		return fmt.Errorf("%w: AddDecl.Name", ErrEmptyField)
	}
	if !o.DeclKind.IsValid() {
		return fmt.Errorf("%w: AddDecl.DeclKind=%q", ErrInvalidEnum, o.DeclKind)
	}
	return nil
}
func (o *AddDecl) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// EditDecl records a body change to an existing top-level declaration.
// The Name does NOT change — RenameSymbol is the op for that.
type EditDecl struct {
	Path      string   `json:"path"`
	DeclKind  DeclKind `json:"decl_kind"`
	Name      string   `json:"name"`
	NewSource string   `json:"new_source"`
}

func (o *EditDecl) Kind() OpKind { return OpKindEditDecl }
func (o *EditDecl) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: EditDecl.Path", ErrEmptyField)
	}
	if o.Name == "" {
		return fmt.Errorf("%w: EditDecl.Name", ErrEmptyField)
	}
	if !o.DeclKind.IsValid() {
		return fmt.Errorf("%w: EditDecl.DeclKind=%q", ErrInvalidEnum, o.DeclKind)
	}
	return nil
}
func (o *EditDecl) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// DeleteDecl records removal of a top-level declaration.
type DeleteDecl struct {
	Path     string   `json:"path"`
	DeclKind DeclKind `json:"decl_kind"`
	Name     string   `json:"name"`
}

func (o *DeleteDecl) Kind() OpKind { return OpKindDeleteDecl }
func (o *DeleteDecl) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: DeleteDecl.Path", ErrEmptyField)
	}
	if o.Name == "" {
		return fmt.Errorf("%w: DeleteDecl.Name", ErrEmptyField)
	}
	if !o.DeclKind.IsValid() {
		return fmt.Errorf("%w: DeleteDecl.DeclKind=%q", ErrInvalidEnum, o.DeclKind)
	}
	return nil
}
func (o *DeleteDecl) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// RenameSymbol records a same-scope rename. v1: Scope is file-level
// only (ScopeRef.Path required). Cross-file rename → Phase 2 with
// LSP integration. Callers within the same file that reference the
// renamed symbol are updated by the projector during materialization.
type RenameSymbol struct {
	Path    string   `json:"path"`
	OldName string   `json:"old_name"`
	NewName string   `json:"new_name"`
	Scope   ScopeRef `json:"scope"`
}

func (o *RenameSymbol) Kind() OpKind { return OpKindRenameSymbol }
func (o *RenameSymbol) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: RenameSymbol.Path", ErrEmptyField)
	}
	if o.OldName == "" {
		return fmt.Errorf("%w: RenameSymbol.OldName", ErrEmptyField)
	}
	if o.NewName == "" {
		return fmt.Errorf("%w: RenameSymbol.NewName", ErrEmptyField)
	}
	if o.OldName == o.NewName {
		return fmt.Errorf("operation: RenameSymbol OldName == NewName (%q) — no-op rename", o.OldName)
	}
	if !o.Scope.IsValid() {
		return fmt.Errorf("%w: RenameSymbol.Scope", ErrEmptyField)
	}
	return nil
}
func (o *RenameSymbol) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

func init() {
	register(OpKindAddDecl, func() Operation { return &AddDecl{} })
	register(OpKindEditDecl, func() Operation { return &EditDecl{} })
	register(OpKindDeleteDecl, func() Operation { return &DeleteDecl{} })
	register(OpKindRenameSymbol, func() Operation { return &RenameSymbol{} })
}

// -----------------------------------------------------------------
// Function-level ops
// -----------------------------------------------------------------

// AddFunction records a new function with full signature + body.
// Distinct from AddDecl because functions need a Language field
// for the projector to choose the right body-spec when materializing.
type AddFunction struct {
	Path      string   `json:"path"`
	Name      string   `json:"name"`
	Signature string   `json:"signature"`
	Body      string   `json:"body"`
	Language  Language `json:"language"`
}

func (o *AddFunction) Kind() OpKind { return OpKindAddFunction }
func (o *AddFunction) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: AddFunction.Path", ErrEmptyField)
	}
	if o.Name == "" {
		return fmt.Errorf("%w: AddFunction.Name", ErrEmptyField)
	}
	if o.Signature == "" {
		return fmt.Errorf("%w: AddFunction.Signature", ErrEmptyField)
	}
	if !o.Language.IsValid() {
		return fmt.Errorf("%w: AddFunction.Language=%q", ErrInvalidEnum, o.Language)
	}
	return nil
}
func (o *AddFunction) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// DeleteFunction records removal of a function. Same shape as
// DeleteDecl but kept distinct so the projector can apply
// function-specific cleanup (e.g., removing unused private helpers
// the function called).
type DeleteFunction struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

func (o *DeleteFunction) Kind() OpKind { return OpKindDeleteFunction }
func (o *DeleteFunction) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: DeleteFunction.Path", ErrEmptyField)
	}
	if o.Name == "" {
		return fmt.Errorf("%w: DeleteFunction.Name", ErrEmptyField)
	}
	return nil
}
func (o *DeleteFunction) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// RewriteFunction records a body change with signature preserved.
// Distinct from EditDecl{DeclKind: function} because v1 doesn't
// expose function via DeclKind — functions are first-class.
type RewriteFunction struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	NewBody string `json:"new_body"`
}

func (o *RewriteFunction) Kind() OpKind { return OpKindRewriteFunction }
func (o *RewriteFunction) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: RewriteFunction.Path", ErrEmptyField)
	}
	if o.Name == "" {
		return fmt.Errorf("%w: RewriteFunction.Name", ErrEmptyField)
	}
	return nil
}
func (o *RewriteFunction) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

func init() {
	register(OpKindAddFunction, func() Operation { return &AddFunction{} })
	register(OpKindDeleteFunction, func() Operation { return &DeleteFunction{} })
	register(OpKindRewriteFunction, func() Operation { return &RewriteFunction{} })
}

// -----------------------------------------------------------------
// Sub-function-level ops
// -----------------------------------------------------------------

// EditStatement records a byte-range edit inside a function body.
// FuncRef is the name of the containing function. Per the
// causal-dependency invariant locked 2026-05-14 (Q4 resolution via
// multi-LLM consult), an EditStatement is invalidated at projection
// time if its FuncRef has been renamed or deleted by an earlier op
// in the same projection cycle. The op's CausalParents field MUST
// include the most recent op_id that touched FuncRef, so the
// projector can detect the dependency cleanly.
type EditStatement struct {
	Path      string `json:"path"`
	FuncRef   string `json:"func_ref"`
	StmtRange Range  `json:"stmt_range"`
	NewText   string `json:"new_text"`
}

func (o *EditStatement) Kind() OpKind { return OpKindEditStatement }
func (o *EditStatement) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: EditStatement.Path", ErrEmptyField)
	}
	if o.FuncRef == "" {
		return fmt.Errorf("%w: EditStatement.FuncRef", ErrEmptyField)
	}
	if !o.StmtRange.IsValid() {
		return fmt.Errorf("%w: EditStatement.StmtRange=%+v", ErrInvalidRange, o.StmtRange)
	}
	return nil
}
func (o *EditStatement) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

func init() {
	register(OpKindEditStatement, func() Operation { return &EditStatement{} })
}

// -----------------------------------------------------------------
// Import-level ops
// -----------------------------------------------------------------

// AddImport records addition of an import / use / require statement.
// Module is the language-natural identifier — Go full path, JS/TS
// bare specifier or relative path, Python dotted module, etc.
type AddImport struct {
	Path   string `json:"path"`
	Module string `json:"module"`
}

func (o *AddImport) Kind() OpKind { return OpKindAddImport }
func (o *AddImport) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: AddImport.Path", ErrEmptyField)
	}
	if o.Module == "" {
		return fmt.Errorf("%w: AddImport.Module", ErrEmptyField)
	}
	return nil
}
func (o *AddImport) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// RemoveImport records removal of an import.
type RemoveImport struct {
	Path   string `json:"path"`
	Module string `json:"module"`
}

func (o *RemoveImport) Kind() OpKind { return OpKindRemoveImport }
func (o *RemoveImport) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: RemoveImport.Path", ErrEmptyField)
	}
	if o.Module == "" {
		return fmt.Errorf("%w: RemoveImport.Module", ErrEmptyField)
	}
	return nil
}
func (o *RemoveImport) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// EditImport records a module rename (e.g., upgraded major version,
// path change). OldModule MUST differ from NewModule.
type EditImport struct {
	Path      string `json:"path"`
	OldModule string `json:"old_module"`
	NewModule string `json:"new_module"`
}

func (o *EditImport) Kind() OpKind { return OpKindEditImport }
func (o *EditImport) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: EditImport.Path", ErrEmptyField)
	}
	if o.OldModule == "" {
		return fmt.Errorf("%w: EditImport.OldModule", ErrEmptyField)
	}
	if o.NewModule == "" {
		return fmt.Errorf("%w: EditImport.NewModule", ErrEmptyField)
	}
	if o.OldModule == o.NewModule {
		return fmt.Errorf("operation: EditImport OldModule == NewModule (%q) — no-op", o.OldModule)
	}
	return nil
}
func (o *EditImport) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

func init() {
	register(OpKindAddImport, func() Operation { return &AddImport{} })
	register(OpKindRemoveImport, func() Operation { return &RemoveImport{} })
	register(OpKindEditImport, func() Operation { return &EditImport{} })
}

// -----------------------------------------------------------------
// Notebook-level ops (Jupyter .ipynb and similar cell-structured)
// -----------------------------------------------------------------

// AddCell records a new cell in a notebook at CellIdx. Source is
// opaque string at v1 — nested ops on the language inside a code
// cell are Phase 2.
type AddCell struct {
	Notebook string   `json:"notebook"`
	CellIdx  int      `json:"cell_idx"`
	Kind_    CellKind `json:"kind"`
	Source   string   `json:"source"`
}

func (o *AddCell) Kind() OpKind { return OpKindAddCell }
func (o *AddCell) Validate() error {
	if o.Notebook == "" {
		return fmt.Errorf("%w: AddCell.Notebook", ErrEmptyField)
	}
	if o.CellIdx < 0 {
		return fmt.Errorf("%w: AddCell.CellIdx=%d", ErrInvalidRange, o.CellIdx)
	}
	if !o.Kind_.IsValid() {
		return fmt.Errorf("%w: AddCell.Kind=%q", ErrInvalidEnum, o.Kind_)
	}
	return nil
}
func (o *AddCell) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// EditCell records a source change to an existing cell.
type EditCell struct {
	Notebook  string  `json:"notebook"`
	CellRef   CellRef `json:"cell_ref"`
	NewSource string  `json:"new_source"`
}

func (o *EditCell) Kind() OpKind { return OpKindEditCell }
func (o *EditCell) Validate() error {
	if o.Notebook == "" {
		return fmt.Errorf("%w: EditCell.Notebook", ErrEmptyField)
	}
	if !o.CellRef.IsValid() {
		return fmt.Errorf("%w: EditCell.CellRef=%+v", ErrInvalidRange, o.CellRef)
	}
	return nil
}
func (o *EditCell) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

// DeleteCell records removal of a cell.
type DeleteCell struct {
	Notebook string  `json:"notebook"`
	CellRef  CellRef `json:"cell_ref"`
}

func (o *DeleteCell) Kind() OpKind { return OpKindDeleteCell }
func (o *DeleteCell) Validate() error {
	if o.Notebook == "" {
		return fmt.Errorf("%w: DeleteCell.Notebook", ErrEmptyField)
	}
	if !o.CellRef.IsValid() {
		return fmt.Errorf("%w: DeleteCell.CellRef=%+v", ErrInvalidRange, o.CellRef)
	}
	return nil
}
func (o *DeleteCell) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

func init() {
	register(OpKindAddCell, func() Operation { return &AddCell{} })
	register(OpKindEditCell, func() Operation { return &EditCell{} })
	register(OpKindDeleteCell, func() Operation { return &DeleteCell{} })
}

// -----------------------------------------------------------------
// Honest fallback (NOT a failure mode — first-class op)
// -----------------------------------------------------------------

// RewriteRegion records a byte-range replacement that the extractor
// could not decompose into a typed op. v1 fallback for: cross-decl
// edits, whitespace-only changes, opaque notebook JSON edits,
// language not yet supported with Tier-2 regex pack.
//
// Per the extraction-confidence policy (D5 in the locked design):
// when the extractor's candidate typed op fails the projector's
// equality check (project(baseline, op) ≠ observed_post), the
// extractor emits RewriteRegion + a diagnostic. RewriteRegion is
// the *honest* fallback, not a degradation signal.
type RewriteRegion struct {
	Path      string `json:"path"`
	ByteRange Range  `json:"byte_range"`
	Content   []byte `json:"content"`
}

func (o *RewriteRegion) Kind() OpKind { return OpKindRewriteRegion }
func (o *RewriteRegion) Validate() error {
	if o.Path == "" {
		return fmt.Errorf("%w: RewriteRegion.Path", ErrEmptyField)
	}
	if !o.ByteRange.IsValid() {
		return fmt.Errorf("%w: RewriteRegion.ByteRange=%+v", ErrInvalidRange, o.ByteRange)
	}
	return nil
}
func (o *RewriteRegion) canonicalBody() ([]byte, error) { return marshalCanonical(o) }

func init() {
	register(OpKindRewriteRegion, func() Operation { return &RewriteRegion{} })
}
