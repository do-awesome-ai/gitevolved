// ast_extract.go — Phase 1.1 AST-aware pattern matchers that attempt
// to extract typed operations from pre/post file pairs before falling
// back to the honest RewriteRegion fallback.
//
// # Why this exists
//
// v1.0 Extract() emits RewriteRegion for ALL edit-existing-file cases.
// Phase 1.1 upgrades that path: before emitting RewriteRegion, we try
// to identify higher-fidelity typed ops (AddFunction, DeleteFunction,
// RewriteFunction, AddImport, RemoveImport, RenameSymbol, AddDecl,
// DeleteDecl). These carry richer semantic intent in the op-log and
// enable more precise conflict detection downstream.
//
// # Safety invariant
//
// Every candidate typed op MUST round-trip through the projector:
//
//	project(pre_state, candidate_op) == post
//
// If the round-trip fails (including ErrUnsupportedOp from the
// projector), we fall back to RewriteRegion with zero corruption risk.
// The projector is the final arbiter; the extractor is best-effort.
//
// # Pattern-matching strategy
//
// Line-based heuristics, NOT full AST parsing. This keeps the
// extractor dependency-free (no tree-sitter, no go/parser import for
// non-Go languages). Each matcher scans for signature patterns:
//
//   - func <Name>( → function boundary
//   - import ( → import block
//   - type/const/var <Name> → declaration
//   - pure substitution diff → rename
//
// Ambiguity → nil return → RewriteRegion. We never guess.
package extract

import (
	"bytes"
	"strings"

	"github.com/do-awesome-ai/gitevolved/pkg/operation"
	"github.com/do-awesome-ai/gitevolved/pkg/projector"
)

// tryTypedExtraction attempts to extract a more specific typed op from
// the pre/post pair. Returns nil if no confident match — caller falls
// back to RewriteRegion.
//
// The function tries matchers in order from most specific to least:
//  1. RenameSymbol (pure substitution — most constrained)
//  2. AddFunction / DeleteFunction / RewriteFunction
//  3. AddImport / RemoveImport
//  4. AddDecl / DeleteDecl (broadest pattern)
//
// Each matcher returns (op, true) on confident match or (nil, false).
// On confident match, validateRoundTrip checks the candidate against
// the projector. If round-trip passes, the typed op is returned.
// Otherwise, nil (fall back to RewriteRegion).
func tryTypedExtraction(path string, pre, post []byte) operation.Operation {
	// Try each matcher in priority order.
	matchers := []func(string, []byte, []byte) (operation.Operation, bool){
		tryExtractRenameSymbol,
		tryExtractAddFunction,
		tryExtractDeleteFunction,
		tryExtractRewriteFunction,
		tryExtractAddImport,
		tryExtractRemoveImport,
		tryExtractAddDecl,
		tryExtractDeleteDecl,
	}

	for _, m := range matchers {
		if candidate, ok := m(path, pre, post); ok {
			if validateRoundTrip(path, pre, candidate, post) {
				return candidate
			}
			// Round-trip failed — this candidate is wrong. Try next
			// matcher (unlikely to succeed, but costs little).
		}
	}
	return nil
}

// validateRoundTrip checks that applying the candidate op to pre
// produces post. Uses the projector's ApplyOp for ground truth.
// Returns false on any error (including ErrUnsupportedOp — meaning
// the projector doesn't handle this op kind yet).
func validateRoundTrip(path string, pre []byte, candidate operation.Operation, expectedPost []byte) bool {
	state := projector.State{}
	if pre != nil {
		cp := make([]byte, len(pre))
		copy(cp, pre)
		state[path] = cp
	}
	result, err := projector.ApplyOp(state, candidate)
	if err != nil {
		// Includes ErrUnsupportedOp — projector can't handle yet.
		return false
	}
	got, exists := result[path]
	if !exists {
		return false
	}
	return bytes.Equal(got, expectedPost)
}

// =========================================================================
// Pattern matchers
// =========================================================================

// tryExtractAddFunction detects when post contains a new function
// that is entirely absent from pre. The function must:
//   - Have a signature line matching "func <Name>(" (Go-style)
//   - The entire function text (signature through closing brace) must
//     NOT appear in pre
//   - The rest of the file (pre minus nothing = post minus function)
//     must be unchanged except for the function insertion
//
// The projector's handleAddFunction builds output as:
//
//	Signature + " {\n" + Body + "\n}\n"
//
// So Signature is the declaration line WITHOUT the trailing ` {` and
// Body is the interior lines between `{` and `}` (not including braces).
func tryExtractAddFunction(path string, pre, post []byte) (operation.Operation, bool) {
	preLines := strings.Split(string(pre), "\n")
	postLines := strings.Split(string(post), "\n")

	// Find function signatures in post that are absent from pre.
	preFuncs := findFuncSignatures(preLines)
	postFuncs := findFuncSignatures(postLines)

	// Look for exactly one new function in post.
	var newFuncs []funcInfo
	for _, pf := range postFuncs {
		found := false
		for _, ef := range preFuncs {
			if ef.name == pf.name {
				found = true
				break
			}
		}
		if !found {
			newFuncs = append(newFuncs, pf)
		}
	}

	if len(newFuncs) != 1 {
		return nil, false
	}

	nf := newFuncs[0]
	// Extract the function body from post.
	endLine := findMatchingBrace(postLines, nf.lineIdx)
	if endLine < 0 {
		return nil, false
	}

	// Extract signature: the line up to (but not including) the trailing ` {`.
	sigLine := postLines[nf.lineIdx]
	signature := strings.TrimSpace(sigLine)
	// Strip trailing " {" from signature if present (projector adds it back).
	signature = strings.TrimSuffix(signature, " {")
	signature = strings.TrimSuffix(signature, "{")
	signature = strings.TrimSpace(signature)

	// Extract body: interior lines between signature line and closing brace.
	// Body does NOT include the `{` line or the `}` line.
	var body string
	if endLine > nf.lineIdx+1 {
		bodyLines := postLines[nf.lineIdx+1 : endLine]
		body = strings.Join(bodyLines, "\n")
	}

	// Verify that removing the function from post produces pre
	// (possibly modulo blank lines around the insertion point).
	withoutFunc := removeLines(postLines, nf.lineIdx, endLine)
	if !linesEqualTrimmed(preLines, withoutFunc) {
		return nil, false
	}

	lang := detectLanguage(path)
	op := &operation.AddFunction{
		Path:      path,
		Name:      nf.name,
		Signature: signature,
		Body:      body,
		Language:  lang,
	}
	if op.Validate() != nil {
		return nil, false
	}
	return op, true
}

// tryExtractDeleteFunction detects when pre contains a function that
// is entirely absent from post.
func tryExtractDeleteFunction(path string, pre, post []byte) (operation.Operation, bool) {
	preLines := strings.Split(string(pre), "\n")
	postLines := strings.Split(string(post), "\n")

	preFuncs := findFuncSignatures(preLines)
	postFuncs := findFuncSignatures(postLines)

	// Look for exactly one function in pre that's missing from post.
	var missingFuncs []funcInfo
	for _, ef := range preFuncs {
		found := false
		for _, pf := range postFuncs {
			if pf.name == ef.name {
				found = true
				break
			}
		}
		if !found {
			missingFuncs = append(missingFuncs, ef)
		}
	}

	if len(missingFuncs) != 1 {
		return nil, false
	}

	mf := missingFuncs[0]
	// Find the function extent in pre.
	endLine := findMatchingBrace(preLines, mf.lineIdx)
	if endLine < 0 {
		return nil, false
	}

	// Verify that removing the function from pre produces post.
	withoutFunc := removeLines(preLines, mf.lineIdx, endLine)
	if !linesEqualTrimmed(postLines, withoutFunc) {
		return nil, false
	}

	op := &operation.DeleteFunction{
		Path: path,
		Name: mf.name,
	}
	if op.Validate() != nil {
		return nil, false
	}
	return op, true
}

// tryExtractRewriteFunction detects when the same function signature
// exists in both pre and post but the body differs, and the rest of
// the file is unchanged.
func tryExtractRewriteFunction(path string, pre, post []byte) (operation.Operation, bool) {
	preLines := strings.Split(string(pre), "\n")
	postLines := strings.Split(string(post), "\n")

	preFuncs := findFuncSignatures(preLines)
	postFuncs := findFuncSignatures(postLines)

	// Find functions present in both with matching names.
	type funcPair struct {
		name     string
		preLine  int
		postLine int
	}
	var shared []funcPair
	for _, ef := range preFuncs {
		for _, pf := range postFuncs {
			if pf.name == ef.name {
				shared = append(shared, funcPair{
					name:     ef.name,
					preLine:  ef.lineIdx,
					postLine: pf.lineIdx,
				})
				break
			}
		}
	}

	// We need exactly one function that changed, and the rest of the
	// file must be identical outside that function.
	var changedPair *funcPair
	for i := range shared {
		fp := &shared[i]
		preEnd := findMatchingBrace(preLines, fp.preLine)
		postEnd := findMatchingBrace(postLines, fp.postLine)
		if preEnd < 0 || postEnd < 0 {
			continue
		}

		preFuncText := strings.Join(preLines[fp.preLine:preEnd+1], "\n")
		postFuncText := strings.Join(postLines[fp.postLine:postEnd+1], "\n")

		if preFuncText != postFuncText {
			if changedPair != nil {
				// Multiple functions changed — ambiguous.
				return nil, false
			}
			changedPair = fp
		}
	}

	if changedPair == nil {
		return nil, false
	}

	// Verify that replacing the old function body with the new one
	// accounts for the entire diff.
	preEnd := findMatchingBrace(preLines, changedPair.preLine)
	postEnd := findMatchingBrace(postLines, changedPair.postLine)
	if preEnd < 0 || postEnd < 0 {
		return nil, false
	}

	// Check: lines before the function and after it must be identical.
	preBeforeFunc := preLines[:changedPair.preLine]
	postBeforeFunc := postLines[:changedPair.postLine]
	preAfterFunc := preLines[preEnd+1:]
	postAfterFunc := postLines[postEnd+1:]

	if !linesEqual(preBeforeFunc, postBeforeFunc) || !linesEqual(preAfterFunc, postAfterFunc) {
		return nil, false
	}

	// Extract the new function body: interior lines between the opening
	// `{` (on the signature line) and the closing `}`.
	// The projector reconstructs as: sigLine + "\n" + NewBody + "\n}\n"
	var newBody string
	if postEnd > changedPair.postLine+1 {
		bodyLines := postLines[changedPair.postLine+1 : postEnd]
		newBody = strings.Join(bodyLines, "\n")
	}

	op := &operation.RewriteFunction{
		Path:    path,
		Name:    changedPair.name,
		NewBody: newBody,
	}
	if op.Validate() != nil {
		return nil, false
	}
	return op, true
}

// tryExtractAddImport detects when post's import block has exactly
// one new module line that pre doesn't have, and the rest of the file
// is unchanged.
func tryExtractAddImport(path string, pre, post []byte) (operation.Operation, bool) {
	preImports := extractImports(string(pre))
	postImports := extractImports(string(post))

	// Find new imports in post.
	var added []string
	for _, pi := range postImports {
		found := false
		for _, ei := range preImports {
			if pi == ei {
				found = true
				break
			}
		}
		if !found {
			added = append(added, pi)
		}
	}

	if len(added) != 1 {
		return nil, false
	}

	// Verify nothing else changed: remove the added import from post
	// and check if it equals pre.
	postWithoutImport := removeImportFromSource(string(post), added[0])
	if postWithoutImport != string(pre) {
		return nil, false
	}

	op := &operation.AddImport{
		Path:   path,
		Module: added[0],
	}
	if op.Validate() != nil {
		return nil, false
	}
	return op, true
}

// tryExtractRemoveImport detects when pre's import block has exactly
// one module line that post doesn't have, and the rest of the file
// is unchanged.
func tryExtractRemoveImport(path string, pre, post []byte) (operation.Operation, bool) {
	preImports := extractImports(string(pre))
	postImports := extractImports(string(post))

	// Find removed imports (in pre but not post).
	var removed []string
	for _, ei := range preImports {
		found := false
		for _, pi := range postImports {
			if ei == pi {
				found = true
				break
			}
		}
		if !found {
			removed = append(removed, ei)
		}
	}

	if len(removed) != 1 {
		return nil, false
	}

	// Verify nothing else changed: remove the import from pre
	// and check if it equals post.
	preWithoutImport := removeImportFromSource(string(pre), removed[0])
	if preWithoutImport != string(post) {
		return nil, false
	}

	op := &operation.RemoveImport{
		Path:   path,
		Module: removed[0],
	}
	if op.Validate() != nil {
		return nil, false
	}
	return op, true
}

// tryExtractRenameSymbol detects when the entire diff between pre and
// post is explainable as a pure string substitution s/oldName/newName/
// throughout the file.
//
// Algorithm:
//  1. Find the first diff position (longest common prefix).
//  2. Expand backward to find the start of the identifier token.
//  3. Extract the full identifier word at that position in pre (oldName
//     candidate) and the corresponding word in post (newName candidate).
//  4. Validate: strings.ReplaceAll(pre, oldName, newName) == post.
//  5. Both must be valid identifiers.
func tryExtractRenameSymbol(path string, pre, post []byte) (operation.Operation, bool) {
	if len(pre) == 0 || len(post) == 0 {
		return nil, false
	}

	preStr := string(pre)
	postStr := string(post)

	// Find the first byte position where they differ.
	prefixLen := commonPrefixLen(pre, post)
	if prefixLen == len(pre) || prefixLen == len(post) {
		return nil, false // one is a prefix of the other — not a rename
	}

	// Expand backward from the diff start to find the beginning of the
	// identifier token in pre.
	tokenStart := prefixLen
	for tokenStart > 0 && isIdentChar(pre[tokenStart-1]) {
		tokenStart--
	}

	// Extract the full word at this position in pre (from tokenStart to
	// end of identifier chars).
	tokenEnd := tokenStart
	for tokenEnd < len(pre) && isIdentChar(pre[tokenEnd]) {
		tokenEnd++
	}

	oldName := preStr[tokenStart:tokenEnd]
	if oldName == "" || !isIdentifier(oldName) {
		return nil, false
	}

	// Extract the corresponding word in post at the same start position.
	// (The prefix up to tokenStart is identical, so post[tokenStart] is
	// where the new identifier starts.)
	postTokenEnd := tokenStart
	for postTokenEnd < len(post) && isIdentChar(post[postTokenEnd]) {
		postTokenEnd++
	}

	newName := postStr[tokenStart:postTokenEnd]
	if newName == "" || !isIdentifier(newName) || oldName == newName {
		return nil, false
	}

	// The definitive check: replacing ALL occurrences of oldName with
	// newName in pre must produce post exactly.
	replaced := strings.ReplaceAll(preStr, oldName, newName)
	if replaced != postStr {
		return nil, false
	}

	op := &operation.RenameSymbol{
		Path:    path,
		OldName: oldName,
		NewName: newName,
		Scope:   operation.ScopeRef{Path: path},
	}
	if op.Validate() != nil {
		return nil, false
	}
	return op, true
}

// isIdentChar reports whether b is a valid identifier character.
func isIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// tryExtractAddDecl detects when post has a new top-level type/const/var
// declaration that pre lacks, and the rest of the file is unchanged.
func tryExtractAddDecl(path string, pre, post []byte) (operation.Operation, bool) {
	preDecls := findDeclarations(string(pre))
	postDecls := findDeclarations(string(post))

	// Find new declarations.
	var added []declInfo
	for _, pd := range postDecls {
		found := false
		for _, ed := range preDecls {
			if pd.name == ed.name && pd.kind == ed.kind {
				found = true
				break
			}
		}
		if !found {
			added = append(added, pd)
		}
	}

	if len(added) != 1 {
		return nil, false
	}

	ad := added[0]
	postLines := strings.Split(string(post), "\n")

	// Find the extent of the declaration in post.
	endLine := findDeclEnd(postLines, ad.lineIdx)
	if endLine < 0 {
		return nil, false
	}

	declText := strings.Join(postLines[ad.lineIdx:endLine+1], "\n")

	// Verify that removing the declaration from post produces pre.
	withoutDecl := removeLines(postLines, ad.lineIdx, endLine)
	preLines := strings.Split(string(pre), "\n")
	if !linesEqualTrimmed(preLines, withoutDecl) {
		return nil, false
	}

	op := &operation.AddDecl{
		Path:     path,
		DeclKind: ad.kind,
		Name:     ad.name,
		Source:   declText,
	}
	if op.Validate() != nil {
		return nil, false
	}
	return op, true
}

// tryExtractDeleteDecl detects when pre has a top-level declaration
// that post lacks, and the rest of the file is unchanged.
func tryExtractDeleteDecl(path string, pre, post []byte) (operation.Operation, bool) {
	preDecls := findDeclarations(string(pre))
	postDecls := findDeclarations(string(post))

	// Find removed declarations.
	var removed []declInfo
	for _, ed := range preDecls {
		found := false
		for _, pd := range postDecls {
			if ed.name == pd.name && ed.kind == pd.kind {
				found = true
				break
			}
		}
		if !found {
			removed = append(removed, ed)
		}
	}

	if len(removed) != 1 {
		return nil, false
	}

	rd := removed[0]
	preLines := strings.Split(string(pre), "\n")

	// Find the extent of the declaration in pre.
	endLine := findDeclEnd(preLines, rd.lineIdx)
	if endLine < 0 {
		return nil, false
	}

	// Verify that removing the declaration from pre produces post.
	withoutDecl := removeLines(preLines, rd.lineIdx, endLine)
	postLines := strings.Split(string(post), "\n")
	if !linesEqualTrimmed(postLines, withoutDecl) {
		return nil, false
	}

	op := &operation.DeleteDecl{
		Path:     path,
		DeclKind: rd.kind,
		Name:     rd.name,
	}
	if op.Validate() != nil {
		return nil, false
	}
	return op, true
}

// =========================================================================
// Helpers
// =========================================================================

// funcInfo captures a detected function signature.
type funcInfo struct {
	name      string
	signature string
	lineIdx   int
}

// findFuncSignatures scans lines for Go-style function signatures.
// Returns all lines matching "func <Name>(..." pattern.
func findFuncSignatures(lines []string) []funcInfo {
	var result []funcInfo
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "func ") {
			continue
		}
		// Extract function name: "func Name(" or "func (r Recv) Name("
		rest := trimmed[5:] // after "func "
		name := ""
		if strings.HasPrefix(rest, "(") {
			// Method receiver: skip to closing ")" then space then name.
			closeIdx := strings.Index(rest, ")")
			if closeIdx < 0 || closeIdx+2 >= len(rest) {
				continue
			}
			rest = strings.TrimSpace(rest[closeIdx+1:])
		}
		// Now rest starts with the function name.
		parenIdx := strings.Index(rest, "(")
		if parenIdx <= 0 {
			continue
		}
		name = strings.TrimSpace(rest[:parenIdx])
		if name == "" || !isIdentifier(name) {
			continue
		}
		result = append(result, funcInfo{
			name:      name,
			signature: trimmed,
			lineIdx:   i,
		})
	}
	return result
}

// findMatchingBrace finds the line index of the closing brace that
// matches the opening brace on or after startLine. Returns -1 if
// no match found.
func findMatchingBrace(lines []string, startLine int) int {
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
					return i
				}
			}
		}
	}
	return -1
}

// removeLines removes lines[start..end] (inclusive) and trims
// adjacent blank lines at the removal point.
func removeLines(lines []string, start, end int) []string {
	result := make([]string, 0, len(lines)-(end-start+1))
	result = append(result, lines[:start]...)
	result = append(result, lines[end+1:]...)
	// Remove one adjacent blank line at the seam if it exists.
	if start < len(result) && start > 0 {
		if strings.TrimSpace(result[start]) == "" && strings.TrimSpace(result[start-1]) == "" {
			result = append(result[:start], result[start+1:]...)
		}
	}
	return result
}

// linesEqual checks if two line slices are identical.
func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// linesEqualTrimmed compares two line slices, trimming trailing empty
// lines from both before comparison.
func linesEqualTrimmed(a, b []string) bool {
	a = trimTrailingEmpty(a)
	b = trimTrailingEmpty(b)
	return linesEqual(a, b)
}

// trimTrailingEmpty removes trailing empty (whitespace-only) lines.
func trimTrailingEmpty(lines []string) []string {
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// extractImports extracts module strings from a Go-style import block.
// Handles both single-line `import "mod"` and multi-line `import (...)`.
func extractImports(src string) []string {
	var imports []string
	lines := strings.Split(src, "\n")
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "import (") {
			inBlock = true
			continue
		}
		if inBlock {
			if trimmed == ")" {
				inBlock = false
				continue
			}
			mod := extractModuleName(trimmed)
			if mod != "" {
				imports = append(imports, mod)
			}
			continue
		}
		if strings.HasPrefix(trimmed, "import ") {
			rest := strings.TrimPrefix(trimmed, "import ")
			mod := extractModuleName(strings.TrimSpace(rest))
			if mod != "" {
				imports = append(imports, mod)
			}
		}
	}
	return imports
}

// extractModuleName extracts the module path from a quoted import line.
// Handles optional alias: `alias "path"` or just `"path"`.
func extractModuleName(s string) string {
	// Find the quoted string.
	start := strings.Index(s, "\"")
	if start < 0 {
		return ""
	}
	end := strings.Index(s[start+1:], "\"")
	if end < 0 {
		return ""
	}
	return s[start+1 : start+1+end]
}

// removeImportFromSource removes a single import line from the source.
// Returns the modified source string.
func removeImportFromSource(src, module string) string {
	lines := strings.Split(src, "\n")
	quotedModule := "\"" + module + "\""
	var result []string
	inBlock := false
	removed := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "import (") {
			inBlock = true
			result = append(result, line)
			continue
		}
		if inBlock {
			if trimmed == ")" {
				inBlock = false
				result = append(result, line)
				continue
			}
			if !removed && strings.Contains(line, quotedModule) {
				removed = true
				continue // skip this line
			}
			result = append(result, line)
			continue
		}
		if !removed && strings.HasPrefix(trimmed, "import ") && strings.Contains(line, quotedModule) {
			removed = true
			continue // skip single-line import
		}
		result = append(result, line)
	}
	return strings.Join(result, "\n")
}

// isIdentifier reports whether s looks like a source-code identifier:
// non-empty, starts with letter or underscore, contains only letters,
// digits, underscores.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i, ch := range s {
		if i == 0 {
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_') {
				return false
			}
		} else {
			if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_') {
				return false
			}
		}
	}
	return true
}

// declInfo captures a detected top-level declaration.
type declInfo struct {
	kind    operation.DeclKind
	name    string
	lineIdx int
}

// findDeclarations scans for Go-style type/const/var declarations.
func findDeclarations(src string) []declInfo {
	var result []declInfo
	lines := strings.Split(src, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		kind, name := parseDeclLine(trimmed)
		if kind != "" && name != "" {
			result = append(result, declInfo{kind: kind, name: name, lineIdx: i})
		}
	}
	return result
}

// parseDeclLine parses a line to see if it starts a top-level
// type/const/var declaration. Returns ("", "") if not.
func parseDeclLine(line string) (operation.DeclKind, string) {
	prefixes := []struct {
		prefix string
		kind   operation.DeclKind
	}{
		{"type ", operation.DeclKindType},
		{"const ", operation.DeclKindConst},
		{"var ", operation.DeclKindVar},
	}
	for _, p := range prefixes {
		if strings.HasPrefix(line, p.prefix) {
			rest := strings.TrimSpace(line[len(p.prefix):])
			// Extract the name (first word).
			name := ""
			for _, ch := range rest {
				if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' {
					name += string(ch)
				} else {
					break
				}
			}
			if name == "" {
				continue
			}
			// Disambiguate struct vs type alias.
			if p.kind == operation.DeclKindType {
				if strings.Contains(rest, " struct") {
					return operation.DeclKindStruct, name
				}
				if strings.Contains(rest, " enum") {
					return operation.DeclKindEnum, name
				}
			}
			return p.kind, name
		}
	}
	return "", ""
}

// findDeclEnd finds the end line of a declaration starting at startLine.
// For single-line declarations, returns startLine. For multi-line
// (brace-delimited), finds the matching closing brace.
func findDeclEnd(lines []string, startLine int) int {
	if startLine >= len(lines) {
		return -1
	}
	// Check if this declaration has braces.
	if strings.Contains(lines[startLine], "{") {
		return findMatchingBrace(lines, startLine)
	}
	// Single-line declaration (or parenthesized group).
	if strings.Contains(lines[startLine], "(") {
		// Parenthesized const/var group.
		depth := 0
		for i := startLine; i < len(lines); i++ {
			for _, ch := range lines[i] {
				if ch == '(' {
					depth++
				} else if ch == ')' {
					depth--
					if depth == 0 {
						return i
					}
				}
			}
		}
		return -1
	}
	// Simple single-line declaration.
	return startLine
}

// detectLanguage infers the Language enum from a file path extension.
func detectLanguage(path string) operation.Language {
	switch {
	case strings.HasSuffix(path, ".go"):
		return operation.LanguageGo
	case strings.HasSuffix(path, ".ts"):
		return operation.LanguageTypeScript
	case strings.HasSuffix(path, ".tsx"):
		return operation.LanguageTSX
	case strings.HasSuffix(path, ".js"):
		return operation.LanguageJavaScript
	case strings.HasSuffix(path, ".swift"):
		return operation.LanguageSwift
	case strings.HasSuffix(path, ".py"):
		return operation.LanguagePython
	case strings.HasSuffix(path, ".rs"):
		return operation.LanguageRust
	case strings.HasSuffix(path, ".java"):
		return operation.LanguageJava
	case strings.HasSuffix(path, ".kt"):
		return operation.LanguageKotlin
	case strings.HasSuffix(path, ".c"):
		return operation.LanguageC
	case strings.HasSuffix(path, ".cpp"), strings.HasSuffix(path, ".cc"):
		return operation.LanguageCPP
	case strings.HasSuffix(path, ".cs"):
		return operation.LanguageCSharp
	case strings.HasSuffix(path, ".rb"):
		return operation.LanguageRuby
	case strings.HasSuffix(path, ".php"):
		return operation.LanguagePHP
	case strings.HasSuffix(path, ".lua"):
		return operation.LanguageLua
	case strings.HasSuffix(path, ".scala"):
		return operation.LanguageScala
	case strings.HasSuffix(path, ".zig"):
		return operation.LanguageZig
	default:
		return operation.LanguageNotApplicable
	}
}
