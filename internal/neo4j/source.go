package neo4j

import (
	"iter"
	"maps"
	"slices"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// Source is the v2 payload as a module stream. *v2emit.Stream satisfies it.
// ParamIn, ParamOut and Degraded are valid only after one complete Modules walk.
type Source interface {
	Envelope() *v2.Analysis
	ModuleKeys() []string
	Modules() iter.Seq2[string, v2.Module]
	ParamIn() iter.Seq[v2.ParamEdge]
	ParamOut() iter.Seq[v2.ParamEdge]
	Degraded() []v2.Degraded
}

// TreeSource adapts an in-memory tree to a Source. Tests and Project use it.
func TreeSource(a *v2.Analysis) Source { return treeSource(a) }

type tree struct {
	a   *v2.Analysis
	env *v2.Analysis
}

func treeSource(a *v2.Analysis) *tree {
	env := *a
	env.Application.SymbolTable = nil
	env.Application.ParamIn = nil
	env.Application.ParamOut = nil
	env.Degraded = nil
	return &tree{a: a, env: &env}
}

func (t *tree) Envelope() *v2.Analysis { return t.env }

func (t *tree) ModuleKeys() []string {
	return slices.Sorted(maps.Keys(t.a.Application.SymbolTable))
}

func (t *tree) Modules() iter.Seq2[string, v2.Module] {
	return func(yield func(string, v2.Module) bool) {
		for _, k := range t.ModuleKeys() {
			if !yield(k, t.a.Application.SymbolTable[k]) {
				return
			}
		}
	}
}

func (t *tree) ParamIn() iter.Seq[v2.ParamEdge]  { return slices.Values(t.a.Application.ParamIn) }
func (t *tree) ParamOut() iter.Seq[v2.ParamEdge] { return slices.Values(t.a.Application.ParamOut) }
func (t *tree) Degraded() []v2.Degraded          { return t.a.Degraded }
