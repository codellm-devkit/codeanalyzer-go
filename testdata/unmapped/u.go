// Package unmapped holds a function literal in a package-level variable
// initializer. The symbol table has no callable for it (L1 records callables
// of declarations only), so its SSA function, a child of the synthetic
// package initializer, maps to nothing: unmapped_ssa.
package unmapped

func helper() int { return 1 }

// Handler is a package-level function literal.
var Handler = func() int { return helper() }

// Use is a declared function: it maps.
func Use() int { return Handler() }
