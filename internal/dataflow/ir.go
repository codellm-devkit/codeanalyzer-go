// Package dataflow builds the L3 intraprocedural graphs (CFG, CDG, DDG) of one
// callable body from go/ast. Its IR is keyed on the same local body keys the L1
// emitter writes ("l:c", "l:c/2", "@entry", "@exit"), so the L3 overlay joins
// the L1 call nodes on one string and never recomposes ids.
//
// The package must not import the v1 schema (internal/schema) or the v2 emitter
// (internal/syntactic_analysis/v2emit): the emitter imports this package.
package dataflow

import (
	"go/ast"
	"go/token"
)

// NodeKind is a body node's kind. Shared vocabulary only — Go adds no kinds
// (spec §4.2): `go` and `defer` are carried by the call node's flags.
type NodeKind string

const (
	KindCall      NodeKind = "call"
	KindStatement NodeKind = "statement"
	KindBranch    NodeKind = "branch"
	KindLoop      NodeKind = "loop"
	KindSwitch    NodeKind = "switch"
	KindReturn    NodeKind = "return"
	KindEntry     NodeKind = "entry"
	KindExit      NodeKind = "exit"
)

// Synthetic node keys. They start with "@", so v2.GlobalOrdinal concatenates
// them to the callable id as-is.
const (
	EntryKey = "@entry"
	ExitKey  = "@exit"
)

// Node is one body node of a callable.
//
//   - Key is the local key: "l:c" (v2.LocalID of the node's anchor position),
//     "l:c/2", … for co-positioned nodes, or EntryKey / ExitKey.
//   - Pos/End is the span of Stmt; NoPos for synthetic nodes. StartLC/EndLC
//     are their real [line, col] (fset.PositionFor(p, false): //line
//     directives are not applied, as they are not to the L1 call-site
//     positions); zero for synthetic nodes.
//   - Stmt is the AST node the body node stands for:
//     a statement (*ast.ExprStmt for a bare call, *ast.GoStmt for a `go` call),
//     a clause head (*ast.CaseClause / *ast.CommClause),
//     or an *ast.CallExpr for a satellite call node or the deferred call node.
//     nil for synthetic nodes.
//   - Parent is the innermost enclosing statement's key for a satellite call
//     node (a call nested in a statement, header or clause head; for a call in
//     a `defer`'s arguments, the defer statement). "" otherwise.
//   - Deferred marks the deferred call node of a `defer` statement (spec
//     §4.3.1): it runs only in the exit chain; its registration is the
//     separate `statement` node keyed at the `defer` keyword.
type Node struct {
	Key      string
	Kind     NodeKind
	Pos, End token.Pos
	StartLC  [2]int
	EndLC    [2]int
	Stmt     ast.Node
	Parent   string
	Deferred bool
}

// CfgEdge is a control-flow edge between two local keys. Kind is a shared CFG
// edge kind ("fallthrough", "true", "false", …) or the Go add "defer_resume".
type CfgEdge struct{ Src, Dst, Kind string }

// CdgEdge is a control-dependence edge between two local keys.
type CdgEdge struct{ Src, Dst string }

// DdgEdge is a data-dependence edge on access path Var. Its prov is always
// ["ssa"] at L3, so it is not stored.
type DdgEdge struct{ Src, Dst, Var string }

// Result is the L3 overlay of one callable body.
type Result struct {
	Nodes []Node
	Cfg   []CfgEdge
	Cdg   []CdgEdge
	Ddg   []DdgEdge
}
