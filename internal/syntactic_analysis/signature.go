// Package syntactic_analysis builds the symbol table from Go source files.
package syntactic_analysis

import (
	"fmt"
	"go/types"
	"strings"
)

// signatureOf is the single canonicalizer for all signature strings in the analyzer.
// It produces the edge id used in GoCallable.signature, GoType.signature, and every
// call-graph edge source/target. All callers must use this function — never build
// signatures ad-hoc. Caller-side and callee-side ids are then guaranteed identical.
//
// Format:
//   - Package function:  "pkg/path.FuncName"
//   - Method:            "pkg/path.TypeName.MethodName"
//   - Interface method:  "pkg/path.InterfaceName.MethodName"
//   - Type:              "pkg/path.TypeName"
func signatureOf(obj types.Object) string {
	if obj == nil {
		return ""
	}
	pkg := obj.Pkg()
	pkgPath := ""
	if pkg != nil {
		pkgPath = pkg.Path()
	}

	switch o := obj.(type) {
	case *types.Func:
		sig := o.Type().(*types.Signature)
		recv := sig.Recv()
		if recv != nil {
			// Method: extract the receiver type name, stripping pointer indirection.
			recvType := recv.Type()
			if ptr, ok := recvType.(*types.Pointer); ok {
				recvType = ptr.Elem()
			}
			if named, ok := recvType.(*types.Named); ok {
				typeName := named.Obj().Name()
				return fmt.Sprintf("%s.%s.%s", pkgPath, typeName, o.Name())
			}
		}
		return fmt.Sprintf("%s.%s", pkgPath, o.Name())
	case *types.TypeName:
		return fmt.Sprintf("%s.%s", pkgPath, o.Name())
	default:
		return fmt.Sprintf("%s.%s", pkgPath, obj.Name())
	}
}

// signatureOfNamed builds a type signature from a *types.Named directly.
// Used when we have the named type but not a types.Object.
func signatureOfNamed(named *types.Named) string {
	if named == nil {
		return ""
	}
	obj := named.Obj()
	pkgPath := ""
	if obj.Pkg() != nil {
		pkgPath = obj.Pkg().Path()
	}
	return fmt.Sprintf("%s.%s", pkgPath, obj.Name())
}

// signatureForCall builds a callee signature from a *types.Func resolved at a call site.
func signatureForCall(fn *types.Func) string {
	return signatureOf(fn)
}

// normalizeReturnType joins multiple return types into a single parenthesized string.
// Single non-error returns are returned as-is; multiple returns become "(t1, t2, ...)".
func normalizeReturnType(results *types.Tuple) (joined string, parts []string) {
	if results == nil || results.Len() == 0 {
		return "", nil
	}
	parts = make([]string, results.Len())
	for i := 0; i < results.Len(); i++ {
		parts[i] = results.At(i).Type().String()
	}
	if len(parts) == 1 {
		return parts[0], parts
	}
	return "(" + strings.Join(parts, ", ") + ")", parts
}
