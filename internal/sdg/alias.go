package sdg

import (
	"go/types"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
)

// ProvPointsTo is the prov of every may-alias ddg edge (spec §4.10): the L4
// semantic delta over L3's "ssa" edges and the "reaching-defs" port wiring.
const ProvPointsTo = "points-to"

// AliasOracle answers may-alias queries between two access paths of one
// callable (spec §4.10). The interface is frozen so that a points-to
// analysis (an Andersen solve over go/ssa) can replace the type-based MVP
// without a schema change; prov stays "points-to".
//
// The DDG solver (dataflow.BuildDDGAlias) consults it only for a def and a
// use with different bases and prefix-compatible suffixes, the def's suffix
// non-empty, so MayAlias decides on the bases alone.
type AliasOracle interface {
	MayAlias(a, b dataflow.Path) bool
}

// aliasOracle is the oracle the port wiring consults; nil turns the alias
// pass off (a test seam: TestAlias_AddsOnly compares Run off vs on).
var aliasOracle AliasOracle = typeOracle{}

// typeOracle is the type-based may-alias MVP (spec §4.10). Two paths may
// alias iff
//
//   - both have a base object (a nil base, a "<defer>:" pseudo path or an
//     unresolved identifier, never aliases anything), and
//   - they share the base, or both bases' types may hold a reference
//     (dataflow.MayHoldRef, the predicate the by-ref rules share) and either
//     the types are identical or one is an interface the other's type
//     implements (types.Implements on T or *T).
//
// A value type that holds no reference never aliases: its copy is its own
// memory. A package-level variable is a base like any other, so a global
// pointer may alias a local one of its type; so may a closure's capture.
// A type parameter is compared by identity only, never through its
// constraint's interface.
type typeOracle struct{}

func (typeOracle) MayAlias(a, b dataflow.Path) bool {
	if a.Base == nil || b.Base == nil {
		return false
	}
	if a.Base == b.Base {
		return true
	}
	return typesMayAlias(a.Base.Type(), b.Base.Type())
}

func typesMayAlias(ta, tb types.Type) bool {
	if !dataflow.MayHoldRef(ta) || !dataflow.MayHoldRef(tb) {
		return false
	}
	if types.Identical(ta, tb) {
		return true
	}
	return implements(ta, tb) || implements(tb, ta)
}

// implements reports whether iface is an interface type (not a type
// parameter) that t or *t implements.
func implements(t, iface types.Type) bool {
	if _, tp := iface.(*types.TypeParam); tp {
		return false
	}
	it, ok := iface.Underlying().(*types.Interface)
	if !ok {
		return false
	}
	if types.Implements(t, it) {
		return true
	}
	if _, ptr := t.Underlying().(*types.Pointer); ptr || types.IsInterface(t) {
		return false
	}
	return types.Implements(types.NewPointer(t), it)
}

// memoOracle memoizes o per base pair for one unit's alias solve: the
// solver asks about the same two bases once per reaching def and use, and
// types.Implements walks method sets. It lives only for one wire call.
// Only typeOracle, which decides on the bases alone, is memoized; any other
// oracle is asked every time.
func memoOracle(o AliasOracle) func(a, b dataflow.Path) bool {
	if _, ok := o.(typeOracle); !ok {
		return o.MayAlias
	}
	type pair struct{ a, b types.Object }
	memo := map[pair]bool{}
	return func(a, b dataflow.Path) bool {
		if a.Base == nil || b.Base == nil {
			return o.MayAlias(a, b)
		}
		k := pair{a.Base, b.Base}
		v, ok := memo[k]
		if !ok {
			v = o.MayAlias(a, b)
			memo[k] = v
		}
		return v
	}
}
