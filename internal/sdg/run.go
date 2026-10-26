package sdg

import (
	"fmt"
	"runtime/debug"
	"time"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// vtaCallGraph is vta.CallGraph (a test seam for the recovered-panic path).
var vtaCallGraph = vta.CallGraph

// memLevers enables the output-neutral memory levers of Run (spec §4.13):
// early release of the builder's per-unit caches, interned port names, and
// the lower GC target. A test seam: TestSDG_OutputUnchangedByMemoryLevers
// turns it off to check the result is the same.
var memLevers = true

// l4GCPercent is the GC target during Run. L4's live heap grows by the
// whole-program overlay, and GOGC=100 headroom doubles the transient
// garbage on top of it; 50 trades some GC time for that headroom.
const l4GCPercent = 50

// Run builds the L4 overlay of in. It builds SSA over the project packages,
// joins it to the L3 units, resolves every call site's dispatch targets
// (L2's static callee ∪ VTA's callees), and returns the call-graph edges and
// externals only VTA found, the degradation, and the SDG: every unit's
// vertices, port edges and summaries, the param edges, and the stand-in
// formals.
//
// Order: the join is computed before VTA, so it never reads a program a VTA
// panic may have wedged; after no_vta, later stages use the program for the
// join alone. The degradations are, in order: partial_ssa / no_ssa (SSA),
// l3_skipped and range_func (from the units, whether or not SSA built),
// unmapped_ssa (the join), no_vta.
//
// Run lowers the process-wide GC target to l4GCPercent while it runs and
// restores the previous one on return (debug.SetGCPercent is global: a
// caller running other work concurrently shares the lower target). A target
// already at or below it, or GC off (GOGC=off), is kept. L1–L3 and the
// emission run at the caller's setting.
func Run(in Input) Result {
	if memLevers {
		prev := debug.SetGCPercent(l4GCPercent)
		if prev < l4GCPercent {
			debug.SetGCPercent(prev)
		}
		defer debug.SetGCPercent(prev)
	}
	res, _ := run(in)
	return res
}

// run is Run that also returns the call-site dispatch. Every site keeps its
// static L2 targets whether or not SSA or VTA ran, so the SDG is built on
// every path.
func run(in Input) (Result, *dispatch) {
	res, d := dispatchRun(in)
	overlay(in, d, &res)
	return res, d
}

// dispatchRun is run up to the dispatch: SSA, the join and VTA.
func dispatchRun(in Input) (Result, *dispatch) {
	d := newDispatch(in)
	start := time.Now()
	prog, pkgs, deg := buildSSA(in)
	utils.Info("L4: SSA built %d project packages in %s", len(pkgs), time.Since(start).Round(time.Millisecond))
	for _, dg := range []*Degraded{l3Skipped(in), rangeFunc(in)} {
		if dg != nil {
			deg = append(deg, *dg)
		}
	}
	if len(pkgs) == 0 {
		return d.result(deg), d
	}

	// The VTA function set is built under recover: it builds wrappers on
	// demand, and a panic there may wedge the program, so neither the join
	// nor VTA then reads it.
	start = time.Now()
	funcs, all, err := funcSet(prog, pkgs)
	if err != nil {
		utils.Debug("L4: VTA function set failed: %v", err)
		return d.result(append(deg, Degraded{Code: CodeNoVTA, Detail: err.Error()})), d
	}
	j := newJoin(in)
	cov := j.coverage(funcs)
	utils.Info("L4: SSA join: %d functions (%d by wrapper, %d without a unit), %d calls; %d functions, %d calls unmapped in %s",
		cov.Funcs, cov.Wrappers, cov.NoUnit, cov.Calls, cov.Unmapped, cov.UnmappedCalls, time.Since(start).Round(time.Millisecond))
	if dg := j.degraded(cov); dg != nil {
		deg = append(deg, *dg)
	}
	d.addInvokes(j, funcs)

	start = time.Now()
	cg, err := runVTA(all)
	if err != nil {
		utils.Debug("L4: VTA failed: %v", err)
		deg = append(deg, Degraded{Code: CodeNoVTA, Detail: err.Error()})
	} else {
		d.addVTA(j, cg)
		utils.Info("L4: VTA over %d functions (%d library targets): %d edges in %s",
			len(all), len(all)-len(funcs), edgeCount(cg), time.Since(start).Round(time.Millisecond))
	}
	res := d.result(deg)
	utils.Info("L4: dispatch: %d call sites with targets; %d call_graph edges and %d externals added by VTA",
		len(d.sites), len(res.CallEdges), len(res.NewExternals))
	return res, d
}

// overlay fills res's vertices, port edges, summaries and param edges
// (summary.go, params.go), serially. It reads the dispatch, the L3 results
// and go/types, never the SSA program, so it runs after a no_vta too (the
// dispatch then holds the L2 static targets).
func overlay(in Input, d *dispatch, res *Result) {
	start := time.Now()
	newSDGBuilder(in, d).fill(res)
	var nv, nd, ns int
	for _, c := range res.Callables {
		nv, nd, ns = nv+len(c.Vertices), nd+len(c.Ddg), ns+len(c.Summary)
	}
	utils.Info("L4: %d vertices, %d port edges, %d summary edges, %d param_in, %d param_out, %d stand-ins in %s",
		nv, nd, ns, len(res.ParamIn), len(res.ParamOut), len(res.Externals), time.Since(start).Round(time.Millisecond))
}

// funcSet is projectFuncs under recover: it builds functions on demand
// (MethodValue wrappers), so a builder panic is turned into an error. It
// returns the project functions (the join is measured on them) and the VTA
// set: those plus the target-only library functions.
func funcSet(prog *ssa.Program, pkgs []*ssa.Package) (funcs, all map[*ssa.Function]bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			funcs, all, err = nil, nil, fmt.Errorf("panic: %v", r)
		}
	}()
	funcs, targetOnly := projectFuncs(prog, pkgs)
	all = make(map[*ssa.Function]bool, len(funcs)+len(targetOnly))
	for fn := range funcs {
		all[fn] = true
	}
	for fn := range targetOnly {
		all[fn] = true
	}
	return funcs, all, nil
}

// runVTA runs VTA over funcs with no initial graph (VTA then uses its
// internal lazy CHA over that set), turning a panic into an error. VTA builds
// functions on demand (method lookups), so it may wedge the program: after
// an error, later stages must not read the program beyond the join.
func runVTA(funcs map[*ssa.Function]bool) (cg *callgraph.Graph, err error) {
	defer func() {
		if r := recover(); r != nil {
			cg, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return vtaCallGraph(funcs, nil), nil
}

func edgeCount(cg *callgraph.Graph) int {
	n := 0
	for _, node := range cg.Nodes {
		n += len(node.Out)
	}
	return n
}
