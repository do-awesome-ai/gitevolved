// types.go — supporting enum + reference types for the operation
// vocabulary.
//
// # Why a separate file
//
// Every concrete op in ops.go references one or more of these types
// (DeclKind, Language, CellKind, Range, ScopeRef, CellRef). Keeping
// them isolated makes the op definitions in ops.go scan-friendly:
// each op struct stays as a compact data record without inline type
// declarations interrupting the survey.
//
// # Stability
//
// These types are wire-format. Adding a new value to any of these
// enums is a schema-version bump (envelope.SchemaVersion → "v2"),
// not a backwards-compatible change — older readers MUST reject
// unknown values explicitly per the discriminator-safety contract
// in operation.go.
package operation

// DeclKind names the kind of top-level declaration an AddDecl /
// EditDecl / DeleteDecl operation targets. Covers struct / enum /
// type / const / var uniformly so the decl-level ops don't multiply
// per language.
type DeclKind string

const (
	// DeclKindStruct — Go `type X struct`, TS `class X` / `interface X`,
	// Swift `struct X` / `class X`, etc.
	DeclKindStruct DeclKind = "struct"
	// DeclKindEnum — Go `type X int` + iota constants, TS `enum X`,
	// Swift `enum X`, etc.
	DeclKindEnum DeclKind = "enum"
	// DeclKindType — Go `type X = Y`, TS `type X = Y`, generic alias.
	DeclKindType DeclKind = "type"
	// DeclKindConst — top-level constant.
	DeclKindConst DeclKind = "const"
	// DeclKindVar — top-level variable.
	DeclKindVar DeclKind = "var"
)

// IsValid reports whether dk is a known DeclKind. Used by the
// discriminator-safety check in op validation — unknown DeclKind in
// a deserialized op is a wire-format error, not a silent default.
func (dk DeclKind) IsValid() bool {
	switch dk {
	case DeclKindStruct, DeclKindEnum, DeclKindType, DeclKindConst, DeclKindVar:
		return true
	default:
		return false
	}
}

// Language names the source language a function-level op targets.
// Mirrors the 17 do-pack-supported languages plus a NotApplicable
// value for ops that don't care (file-level, region-level).
//
// Phase 1 v1 has full semantic support for Go, TypeScript / TSX /
// JavaScript, and Swift; the other 14 languages emit only file-level
// or RewriteRegion ops, so their Language values still parse but
// downstream consumers (projector, extractor) treat sub-function ops
// as out-of-scope.
type Language string

const (
	LanguageGo         Language = "go"
	LanguageTypeScript Language = "ts"
	LanguageTSX        Language = "tsx"
	LanguageJavaScript Language = "js"
	LanguageSwift      Language = "swift"
	LanguagePython     Language = "py"
	LanguageRust       Language = "rust"
	LanguageJava       Language = "java"
	LanguageKotlin     Language = "kotlin"
	LanguageC          Language = "c"
	LanguageCPP        Language = "cpp"
	LanguageCSharp     Language = "csharp"
	LanguageRuby       Language = "ruby"
	LanguagePHP        Language = "php"
	LanguageLua        Language = "lua"
	LanguageScala      Language = "scala"
	LanguageZig        Language = "zig"
	// LanguageNotApplicable — for ops that target a file regardless of
	// content (DeleteFile, RewriteRegion). Distinct from "" so the
	// IsValid check still catches accidental empty fields.
	LanguageNotApplicable Language = "n/a"
)

// IsValid reports whether l is a known Language.
func (l Language) IsValid() bool {
	switch l {
	case LanguageGo, LanguageTypeScript, LanguageTSX, LanguageJavaScript,
		LanguageSwift, LanguagePython, LanguageRust, LanguageJava,
		LanguageKotlin, LanguageC, LanguageCPP, LanguageCSharp,
		LanguageRuby, LanguagePHP, LanguageLua, LanguageScala,
		LanguageZig, LanguageNotApplicable:
		return true
	default:
		return false
	}
}

// CellKind discriminates the two cell types inside a Jupyter (or
// similar) notebook. Phase 1 treats the cell `Source` as an opaque
// string for both kinds; nested operations against the language
// inside a code cell are Phase 2.
type CellKind string

const (
	CellKindCode     CellKind = "code"
	CellKindMarkdown CellKind = "markdown"
)

// IsValid reports whether ck is a known CellKind.
func (ck CellKind) IsValid() bool {
	return ck == CellKindCode || ck == CellKindMarkdown
}

// Range names a half-open byte range [Start, End) inside a file or
// inside a function body. Used by EditStatement (statement range
// inside a function) and RewriteRegion (byte range inside a file).
//
// Start MUST be < End and both MUST be ≥ 0. Validation enforced by
// the owning op's Validate method.
type Range struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// IsValid reports whether r is a well-formed half-open range.
func (r Range) IsValid() bool {
	return r.Start >= 0 && r.End > r.Start
}

// ScopeRef qualifies a RenameSymbol target so symbols in different
// scopes (package, function, struct member) can share names without
// the rename clobbering the wrong one.
//
// v1 supports file-level rename only (Path field). Cross-file
// rename ("rename `getUser` across 14 callers") requires LSP
// resolution and is deferred to a later version.
type ScopeRef struct {
	// Path is the file the rename targets. v1: required, single-file
	// only. Phase 2 will add Package, Function, Member fields for
	// nested scopes.
	Path string `json:"path"`
}

// IsValid reports whether the scope is well-formed for v1
// (single-file scope, path non-empty).
func (s ScopeRef) IsValid() bool {
	return s.Path != ""
}

// CellRef identifies a notebook cell. v1 uses index; Phase 2 will
// add stable cell IDs once the projector understands notebook
// cell identity across re-orderings.
type CellRef struct {
	Index int `json:"index"`
}

// IsValid reports whether the cell ref is well-formed.
func (c CellRef) IsValid() bool {
	return c.Index >= 0
}
