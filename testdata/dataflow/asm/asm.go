// Package asm declares a function with no Go body (implemented in assembly,
// asm.s). The symbol table records it as a callable, but it has no L3 unit:
// the SSA join maps it through bodilessDecl, not fnUnit.
package asm

// Add is implemented in assembly.
func Add(a, b int) int

// Use calls Add.
func Use() int { return Add(1, 2) }
