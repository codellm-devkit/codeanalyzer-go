package syntactic_analysis

import (
	"go/ast"
	"go/types"
	"sort"

	"github.com/codellm-devkit/codeanalyzer-go/internal/astwalk"
	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// L3Units builds the dataflow.Run input for symbolTable (the table Build
// returned): one unit per emitted callable with a body, closures included,
// sorted by key.
//
//   - A FuncDecl is matched to its symbol-table callable by (declaring file,
//     signature) — the same pair the emitter looks results up by — and its
//     start line. The line check keeps a second `func init()` in one file,
//     which the symbol table overwrote, from taking the survivor's key.
//     Declarations the symbol table dropped (cgo wrappers, methods whose
//     receiver type was not found) get no unit.
//   - A FuncLit is matched to its L1 closure by name: buildInnerCallables
//     names the outermost literals of a body with astwalk.Literals and
//     recurses per literal, so walking the same way pairs each literal with
//     InnerCallables[name]. Its start line is checked too. Its key's Path is
//     the enclosing callable's declaring file.
//   - Calls is v2.CallBodyKeys over the callable's L1 call sites, the exact
//     keys the L1 emitter writes, each with its site's Lparen.
//
// A mismatch (which would mean the walk drifted from L1's) is logged and the
// callable is left at L1.
func (b *SymbolTableBuilder) L3Units(symbolTable map[string]schema.GoFile) []dataflow.Unit {
	byKey := map[dataflow.L3Key]schema.GoCallable{}
	for _, f := range symbolTable {
		for _, c := range f.Functions {
			byKey[dataflow.L3Key{Path: c.Path, Signature: c.Signature}] = c
		}
		for _, t := range f.Types {
			for _, c := range t.Methods {
				if c.Path != "" { // interface methods have no declaration
					byKey[dataflow.L3Key{Path: c.Path, Signature: c.Signature}] = c
				}
			}
		}
	}

	paths := make([]string, 0, len(b.pkgs))
	for p := range b.pkgs {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var units []dataflow.Unit
	seen := map[dataflow.L3Key]bool{}
	for _, pp := range paths {
		pkg := b.pkgs[pp]
		for _, file := range pkg.Syntax {
			for _, d := range file.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				tf := b.fset.File(fd.Pos())
				if tf == nil || !utils.IsWithin(b.projectDir, tf.Name()) {
					continue
				}
				key := dataflow.L3Key{
					Path:      utils.RelativePath(b.projectDir, tf.Name()),
					Signature: b.declSignature(pkg, fd),
				}
				c, ok := byKey[key]
				if !ok || seen[key] || c.StartLine != b.position(fd.Pos()).Line {
					continue
				}
				seen[key] = true
				units = append(units, unit(key, fd, pkg.TypesInfo, c))
				units = b.closureUnits(units, key.Path, fd.Body, c, pkg.TypesInfo)
			}
		}
	}
	sort.Slice(units, func(i, j int) bool {
		a, b := units[i].Key, units[j].Key
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Signature < b.Signature
	})
	return units
}

// closureUnits appends a unit for every function literal of body, walking
// astwalk.Literals as buildInnerCallables does.
func (b *SymbolTableBuilder) closureUnits(units []dataflow.Unit, path string, body *ast.BlockStmt, outer schema.GoCallable, info *types.Info) []dataflow.Unit {
	for _, l := range astwalk.Literals(body) {
		ic, ok := outer.InnerCallables[l.Name]
		if !ok || ic.StartLine != b.position(l.Lit.Pos()).Line {
			utils.Warn("L3 skipped for %s.%s (%s): no matching L1 closure", outer.Signature, l.Name, path)
			continue
		}
		units = append(units, unit(dataflow.L3Key{Path: path, Signature: ic.Signature}, l.Lit, info, ic))
		units = b.closureUnits(units, path, l.Lit.Body, ic, info)
	}
	return units
}

func unit(key dataflow.L3Key, fn ast.Node, info *types.Info, c schema.GoCallable) dataflow.Unit {
	pos := make([][2]int, len(c.CallSites))
	for i, cs := range c.CallSites {
		pos[i] = [2]int{cs.StartLine, cs.StartColumn}
	}
	keys := v2.CallBodyKeys(pos)
	calls := make([]dataflow.UnitCall, len(c.CallSites))
	for i, cs := range c.CallSites {
		calls[i] = dataflow.UnitCall{Key: keys[i], Lparen: cs.Lparen}
	}
	return dataflow.Unit{Key: key, Fn: fn, Info: info, Calls: calls}
}
