package sdg

import (
	"fmt"
	"time"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/ssa"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// vtaCallGraph is vta.CallGraph (a test seam for the recovered-panic path).
var vtaCallGraph = vta.CallGraph

// Run builds the L4 overlay of in. For now it builds SSA over the project
// packages, joins it to the L3 units, and runs VTA once over the project
// function set, and returns only the degradation: no vertices or edges yet.
//
// Order: the join is computed before VTA, so it never reads a program a VTA
// panic may have wedged; after no_vta, later stages use the program for the
// join alone. The degradations are, in order: partial_ssa / no_ssa (SSA),
// l3_skipped and range_func (from the units, whether or not SSA built),
// unmapped_ssa (the join), no_vta.
func Run(in Input) Result {
	start := time.Now()
	prog, pkgs, deg := buildSSA(in)
	utils.Info("L4: SSA built %d project packages in %s", len(pkgs), time.Since(start).Round(time.Millisecond))
	for _, d := range []*Degraded{l3Skipped(in), rangeFunc(in)} {
		if d != nil {
			deg = append(deg, *d)
		}
	}
	if len(pkgs) == 0 {
		return Result{Degraded: deg}
	}

	// The project function set (VTA's seeds) is built under recover: it
	// builds wrappers on demand, and a panic there may wedge the program,
	// so neither the join nor VTA then reads it.
	start = time.Now()
	funcs, err := funcSet(prog, pkgs)
	if err != nil {
		utils.Debug("L4: VTA function set failed: %v", err)
		return Result{Degraded: append(deg, Degraded{Code: CodeNoVTA, Detail: err.Error()})}
	}
	j := newJoin(in)
	cov := j.coverage(funcs)
	utils.Info("L4: SSA join: %d functions (%d by wrapper), %d calls; %d functions, %d calls unmapped in %s",
		cov.Funcs, cov.Wrappers, cov.Calls, cov.Unmapped, cov.UnmappedCalls, time.Since(start).Round(time.Millisecond))
	if d := j.degraded(cov); d != nil {
		deg = append(deg, *d)
	}

	start = time.Now()
	cg, err := runVTA(funcs)
	if err != nil {
		utils.Debug("L4: VTA failed: %v", err)
		deg = append(deg, Degraded{Code: CodeNoVTA, Detail: err.Error()})
	} else {
		utils.Info("L4: VTA over %d functions: %d edges in %s", len(funcs), edgeCount(cg), time.Since(start).Round(time.Millisecond))
	}
	return Result{Degraded: deg}
}

// funcSet is projectFuncs under recover: it builds functions on demand
// (MethodValue wrappers), so a builder panic is turned into an error.
func funcSet(prog *ssa.Program, pkgs []*ssa.Package) (funcs map[*ssa.Function]bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			funcs, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return projectFuncs(prog, pkgs), nil
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
