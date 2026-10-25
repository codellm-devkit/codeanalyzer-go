package dataflow

import (
	"go/ast"
	"go/types"
)

// CanPanic exposes canPanic to the external test package.
var CanPanic = canPanic

// PostDominators exposes the immediate post-dominator map to the external
// test package.
var PostDominators = postDominators

// DefUsesPreD1 is DefUses without the callable-level D1 rules (capture
// writes, deferred-operand writes, append's Shallow def): the L3 def/use sets
// before #22, for the gate that the D1 rules only add ddg edges.
func DefUsesPreD1(nodes []Node, info *types.Info, k int, entryBases []types.Object, fn ast.Node, keyOf map[ast.Node]string) map[string]DefUse {
	return defUses(nodes, info, k, entryBases, fn, keyOf, nil)
}
