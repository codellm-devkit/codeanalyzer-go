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
	funcs := projectFuncs(prog, pkgs)
	cg, err := runVTA(funcs)
	if err != nil {
		utils.Warn("L4: VTA failed: %v", err)
		deg = append(deg, Degraded{Code: CodeNoVTA, Detail: err.Error()})
	} else {
		utils.Info("L4: VTA over %d functions: %d edges in %s", len(funcs), edgeCount(cg), time.Since(start).Round(time.Millisecond))
	}
	return Result{Degraded: deg}
}

// runVTA runs VTA with no initial graph (VTA then uses its internal lazy CHA
// over funcs), turning a panic into an error.
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
