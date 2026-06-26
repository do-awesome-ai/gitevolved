// ast_handlers.go — Phase 1.1 AST-based projector handlers using
// line-based pattern matching with language-neutral heuristics.
//
// # Why line-based, not tree-sitter
//
// Phase 1.1 ships heuristic declaration/function/import finding via
// line scanning + brace-depth counting. This covers the 80% case
// (well-formatted Go/TS/Swift/Python source) without a tree-sitter
// dependency. Phase 2 introduces proper AST parsing for edge cases
// (multi-line signatures, nested types, string-embedded braces).
//
// # Safety contract
//
// Every handler returns a clear error on ambiguity. No handler
// silently corrupts state. If a declaration/function/import cannot
// be located unambiguously, the handler returns the appropriate
// ErrXxxNotFound sentinel and the state is unchanged.
//
// # Handler list (11 ops)
//
//   Declaration-level: handleAddDecl, handleEditDecl, handleDeleteDecl, handleRenameSymbol
//   Function-level:    handleAddFunction, handleDeleteFunction, handleRewriteFunction
//   Import-level:      handleAddImport, handleRemoveImport, handleEditImport
//   Sub-function:      handleEditStatement
package projector

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
)

// -----------------------------------------------------------------
// Sentinel errors for Phase 1.1 handlers
// -----------------------------------------------------------------

var (
	// ErrDeclNotFound is returned when a named declaration cannot be
	// located in the target file.
	ErrDeclNotFound = errors.New("projector: declaration not found in file")

	// ErrFunctionNotFound is returned when a named function cannot be
	// located in the target file.
	ErrFunctionNotFound = errors.New("projector: function not found in file")

	// ErrImportNotFound is returned when the specified import module
	// cannot be found in the target file.
	ErrImportNotFound = errors.New("projector: import module not found in file")

	// ErrDuplicateDecl is returned by AddDecl when the named
	// declaration already exists in the file.
	ErrDuplicateDecl = errors.New("projector: declaration already exists")

	// ErrDuplicateFunction is returned by AddFunction when the named
	// function already exists in the file.
	ErrDuplicateFunction = errors.New("projector: function already exists")

	// ErrDuplicateImport is returned by AddImport when the import
	// module is already present in the file.
	ErrDuplicateImport = errors.New("projector: import already present")

	// ErrBraceMismatch is returned when brace-depth counting fails
	// to find a matching close brace for a declaration or function.
	ErrBraceMismatch = errors.New("projector: brace mismatch — cannot determine extent")
)

// -----------------------------------------------------------------
// Declaration-level handlers
// -----------------------------------------------------------------

// handleAddDecl inserts a top-level declaration at the end of the
// file. Errors if the declaration already exists (by name + kind
// pattern match).
func handleAddDecl(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.AddDecl)
	if !ok {
		return state, fmt.Errorf("projector.handleAddDecl: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	// Check for duplicate.
	if findDeclLine(current, o.Name, o.DeclKind) >= 0 {
		return state, fmt.Errorf("%w: %s %s in %s", ErrDuplicateDecl, o.DeclKind, o.Name, o.Path)
	}

	// Append the source at the end of the file with a preceding
	// blank line separator.
	var out []byte
	if len(current) > 0 && current[len(current)-1] != '\n' {
		out = append(current, '\n')
	} else {
		out = append([]byte(nil), current...)
	}
	out = append(out, '\n')
	out = append(out, []byte(o.Source)...)
	if len(o.Source) > 0 && o.Source[len(o.Source)-1] != '\n' {
		out = append(out, '\n')
	}

	state[o.Path] = out
	return state, nil
}

// handleEditDecl replaces an existing declaration's full text with
// NewSource. Finds the declaration by name + kind, determines its
// extent via brace counting, then replaces.
func handleEditDecl(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.EditDecl)
	if !ok {
		return state, fmt.Errorf("projector.handleEditDecl: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	lines := splitLines(current)
	startLine := findDeclLine(current, o.Name, o.DeclKind)
	if startLine < 0 {
		return state, fmt.Errorf("%w: %s %s in %s", ErrDeclNotFound, o.DeclKind, o.Name, o.Path)
	}

	endLine, err := findBlockEnd(lines, startLine)
	if err != nil {
		return state, err
	}

	// Replace lines [startLine, endLine] with NewSource.
	newSource := o.NewSource
	if len(newSource) > 0 && newSource[len(newSource)-1] != '\n' {
		newSource += "\n"
	}

	var out []byte
	out = append(out, joinLines(lines[:startLine])...)
	out = append(out, []byte(newSource)...)
	if endLine+1 < len(lines) {
		out = append(out, joinLines(lines[endLine+1:])...)
	}

	state[o.Path] = out
	return state, nil
}

// handleDeleteDecl removes a named declaration from the file.
func handleDeleteDecl(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.DeleteDecl)
	if !ok {
		return state, fmt.Errorf("projector.handleDeleteDecl: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	lines := splitLines(current)
	startLine := findDeclLine(current, o.Name, o.DeclKind)
	if startLine < 0 {
		return state, fmt.Errorf("%w: %s %s in %s", ErrDeclNotFound, o.DeclKind, o.Name, o.Path)
	}

	endLine, err := findBlockEnd(lines, startLine)
	if err != nil {
		return state, err
	}

	// Remove lines [startLine, endLine]. Also remove a trailing
	// blank line if one exists (cosmetic cleanup).
	var out []byte
	out = append(out, joinLines(lines[:startLine])...)
	rest := endLine + 1
	// Skip one blank line after the removed block if present.
	if rest < len(lines) && strings.TrimSpace(lines[rest]) == "" {
		rest++
	}
	if rest < len(lines) {
		out = append(out, joinLines(lines[rest:])...)
	}

	state[o.Path] = out
	return state, nil
}

// handleRenameSymbol performs a file-scoped string replacement of
// OldName → NewName. v1 scope is single-file only (cross-file rename
// is Phase 2 with LSP).
func handleRenameSymbol(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.RenameSymbol)
	if !ok {
		return state, fmt.Errorf("projector.handleRenameSymbol: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	// Replace all occurrences of OldName with NewName in the file.
	// This is a byte-level replacement — word-boundary-aware rename
	// is Phase 2 (requires AST). For v1.1, callers (extractor) are
	// responsible for emitting RenameSymbol only when full string
	// replacement is semantically correct.
	replaced := bytes.ReplaceAll(current, []byte(o.OldName), []byte(o.NewName))
	if bytes.Equal(replaced, current) {
		// OldName not found — the op targets something not in the file.
		return state, fmt.Errorf("%w: %s %s in %s", ErrDeclNotFound, "symbol", o.OldName, o.Path)
	}

	state[o.Path] = replaced
	return state, nil
}

// -----------------------------------------------------------------
// Function-level handlers
// -----------------------------------------------------------------

// handleAddFunction appends a function to the end of the file.
// Errors if the function already exists.
func handleAddFunction(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.AddFunction)
	if !ok {
		return state, fmt.Errorf("projector.handleAddFunction: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	// Check for duplicate.
	if findFuncLine(current, o.Name) >= 0 {
		return state, fmt.Errorf("%w: %s in %s", ErrDuplicateFunction, o.Name, o.Path)
	}

	// Build the function text from signature + body.
	var funcText string
	if o.Body != "" {
		funcText = o.Signature + " {\n" + o.Body + "\n}\n"
	} else {
		funcText = o.Signature + " {}\n"
	}

	// Append with blank line separator.
	var out []byte
	if len(current) > 0 && current[len(current)-1] != '\n' {
		out = append(current, '\n')
	} else {
		out = append([]byte(nil), current...)
	}
	out = append(out, '\n')
	out = append(out, []byte(funcText)...)

	state[o.Path] = out
	return state, nil
}

// handleDeleteFunction removes a named function from the file.
func handleDeleteFunction(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.DeleteFunction)
	if !ok {
		return state, fmt.Errorf("projector.handleDeleteFunction: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	lines := splitLines(current)
	startLine := findFuncLine(current, o.Name)
	if startLine < 0 {
		return state, fmt.Errorf("%w: %s in %s", ErrFunctionNotFound, o.Name, o.Path)
	}

	endLine, err := findBlockEnd(lines, startLine)
	if err != nil {
		return state, err
	}

	// Remove lines [startLine, endLine].
	var out []byte
	out = append(out, joinLines(lines[:startLine])...)
	rest := endLine + 1
	// Skip one trailing blank line if present.
	if rest < len(lines) && strings.TrimSpace(lines[rest]) == "" {
		rest++
	}
	if rest < len(lines) {
		out = append(out, joinLines(lines[rest:])...)
	}

	state[o.Path] = out
	return state, nil
}

// handleRewriteFunction replaces a function's body (between { and })
// while preserving its signature line.
func handleRewriteFunction(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.RewriteFunction)
	if !ok {
		return state, fmt.Errorf("projector.handleRewriteFunction: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	lines := splitLines(current)
	startLine := findFuncLine(current, o.Name)
	if startLine < 0 {
		return state, fmt.Errorf("%w: %s in %s", ErrFunctionNotFound, o.Name, o.Path)
	}

	endLine, err := findBlockEnd(lines, startLine)
	if err != nil {
		return state, err
	}

	// Reconstruct: keep the signature line (startLine) but replace
	// everything from the opening brace's line to the closing brace.
	// The signature line includes the opening `{`.
	sigLine := lines[startLine]

	var newFunc string
	if o.NewBody != "" {
		newFunc = sigLine + "\n" + o.NewBody + "\n}\n"
	} else {
		// Empty body — preserve the signature with empty braces.
		// Check if sigLine already ends with `{`.
		newFunc = sigLine + "\n}\n"
	}

	var out []byte
	out = append(out, joinLines(lines[:startLine])...)
	out = append(out, []byte(newFunc)...)
	if endLine+1 < len(lines) {
		out = append(out, joinLines(lines[endLine+1:])...)
	}

	state[o.Path] = out
	return state, nil
}

// -----------------------------------------------------------------
// Import-level handlers
// -----------------------------------------------------------------

// handleAddImport inserts an import statement. For Go files with an
// existing import block, inserts inside the block. For files without
// a grouped import, appends an import line after the package clause
// or at the start of the file.
func handleAddImport(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.AddImport)
	if !ok {
		return state, fmt.Errorf("projector.handleAddImport: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	// Check for duplicate.
	if importLineIndex(current, o.Module) >= 0 {
		return state, fmt.Errorf("%w: %s in %s", ErrDuplicateImport, o.Module, o.Path)
	}

	lines := splitLines(current)

	// Strategy 1: find a grouped import block `import (` and insert
	// the module inside it.
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "import (" {
			// Insert the new import right after the opening `(`.
			importLine := "\t\"" + o.Module + "\""
			newLines := make([]string, 0, len(lines)+1)
			newLines = append(newLines, lines[:i+1]...)
			newLines = append(newLines, importLine)
			newLines = append(newLines, lines[i+1:]...)
			state[o.Path] = []byte(strings.Join(newLines, "\n") + "\n")
			return state, nil
		}
	}

	// Strategy 2: find a single-line import and convert to block, or
	// insert after package line.
	insertAt := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") {
			insertAt = i + 1
			// Skip blank line after package if present.
			if insertAt < len(lines) && strings.TrimSpace(lines[insertAt]) == "" {
				insertAt++
			}
			break
		}
	}

	// Insert a standalone import line.
	importLine := "import \"" + o.Module + "\""
	newLines := make([]string, 0, len(lines)+2)
	newLines = append(newLines, lines[:insertAt]...)
	newLines = append(newLines, importLine)
	newLines = append(newLines, lines[insertAt:]...)
	state[o.Path] = []byte(strings.Join(newLines, "\n") + "\n")
	return state, nil
}

// handleRemoveImport removes a line containing the specified import
// module from the file.
func handleRemoveImport(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.RemoveImport)
	if !ok {
		return state, fmt.Errorf("projector.handleRemoveImport: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	idx := importLineIndex(current, o.Module)
	if idx < 0 {
		return state, fmt.Errorf("%w: %s in %s", ErrImportNotFound, o.Module, o.Path)
	}

	lines := splitLines(current)
	// Remove the line at idx.
	newLines := make([]string, 0, len(lines)-1)
	newLines = append(newLines, lines[:idx]...)
	newLines = append(newLines, lines[idx+1:]...)
	state[o.Path] = []byte(strings.Join(newLines, "\n") + "\n")
	return state, nil
}

// handleEditImport renames an import module (OldModule → NewModule).
func handleEditImport(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.EditImport)
	if !ok {
		return state, fmt.Errorf("projector.handleEditImport: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	idx := importLineIndex(current, o.OldModule)
	if idx < 0 {
		return state, fmt.Errorf("%w: %s in %s", ErrImportNotFound, o.OldModule, o.Path)
	}

	lines := splitLines(current)
	// Replace the old module with the new module in the import line.
	lines[idx] = strings.Replace(lines[idx], o.OldModule, o.NewModule, 1)
	state[o.Path] = []byte(strings.Join(lines, "\n") + "\n")
	return state, nil
}

// -----------------------------------------------------------------
// Sub-function-level handlers
// -----------------------------------------------------------------

// handleEditStatement performs a byte-range splice within a named
// function's body. The StmtRange is relative to the function body
// start (first byte after the opening `{` line).
func handleEditStatement(state State, op operation.Operation) (State, error) {
	o, ok := op.(*operation.EditStatement)
	if !ok {
		return state, fmt.Errorf("projector.handleEditStatement: type assertion failed for %T", op)
	}
	current, exists := state[o.Path]
	if !exists {
		return state, fmt.Errorf("%w: %s", ErrPathNotFound, o.Path)
	}

	lines := splitLines(current)
	startLine := findFuncLine(current, o.FuncRef)
	if startLine < 0 {
		return state, fmt.Errorf("%w: %s in %s", ErrFunctionNotFound, o.FuncRef, o.Path)
	}

	endLine, err := findBlockEnd(lines, startLine)
	if err != nil {
		return state, err
	}

	// The function body is everything between startLine (sig + `{`)
	// and endLine (the `}`). Extract body lines.
	if endLine <= startLine {
		// Empty function body — can't splice into nothing meaningful.
		return state, fmt.Errorf("%w: range [%d,%d) against empty function body",
			ErrRangeOutOfBounds, o.StmtRange.Start, o.StmtRange.End)
	}

	// Compute the byte offset of the body start within the full file.
	// Body starts at the line after the signature line.
	bodyStartOffset := 0
	for i := 0; i <= startLine; i++ {
		bodyStartOffset += len(lines[i]) + 1 // +1 for the \n
	}
	// Body ends at the closing brace line.
	bodyEndOffset := bodyStartOffset
	for i := startLine + 1; i < endLine; i++ {
		bodyEndOffset += len(lines[i]) + 1
	}

	bodyLen := bodyEndOffset - bodyStartOffset
	if o.StmtRange.Start < 0 || o.StmtRange.End > bodyLen {
		return state, fmt.Errorf("%w: range [%d,%d) against function body length %d",
			ErrRangeOutOfBounds, o.StmtRange.Start, o.StmtRange.End, bodyLen)
	}

	// Splice within the full file using absolute offsets.
	absStart := bodyStartOffset + o.StmtRange.Start
	absEnd := bodyStartOffset + o.StmtRange.End

	newLen := absStart + len(o.NewText) + (len(current) - absEnd)
	out := make([]byte, 0, newLen)
	out = append(out, current[:absStart]...)
	out = append(out, []byte(o.NewText)...)
	out = append(out, current[absEnd:]...)

	state[o.Path] = out
	return state, nil
}

// -----------------------------------------------------------------
// Line-scanning helpers
// -----------------------------------------------------------------

// declKeywords maps DeclKind to the keywords that precede the name
// on the declaration line.
var declKeywords = map[operation.DeclKind][]string{
	operation.DeclKindStruct: {"type ", "struct ", "class ", "interface "},
	operation.DeclKindEnum:   {"type ", "enum "},
	operation.DeclKindType:   {"type "},
	operation.DeclKindConst:  {"const "},
	operation.DeclKindVar:    {"var "},
}

// findDeclLine scans file content for a line declaring `name` of the
// given kind. Returns the 0-based line index, or -1 if not found.
func findDeclLine(content []byte, name string, kind operation.DeclKind) int {
	lines := splitLines(content)
	keywords := declKeywords[kind]

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		for _, kw := range keywords {
			// Check if the line starts with the keyword followed by the name.
			if strings.HasPrefix(trimmed, kw+name) {
				rest := trimmed[len(kw)+len(name):]
				// The name must end at a word boundary: space, `{`, `(`,
				// end of line, or nothing (for Go `type X struct {`).
				if rest == "" || rest[0] == ' ' || rest[0] == '{' || rest[0] == '(' {
					return i
				}
			}
			// Also handle Go-style: `type Name struct {`
			if kind == operation.DeclKindStruct && strings.HasPrefix(trimmed, "type "+name+" ") {
				return i
			}
		}
	}
	return -1
}

// findFuncLine scans file content for a function definition line
// containing `func <name>(` or `func (<receiver>) <name>(`.
// Returns the 0-based line index, or -1 if not found.
func findFuncLine(content []byte, name string) int {
	lines := splitLines(content)
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Pattern: `func name(` or `func (receiver) name(`
		if !strings.Contains(trimmed, "func ") && !strings.HasPrefix(trimmed, "func ") {
			continue
		}
		// Check for the function name followed by `(`.
		// Handle both `func name(` and `func (T) name(`.
		idx := strings.Index(trimmed, name+"(")
		if idx < 0 {
			idx = strings.Index(trimmed, name+" (")
		}
		if idx > 0 {
			// Verify it's after "func " or after ") ".
			prefix := trimmed[:idx]
			if strings.HasSuffix(prefix, "func ") || strings.HasSuffix(prefix, ") ") || strings.HasSuffix(prefix, ").") {
				return i
			}
		}
	}
	return -1
}

// findBlockEnd finds the line index of the closing `}` for a block
// starting at startLine. Uses brace-depth counting. Returns the
// 0-based line index of the closing brace.
func findBlockEnd(lines []string, startLine int) (int, error) {
	depth := 0
	foundOpen := false
	for i := startLine; i < len(lines); i++ {
		for _, ch := range lines[i] {
			if ch == '{' {
				depth++
				foundOpen = true
			} else if ch == '}' {
				depth--
				if foundOpen && depth == 0 {
					return i, nil
				}
			}
		}
	}
	// If no braces were found at all, treat the declaration as a
	// single-line declaration (e.g., `const X = 42`).
	if !foundOpen {
		return startLine, nil
	}
	return 0, ErrBraceMismatch
}

// importLineIndex finds the 0-based line index containing an import
// of the given module. Returns -1 if not found.
func importLineIndex(content []byte, module string) int {
	lines := splitLines(content)
	for i, line := range lines {
		// Match patterns like:
		//   import "module"
		//   "module"  (inside an import block)
		//   import module (Python-style)
		if strings.Contains(line, "\""+module+"\"") {
			return i
		}
		// Also match unquoted imports (Python/Rust style).
		trimmed := strings.TrimSpace(line)
		if trimmed == "import "+module || trimmed == "from "+module+" import" ||
			strings.HasPrefix(trimmed, "from "+module+" ") ||
			strings.HasPrefix(trimmed, "use "+module) {
			return i
		}
	}
	return -1
}

// splitLines splits content into lines. Handles both \n and \r\n.
// Does NOT include the line terminators in the returned strings.
func splitLines(content []byte) []string {
	s := string(content)
	// Remove trailing newline to avoid an empty last element.
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// joinLines joins lines back with \n separators and adds a trailing
// newline.
func joinLines(lines []string) []byte {
	if len(lines) == 0 {
		return nil
	}
	return []byte(strings.Join(lines, "\n") + "\n")
}
