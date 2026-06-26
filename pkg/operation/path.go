// path.go — PrimaryPath: the canonical op → target-file-path accessor.
//
// Why this exists: multiple layers need "what file does this op target" — the
// staging fold (legacy Event.Path), and the /add server-boundary path validator
// (api.Add, which must reject control-char paths in typed ops the same way it
// does for legacy adds). Keeping the op-type switch in ONE place (here, the
// operation package that owns the vocabulary) prevents the parallel-systems
// drift that two copies would invite. staging.operationPath delegates to this.
package operation

// PrimaryPath returns the primary file-path target of an op, or "" for ops with
// no single file target. Notebook ops report their notebook path. Kept in sync
// with the op vocabulary by living beside it.
func PrimaryPath(op Operation) string {
	switch o := op.(type) {
	case *AddFile:
		return o.Path
	case *DeleteFile:
		return o.Path
	case *AddDecl:
		return o.Path
	case *EditDecl:
		return o.Path
	case *DeleteDecl:
		return o.Path
	case *RenameSymbol:
		return o.Path
	case *AddFunction:
		return o.Path
	case *DeleteFunction:
		return o.Path
	case *RewriteFunction:
		return o.Path
	case *EditStatement:
		return o.Path
	case *AddImport:
		return o.Path
	case *RemoveImport:
		return o.Path
	case *EditImport:
		return o.Path
	case *AddCell:
		return o.Notebook
	case *EditCell:
		return o.Notebook
	case *DeleteCell:
		return o.Notebook
	case *RewriteRegion:
		return o.Path
	}
	return ""
}
