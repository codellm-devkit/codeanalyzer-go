package dataflow

import (
	"go/ast"
	"go/token"
	"go/types"
	"sort"
)

// CFG edge kinds (spec §4.3), shared vocabulary. The Go add EdgeDeferResume
// is in cfg_exits.go, with the defer chain.
const (
	EdgeFallthrough = "fallthrough"
	EdgeTrue        = "true"
	EdgeFalse       = "false"
	EdgeLoopBack    = "loop_back"
	EdgeSwitchCase  = "switch_case"
	EdgeBreak       = "break"
	EdgeContinue    = "continue"
	EdgeReturn      = "return"
	EdgeException   = "exception"
)

// BuildCFG links the body nodes of one callable into its statement-level CFG
// (spec §4.3) and returns the kept nodes with the edges. nodes and keyOf are
// BuildNodes' output for body; every edge endpoint is a key from keyOf,
// EntryKey or ExitKey. info resolves the builtin panic and the types canPanic
// needs; it may be nil (panic by name, every call / selector can panic).
//
// The returned nodes are nodes minus the pruned ones, in nodes' order: a
// statement / header / clause-head node unreachable from @entry (code after a
// return / panic / goto, the implicit empty statement of `L: }`), a deferred
// call whose defer statement is unreachable, and every satellite call node of
// a pruned statement. Callers (the L3 emitter) must emit body nodes from this
// set, not from BuildNodes' output. A nil body yields empty, non-nil slices.
//
// The linker works backwards like Java's CfgBuilder: link(s, next, kind)
// wires statement s so its normal completion reaches next, and returns the
// key control enters s at. A frame stack resolves break / continue /
// fallthrough; goto edges are added once every label's entry is known.
// finishExits then adds panic edges and the defer chain, prunes, and repairs
// exit reachability (see there).
//
// Structured edge rules:
//   - @entry → first statement: fallthrough (return when the body is empty).
//   - straight-line: fallthrough. The last statement of a body falls to
//     @exit with kind return; the last statement of a loop body goes back to
//     the header with kind loop_back. Branching constructs keep their own
//     kind on those terminal edges (an else-less if's false, a break, …).
//   - if: header → then entry true; → else entry (or next) false.
//   - for cond / 3-clause, for range: header → body entry true, → next false,
//     body tail → header loop_back. Init and post fold into the header.
//   - for {}: header → body entry fallthrough, no false edge, plus the
//     synthetic header → @exit exception (restores post-dominance).
//   - a loop body with no node: header → header loop_back instead of the
//     body entry edge.
//   - switch / type switch: header → each clause head switch_case; without a
//     default, also header → next switch_case. Clause head → its first body
//     statement fallthrough (an empty clause falls to next). A fallthrough
//     statement → the next clause's first body statement, or that clause's
//     continuation when its body is empty, kind fallthrough.
//   - select: header → each comm clause head switch_case, never → next.
//     select {} has only the synthetic header → @exit exception.
//   - break / continue (labeled or not) → resolved target, kind break /
//     continue. continue targets the loop header (a for post folds into it).
//   - goto L → L's entry, kind fallthrough. A label on a statement with no
//     node of its own (a block, the implicit empty statement before `}`)
//     resolves to the first node inside it, or to its continuation (ruling R6).
//   - return → @exit, kind return. Every other non-exception edge into
//     @exit (fall-off after an else-less if, a loop's false edge, a goto to
//     a label at the end, …) is normalized to kind return too, so the defer
//     chain sees only return / exception edges into @exit.
//
// Satellite call nodes are never linked. Deferred call nodes are linked only
// in the exit chain; their defer statements are on the straight-line path.
//
// Edges are deduplicated and sorted by (Src, Dst, Kind).
func BuildCFG(nodes []Node, keyOf map[ast.Node]string, body *ast.BlockStmt, info *types.Info) ([]Node, []CfgEdge) {
	if body == nil {
		return []Node{}, []CfgEdge{}
	}
	l := &linker{keyOf: keyOf, edges: map[CfgEdge]bool{}, labels: map[string]string{}}
	first := l.seq(body.List, ExitKey, EdgeReturn)
	if first == ExitKey {
		l.edge(EntryKey, ExitKey, EdgeReturn)
	} else {
		l.edge(EntryKey, first, EdgeFallthrough)
	}
	for _, g := range l.gotos {
		if dst, ok := l.labels[g.label]; ok {
			l.edge(g.src, dst, EdgeFallthrough)
		}
	}
	return finishExits(l, nodes, body, info)
}

// frame is one enclosing break / continue / fallthrough scope.
type frame struct {
	label string // "" when unlabeled
	brk   string // break target (the construct's continuation)
	cont  string // continue target (loop header); "" for switch / select
	fall  string // fallthrough target of the clause being linked (switch)
}

type pendingGoto struct{ src, label string }

type linker struct {
	keyOf  map[ast.Node]string
	edges  map[CfgEdge]bool
	frames []frame
	labels map[string]string // label → entry key of its statement
	gotos  []pendingGoto
}

// edge records src → dst. Every non-exception edge into @exit is a function
// exit (a return, or falling off the end of the body however the last
// construct completes), so it takes kind return.
func (l *linker) edge(src, dst, kind string) {
	if dst == ExitKey && kind != EdgeException {
		kind = EdgeReturn
	}
	if src != "" && dst != "" {
		l.edges[CfgEdge{Src: src, Dst: dst, Kind: kind}] = true
	}
}

func (l *linker) sorted() []CfgEdge {
	out := make([]CfgEdge, 0, len(l.edges))
	for e := range l.edges {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		if a.Dst != b.Dst {
			return a.Dst < b.Dst
		}
		return a.Kind < b.Kind
	})
	return out
}

// seq links a statement list: each statement falls through to the next, the
// last one reaches next with kind. It returns the list's entry (next when the
// list has no node).
func (l *linker) seq(list []ast.Stmt, next, kind string) string {
	for i := len(list) - 1; i >= 0; i-- {
		next = l.link(list[i], next, kind, "")
		kind = EdgeFallthrough
	}
	return next
}

// link wires s so its normal completion reaches next with kind, and returns
// the key control enters s at. label is the label naming s, if any.
func (l *linker) link(s ast.Stmt, next, kind, label string) string {
	switch s := s.(type) {
	case *ast.BlockStmt:
		return l.seq(s.List, next, kind)

	case *ast.LabeledStmt:
		entry := next // R6: implicit `L: }` resolves to the continuation
		if e, ok := s.Stmt.(*ast.EmptyStmt); !ok || !e.Implicit {
			entry = l.link(s.Stmt, next, kind, s.Label.Name)
		}
		l.labels[s.Label.Name] = entry
		return entry

	case *ast.IfStmt:
		h := l.keyOf[s]
		l.edge(h, l.seq(s.Body.List, next, kind), EdgeTrue)
		if s.Else != nil {
			l.edge(h, l.link(s.Else, next, kind, ""), EdgeFalse)
		} else {
			l.edge(h, next, EdgeFalse)
		}
		return h

	case *ast.ForStmt:
		return l.loop(s, s.Body, s.Cond == nil, next, label)

	case *ast.RangeStmt:
		return l.loop(s, s.Body, false, next, label)

	case *ast.SwitchStmt:
		return l.clauses(s, s.Body, true, next, kind, label)

	case *ast.TypeSwitchStmt:
		return l.clauses(s, s.Body, true, next, kind, label)

	case *ast.SelectStmt:
		h := l.clauses(s, s.Body, false, next, kind, label)
		if len(s.Body.List) == 0 {
			l.blocksForever(h)
		}
		return h

	case *ast.ReturnStmt:
		k := l.keyOf[s]
		l.edge(k, ExitKey, EdgeReturn)
		return k

	case *ast.BranchStmt:
		return l.branch(s, next)

	default:
		// Simple statements, bare / go calls, defer registrations. A statement
		// with no node (an unlabeled empty statement) is transparent.
		k, ok := l.keyOf[s]
		if !ok {
			return next
		}
		l.edge(k, next, kind)
		return k
	}
}

// loop links a for / for-range statement with header node keyOf[s].
func (l *linker) loop(s ast.Stmt, body *ast.BlockStmt, infinite bool, next, label string) string {
	h := l.keyOf[s]
	l.frames = append(l.frames, frame{label: label, brk: next, cont: h})
	entry := l.seq(body.List, h, EdgeLoopBack)
	l.frames = l.frames[:len(l.frames)-1]
	switch {
	case entry == h:
		l.edge(h, h, EdgeLoopBack) // empty body: the header loops on itself
	case infinite:
		l.edge(h, entry, EdgeFallthrough)
	default:
		l.edge(h, entry, EdgeTrue)
	}
	if infinite {
		l.blocksForever(h)
	} else {
		l.edge(h, next, EdgeFalse)
	}
	return h
}

// blocksForever adds the synthetic exception edge h → @exit that keeps a
// header with no normal way out (for {}, select {}) post-dominated by @exit.
// The defer chain redirects it like any exit edge; the later "every node
// reaches @exit" repair then finds h's loop already exiting, and would add
// the same edge anyway (absorbed by the dedup).
func (l *linker) blocksForever(h string) {
	l.edge(h, ExitKey, EdgeException)
}

// clauses links a switch, type switch (noMatch: a missing default falls to
// next) or select (no such edge) with header node keyOf[s]. Clauses are
// linked last to first so each knows where a fallthrough into the next one
// lands.
func (l *linker) clauses(s ast.Stmt, body *ast.BlockStmt, noMatch bool, next, kind, label string) string {
	h := l.keyOf[s]
	hasDefault := false
	l.frames = append(l.frames, frame{label: label, brk: next})
	top := len(l.frames) - 1
	fallInto := next
	for i := len(body.List) - 1; i >= 0; i-- {
		var stmts []ast.Stmt
		switch c := body.List[i].(type) {
		case *ast.CaseClause:
			stmts, hasDefault = c.Body, hasDefault || c.List == nil
		case *ast.CommClause:
			stmts = c.Body
		}
		head := l.keyOf[body.List[i]]
		l.frames[top].fall = fallInto
		entry := l.seq(stmts, next, kind)
		if entry != next {
			l.edge(head, entry, EdgeFallthrough)
		} else {
			l.edge(head, next, kind)
		}
		l.edge(h, head, EdgeSwitchCase)
		fallInto = entry
	}
	l.frames = l.frames[:top]
	if noMatch && !hasDefault {
		l.edge(h, next, EdgeSwitchCase)
	}
	return h
}

// branch links break / continue / goto / fallthrough statement s.
func (l *linker) branch(s *ast.BranchStmt, next string) string {
	k, ok := l.keyOf[s]
	if !ok {
		return next
	}
	name := ""
	if s.Label != nil {
		name = s.Label.Name
	}
	switch s.Tok {
	case token.BREAK:
		if f := l.target(name, false); f != nil {
			l.edge(k, f.brk, EdgeBreak)
		}
	case token.CONTINUE:
		if f := l.target(name, true); f != nil {
			l.edge(k, f.cont, EdgeContinue)
		}
	case token.FALLTHROUGH:
		if n := len(l.frames); n > 0 {
			l.edge(k, l.frames[n-1].fall, EdgeFallthrough)
		}
	case token.GOTO:
		l.gotos = append(l.gotos, pendingGoto{src: k, label: name})
	}
	return k
}

// target returns the innermost frame a break (loop=false) or continue
// (loop=true) with label name ("" = unlabeled) resolves to, or nil for
// ill-formed code.
func (l *linker) target(name string, loop bool) *frame {
	for i := len(l.frames) - 1; i >= 0; i-- {
		f := &l.frames[i]
		if (name == "" || f.label == name) && (!loop || f.cont != "") {
			return f
		}
	}
	return nil
}
