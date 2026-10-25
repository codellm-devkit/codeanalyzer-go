package dataflow

import (
	"go/ast"
	"go/token"
	"go/types"
	"runtime"
	"runtime/debug"
	"sync"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// L3Key names one callable of the symbol table: its declaring file (a
// symbol_table key; for a closure, its enclosing callable's file) and its
// signature (for a closure, "<outer>.closure_N"). It is the join key between
// Run's side table and the emitted tree — the emitter looks each callable up
// by (declaring file, signature), so a method declared apart from its
// receiver type still finds its result.
type L3Key struct{ Path, Signature string }

// Unit is the input for one callable. The caller (the symbol-table builder,
// which owns the AST walk) builds one per callable that has a body:
//
//   - Fn is the *ast.FuncDecl or *ast.FuncLit; a closure is its own unit.
//   - Info is the type info of Fn's package (nil degrades: no def/use).
//   - Calls is the callable's L1 call nodes: each L1 call-node key
//     (v2.CallBodyKeys over its call sites) with its site's CallExpr.Lparen.
//     BuildNodes pairs them with the body's calls by Lparen, so their order
//     does not matter.
type Unit struct {
	Key   L3Key
	Fn    ast.Node
	Info  *types.Info
	Calls []UnitCall
}

// UnitCall is one L1 call node of a unit: its key and the raw Lparen of the
// call expression it was recorded from.
type UnitCall struct {
	Key    string
	Lparen token.Pos
}

// Run builds the L3 overlay of every unit: the pruned body nodes, CFG, CDG
// and DDG (k = the access-path limit, --graph-field-depth). Units fan out
// over jobs workers (jobs < 1 means GOMAXPROCS); each worker writes only its
// own result slot and no id is minted here, so the map is the same for any
// jobs value and its iteration order never reaches the output.
//
// A unit that panics in any stage is skipped the same way (runOne).
//
// A unit with a body call that no L1 call key names (by Lparen) gets no
// result and a warning on stderr: a call node without its L1 key would be
// silently wrong, so its body stays at L1. A count mismatch between the L1
// keys and the body's recorded calls (RecordedCallCount) is a cross-check
// only: it warns, and the unit is still built when every call is paired.
func Run(fset *token.FileSet, units []Unit, k, jobs int) map[L3Key]Result {
	if jobs < 1 {
		jobs = runtime.GOMAXPROCS(0)
	}
	results := make([]*Result, len(units))
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < jobs; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				results[i] = runOne(fset, units[i], k)
			}
		}()
	}
	for i := range units {
		next <- i
	}
	close(next)
	wg.Wait()

	out := make(map[L3Key]Result, len(units))
	for i, r := range results {
		if r != nil {
			out[units[i].Key] = *r
		}
	}
	return out
}

// runOne builds one unit's result. A panic in any stage (a latent bug in the
// CFG, def-use or DDG builders) is recovered: that callable stays at L1 with a
// warning (and, at -vv, the panic's stack), and the rest of the run — and the
// L1/L2 output — is unaffected.
func runOne(fset *token.FileSet, u Unit, k int) (res *Result) {
	defer func() {
		if r := recover(); r != nil {
			utils.Warn("L3 skipped for %s (%s): %v", u.Key.Signature, u.Key.Path, r)
			utils.Debug("L3 panic in %s (%s):\n%s", u.Key.Signature, u.Key.Path, debug.Stack())
			res = nil
		}
	}()
	body := funcBody(u.Fn)
	if body == nil {
		return nil
	}
	recorded, missing := unpairedCalls(body, u.Calls)
	if recorded != len(u.Calls) {
		utils.Warn("L3 for %s (%s): %d L1 call keys for %d calls in the body", u.Key.Signature, u.Key.Path, len(u.Calls), recorded)
	}
	if missing > 0 {
		utils.Warn("L3 skipped for %s (%s): %d calls in the body have no L1 call key", u.Key.Signature, u.Key.Path, missing)
		return nil
	}
	nodes, keyOf := BuildNodes(fset, body, u.Calls)
	kept, cfg := BuildCFG(nodes, keyOf, body, u.Info)
	du := DefUses(kept, u.Info, k, EntryBases(u.Fn, u.Info), u.Fn, keyOf)
	return &Result{
		Nodes: kept,
		Cfg:   cfg,
		Cdg:   BuildCDG(kept, cfg),
		Ddg:   BuildDDG(kept, cfg, du),
	}
}

func funcBody(fn ast.Node) *ast.BlockStmt {
	switch fn := fn.(type) {
	case *ast.FuncDecl:
		return fn.Body
	case *ast.FuncLit:
		return fn.Body
	}
	return nil
}
