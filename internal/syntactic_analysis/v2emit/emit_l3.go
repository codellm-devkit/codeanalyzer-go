package v2emit

import (
	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// l3Overlay merges dataflow.Run's side table into the emitted tree. on is
// maxLevel >= 3; below that the overlay is inert and the tree is exactly the
// L1/L2 one (call nodes with their ids, no parent, no edge lists).
type l3Overlay struct {
	on  bool
	res map[dataflow.L3Key]dataflow.Result
}

func newL3Overlay(maxLevel int, res map[dataflow.L3Key]dataflow.Result) l3Overlay {
	return l3Overlay{on: maxLevel >= 3, res: res}
}

// apply completes c's body from the result keyed by key (the callable's
// declaring file and signature) and copies its edges. li is the declaring
// file's lineIndex, so every span indexes the file c's own span indexes.
//
// The merge is additive over the L1 body:
//   - every L1 call node is kept unchanged — kind, id, span (the CallExpr's,
//     R7), callee, flags — even one the CFG pruned as unreachable (it simply
//     has no edges); a result node on an L1 key only contributes its Parent;
//   - every other result node (the pruned set's statement, control and
//     synthetic nodes) is added with its span from Pos/End and its ID =
//     v2.GlobalOrdinal(c.ID, key), the rule emitBody applies to the L1 nodes
//     (R15), so the id field is uniform at every level.
//
// Run works on local keys only; ids are minted here and in emitBody.
func (o l3Overlay) apply(c *v2.Callable, li *lineIndex, key dataflow.L3Key) {
	if !o.on {
		return
	}
	if r, ok := o.res[key]; ok {
		for _, n := range r.Nodes {
			if bn, ok := c.Body[n.Key]; ok {
				bn.Parent = n.Parent
				c.Body[n.Key] = bn
				continue
			}
			bn := l3BodyNode(n, li)
			bn.ID = v2.GlobalOrdinal(c.ID, n.Key)
			c.Body[n.Key] = bn
		}
		c.Cfg = make([]v2.CfgEdge, len(r.Cfg))
		for i, e := range r.Cfg {
			c.Cfg[i] = v2.CfgEdge{Src: e.Src, Dst: e.Dst, Kind: e.Kind}
		}
		c.Cdg = make([]v2.CdgEdge, len(r.Cdg))
		for i, e := range r.Cdg {
			c.Cdg[i] = v2.CdgEdge{Src: e.Src, Dst: e.Dst}
		}
		c.Ddg = make([]v2.DdgEdge, len(r.Ddg))
		for i, e := range r.Ddg {
			c.Ddg[i] = v2.DdgEdge{Src: e.Src, Dst: e.Dst, Var: e.Var, Prov: []string{"ssa"}} // own slice per edge: no aliasing
		}
	}
}

// l3BodyNode is a non-L1 body node: its kind, and its span from the node's
// [line, col] range (zero for @entry/@exit). A call node reaching here is a
// call L1 did not key — impossible when Run's count check passed — and keeps
// the call shape with a null callee.
func l3BodyNode(n dataflow.Node, li *lineIndex) v2.BodyNode {
	bn := v2.BodyNode{Kind: string(n.Kind), Parent: n.Parent}
	if n.Kind != dataflow.KindEntry && n.Kind != dataflow.KindExit {
		bn.Span = colSpan(li, n.StartLC[0], n.StartLC[1], n.EndLC[0], n.EndLC[1])
	}
	return bn
}
