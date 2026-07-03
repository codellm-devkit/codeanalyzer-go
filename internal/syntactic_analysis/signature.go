// Package syntactic_analysis builds the symbol table from Go source files.
package syntactic_analysis

import (
	"fmt"
	"go/types"
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

