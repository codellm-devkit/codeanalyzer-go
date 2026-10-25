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
// packages and runs VTA once over the project function set, and returns only
// the degradation: no vertices or edges yet.
func Run(in Input) Result {
	start := time.Now()
	prog, pkgs, deg := buildSSA(in)
	utils.Info("L4: SSA built %d project packages in %s", len(pkgs), time.Since(start).Round(time.Millisecond))
	if len(pkgs) == 0 {
		return Result{Degraded: deg}
	}

	start = time.Now()
	cg, nfuncs, err := runVTA(prog, pkgs)
	if err != nil {
		utils.Debug("L4: VTA failed: %v", err)
		deg = append(deg, Degraded{Code: CodeNoVTA, Detail: err.Error()})
	} else {
		utils.Info("L4: VTA over %d functions: %d edges in %s", nfuncs, edgeCount(cg), time.Since(start).Round(time.Millisecond))
	}
	return Result{Degraded: deg}
}

// runVTA builds the project function set and runs VTA over it with no
// initial graph (VTA then uses its internal lazy CHA over that set), turning
// a panic in either into an error. Both stages build functions on demand
// (projectFuncs' MethodValue wrappers, VTA's method lookups), so both sit
// under the recover.
func runVTA(prog *ssa.Program, pkgs []*ssa.Package) (cg *callgraph.Graph, nfuncs int, err error) {
	defer func() {
		if r := recover(); r != nil {
			cg, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	funcs := projectFuncs(prog, pkgs)
	return vtaCallGraph(funcs, nil), len(funcs), nil
}

func edgeCount(cg *callgraph.Graph) int {
	n := 0
	for _, node := range cg.Nodes {
		n += len(node.Out)
	}
	return n
}
