package syntactic_analysis

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"reflect"
	"strings"
	"unicode"

	"golang.org/x/tools/go/packages"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// SymbolTableBuilder constructs a symbol table by loading packages with full type
// information via golang.org/x/tools/go/packages. One builder per analysis run.
//
// Architecture mirrors codeanalyzer-python's SymbolTableBuilder: a cohesive struct
// with per-node-kind private methods, sharing the loaded package context on self.
type SymbolTableBuilder struct {
	projectDir string
	fset       *token.FileSet
	// pkgs is the flat list of loaded packages, keyed by package path.
	pkgs map[string]*packages.Package
}

// NewSymbolTableBuilder creates a builder for projectDir.
func NewSymbolTableBuilder(projectDir string) *SymbolTableBuilder {
	return &SymbolTableBuilder{
		projectDir: projectDir,
		fset:       token.NewFileSet(),
		pkgs:       map[string]*packages.Package{},
	}
}

// Build loads all packages under projectDir (or only targetFiles if non-empty),
// walks each file, and returns the symbol table keyed by relative file path.
func (b *SymbolTableBuilder) Build(targetFiles []string, skipTests bool) (map[string]schema.GoFile, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo |
			packages.NeedImports |
			packages.NeedDeps,
		Dir:  b.projectDir,
		Fset: b.fset,
		// Silence go vet; we only need type info, not a full build.
		BuildFlags: []string{},
	}

	// Build pattern(s) for packages.Load.
	patterns := []string{"./..."}
	if len(targetFiles) > 0 {
		// Map each target file to a file= pattern; packages.Load accepts multiple patterns.
		patterns = make([]string, len(targetFiles))
		for i, f := range targetFiles {
			patterns[i] = "file=" + f
		}
	}

	pkgList, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}

	// Collect packages, warn on errors but don't abort (graceful partial analysis).
	for _, pkg := range pkgList {
		if len(pkg.Errors) > 0 {
			for _, e := range pkg.Errors {
				utils.Warn("package %s: %v", pkg.PkgPath, e)
			}
		}
		if pkg.Types != nil {
			b.pkgs[pkg.PkgPath] = pkg
		}
	}

	symbolTable := map[string]schema.GoFile{}

	for _, pkg := range b.pkgs {
		for i, astFile := range pkg.Syntax {
			if i >= len(pkg.GoFiles) {
				continue
			}
			filePath := pkg.GoFiles[i]
			relPath := utils.RelativePath(b.projectDir, filePath)
			if skipTests && utils.IsTestFile(relPath) {
				continue
			}
			if utils.IsVendored(relPath) {
				continue
			}
			goFile := b.buildGoFile(pkg, astFile, filePath, relPath)
			symbolTable[relPath] = goFile
		}
	}

	// In Go a method can be defined in any file of the package; the main loop
	// above only attaches a method when its receiver type is in the same file.
	// This pass finds methods whose type lives in a sibling file and attaches them.
	b.reconcileCrossFileMethods(symbolTable)

	return symbolTable, nil
}

// reconcileCrossFileMethods attaches methods to their type's owner file when
// the method and its receiver type are declared in different files of the same package.
func (b *SymbolTableBuilder) reconcileCrossFileMethods(symbolTable map[string]schema.GoFile) {
	// Build index: (pkgPath, shortTypeName) → relPath of the file that owns the type.
	type typeKey struct{ pkgPath, typeName string }
	typeOwner := make(map[typeKey]string)
	for relPath, gf := range symbolTable {
		pkgPath := b.filePkgPath(relPath)
		for typeName := range gf.Types {
			typeOwner[typeKey{pkgPath, typeName}] = relPath
		}
	}

	for _, pkg := range b.pkgs {
		for i, astFile := range pkg.Syntax {
			if i >= len(pkg.GoFiles) {
				continue
			}
			filePath := pkg.GoFiles[i]
			relPath := utils.RelativePath(b.projectDir, filePath)

			for _, decl := range astFile.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Recv == nil {
					continue
				}
				typeName := b.receiverTypeName(fd.Recv)
				if typeName == "" {
					continue
				}
				ownerRelPath, ok := typeOwner[typeKey{pkg.PkgPath, typeName}]
				if !ok || ownerRelPath == relPath {
					continue // type not found or already handled by the main loop
				}

				callable := b.buildCallable(pkg, astFile, fd)
				if callable == nil {
					continue
				}
				ownerFile, found := symbolTable[ownerRelPath]
				if !found {
					continue
				}
				gt, found := ownerFile.Types[typeName]
				if !found {
					continue
				}
				if _, alreadyPresent := gt.Methods[callable.Signature]; !alreadyPresent {
					gt.Methods[callable.Signature] = *callable
					ownerFile.Types[typeName] = gt
					symbolTable[ownerRelPath] = ownerFile
				}
			}
		}
	}
}

// filePkgPath returns the package import path for the file at relPath.
func (b *SymbolTableBuilder) filePkgPath(relPath string) string {
	for _, pkg := range b.pkgs {
		for _, absFile := range pkg.GoFiles {
			if utils.RelativePath(b.projectDir, absFile) == relPath {
				return pkg.PkgPath
			}
		}
	}
	return ""
}

// ─── Per-file builder ──────────────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildGoFile(
	pkg *packages.Package,
	astFile *ast.File,
	absPath, relPath string,
) schema.GoFile {
	info, _ := os.Stat(absPath)
	hash, _ := utils.FileHash(absPath)

	var lastMod *float64
	var fileSize *int64
	if info != nil {
		lm := float64(info.ModTime().Unix()) + float64(info.ModTime().Nanosecond())/1e9
		lastMod = &lm
		sz := info.Size()
		fileSize = &sz
	}
	var contentHash *string
	if hash != "" {
		contentHash = &hash
	}

	gf := schema.GoFile{
		FilePath:     relPath,
		PackageName:  astFile.Name.Name,
		Imports:      b.buildImports(astFile),
		Comments:     b.buildFileComments(astFile),
		Types:        map[string]schema.GoType{},
		Functions:    map[string]schema.GoCallable{},
		Variables:    b.buildPackageVars(pkg, astFile),
		ContentHash:  contentHash,
		LastModified: lastMod,
		FileSize:     fileSize,
	}

	// Walk top-level declarations.
	for _, decl := range astFile.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			b.processGenDecl(pkg, astFile, d, &gf)
		case *ast.FuncDecl:
			callable := b.buildCallable(pkg, astFile, d)
			if callable == nil {
				continue
			}
			if d.Recv != nil {
				// Method — attach to its type.
				if typeName := b.receiverTypeName(d.Recv); typeName != "" {
					if gt, ok := gf.Types[typeName]; ok {
						gt.Methods[callable.Signature] = *callable
						gf.Types[typeName] = gt
					}
				}
			} else {
				gf.Functions[callable.Signature] = *callable
			}
		}
	}

	return gf
}

// ─── Imports ──────────────────────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildImports(astFile *ast.File) []schema.GoImport {
	var imports []schema.GoImport
	for _, imp := range astFile.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		alias := ""
		if imp.Name != nil {
			alias = imp.Name.Name
		}
		pos := b.fset.Position(imp.Pos())
		end := b.fset.Position(imp.End())
		imports = append(imports, schema.GoImport{
			Module:    path,
			Alias:     alias,
			StartLine: pos.Line,
			EndLine:   end.Line,
		})
	}
	return imports
}

// ─── Comments ─────────────────────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildFileComments(astFile *ast.File) []schema.GoComment {
	var comments []schema.GoComment
	for _, cg := range astFile.Comments {
		for _, c := range cg.List {
			pos := b.fset.Position(c.Pos())
			end := b.fset.Position(c.End())
			comments = append(comments, schema.GoComment{
				Content:      c.Text,
				StartLine:    pos.Line,
				EndLine:      end.Line,
				StartColumn:  pos.Column,
				EndColumn:    end.Column,
				IsDocComment: strings.HasPrefix(c.Text, "//") || strings.HasPrefix(c.Text, "/*"),
			})
		}
	}
	return comments
}

func (b *SymbolTableBuilder) docComments(doc *ast.CommentGroup) []schema.GoComment {
	if doc == nil {
		return nil
	}
	var comments []schema.GoComment
	for _, c := range doc.List {
		pos := b.fset.Position(c.Pos())
		end := b.fset.Position(c.End())
		comments = append(comments, schema.GoComment{
			Content:      c.Text,
			StartLine:    pos.Line,
			EndLine:      end.Line,
			StartColumn:  pos.Column,
			EndColumn:    end.Column,
			IsDocComment: true,
		})
	}
	return comments
}

// ─── GenDecl processor (type/var/const declarations) ─────────────────────────

func (b *SymbolTableBuilder) processGenDecl(
	pkg *packages.Package,
	astFile *ast.File,
	decl *ast.GenDecl,
	gf *schema.GoFile,
) {
	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			gt := b.buildType(pkg, astFile, decl, s)
			if gt != nil {
				gf.Types[gt.Name] = *gt
			}
		}
	}
}

// ─── Type builder ─────────────────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildType(
	pkg *packages.Package,
	astFile *ast.File,
	decl *ast.GenDecl,
	spec *ast.TypeSpec,
) *schema.GoType {
	typeName := spec.Name.Name
	isExported := unicode.IsUpper(rune(typeName[0]))

	// Resolve via type info if available.
	var typSig string
	if pkg.TypesInfo != nil {
		if obj, ok := pkg.TypesInfo.Defs[spec.Name]; ok && obj != nil {
			typSig = signatureOf(obj)
		}
	}
	if typSig == "" {
		typSig = pkg.Types.Path() + "." + typeName
	}

	pos := b.fset.Position(spec.Pos())
	end := b.fset.Position(spec.End())

	gt := &schema.GoType{
		Name:        typeName,
		Signature:   typSig,
		Comments:    b.docComments(decl.Doc),
		IsExported:  isExported,
		Fields:      []schema.GoField{},
		Methods:     map[string]schema.GoCallable{},
		BaseTypes:   []string{},
		InnerTypes:  map[string]schema.GoType{},
		StartLine:   pos.Line,
		EndLine:     end.Line,
	}

	switch t := spec.Type.(type) {
	case *ast.StructType:
		gt.IsInterface = false
		gt.Fields = b.buildStructFields(pkg, t)
		gt.BaseTypes = b.embeddedTypes(pkg, t)
	case *ast.InterfaceType:
		gt.IsInterface = true
		// Interface methods are collected when we process FuncDecl with receivers,
		// but interface method signatures within the interface type are also recorded here.
		b.collectInterfaceMethods(pkg, t, gt)
	}

	return gt
}

func (b *SymbolTableBuilder) buildStructFields(pkg *packages.Package, st *ast.StructType) []schema.GoField {
	var fields []schema.GoField
	if st.Fields == nil {
		return fields
	}
	for _, field := range st.Fields.List {
		typStr := b.typeString(pkg, field.Type)
		tags := parseStructTags(field.Tag)
		isEmbedded := len(field.Names) == 0

		if isEmbedded {
			pos := b.fset.Position(field.Pos())
			end := b.fset.Position(field.End())
			fields = append(fields, schema.GoField{
				Name:       typStr,
				Type:       typStr,
				Tags:       tags,
				IsExported: true,
				IsEmbedded: true,
				StartLine:  pos.Line,
				EndLine:    end.Line,
			})
			continue
		}
		for _, name := range field.Names {
			pos := b.fset.Position(name.Pos())
			end := b.fset.Position(field.End())
			fields = append(fields, schema.GoField{
				Name:       name.Name,
				Type:       typStr,
				Tags:       tags,
				IsExported: unicode.IsUpper(rune(name.Name[0])),
				IsEmbedded: false,
				StartLine:  pos.Line,
				EndLine:    end.Line,
			})
		}
	}
	return fields
}

func (b *SymbolTableBuilder) embeddedTypes(pkg *packages.Package, st *ast.StructType) []string {
	var embedded []string
	if st.Fields == nil {
		return embedded
	}
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			embedded = append(embedded, b.typeString(pkg, field.Type))
		}
	}
	return embedded
}

func (b *SymbolTableBuilder) collectInterfaceMethods(pkg *packages.Package, it *ast.InterfaceType, gt *schema.GoType) {
	if it.Methods == nil {
		return
	}
	for _, method := range it.Methods.List {
		if len(method.Names) == 0 {
			// Embedded interface — add to base_types.
			gt.BaseTypes = append(gt.BaseTypes, b.typeString(pkg, method.Type))
			continue
		}
		for _, name := range method.Names {
			pos := b.fset.Position(name.Pos())
			end := b.fset.Position(method.End())

			var retType string
			var retTypes []string
			if ft, ok := method.Type.(*ast.FuncType); ok && ft.Results != nil {
				for _, r := range ft.Results.List {
					retTypes = append(retTypes, b.typeString(pkg, r.Type))
				}
				retType = b.joinReturnTypes(retTypes)
			}

			sig := pkg.Types.Path() + "." + gt.Name + "." + name.Name
			callable := schema.GoCallable{
				Name:          name.Name,
				Path:          "",
				Signature:     sig,
				Parameters:    b.buildFuncTypeParams(pkg, method.Type),
				ReturnType:    retType,
				ReturnTypes:   retTypes,
				IsExported:    unicode.IsUpper(rune(name.Name[0])),
				ReceiverType:  gt.Signature,
				CallSites:     []schema.GoCallsite{},
				InnerCallables: map[string]schema.GoCallable{},
				StartLine:     pos.Line,
				EndLine:       end.Line,
			}
			gt.Methods[sig] = callable
		}
	}
}

// ─── Callable builder ─────────────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildCallable(
	pkg *packages.Package,
	astFile *ast.File,
	decl *ast.FuncDecl,
) *schema.GoCallable {
	name := decl.Name.Name
	isExported := unicode.IsUpper(rune(name[0]))
	pos := b.fset.Position(decl.Pos())
	end := b.fset.Position(decl.End())

	var sig string
	if pkg.TypesInfo != nil {
		if obj, ok := pkg.TypesInfo.Defs[decl.Name]; ok && obj != nil {
			sig = signatureOf(obj)
		}
	}
	if sig == "" {
		if decl.Recv != nil {
			if recvName := b.receiverTypeName(decl.Recv); recvName != "" {
				sig = pkg.Types.Path() + "." + recvName + "." + name
			}
		}
		if sig == "" {
			sig = pkg.Types.Path() + "." + name
		}
	}

	var recvType, recvName string
	if decl.Recv != nil && len(decl.Recv.List) > 0 {
		rf := decl.Recv.List[0]
		recvType = b.typeString(pkg, rf.Type)
		if len(rf.Names) > 0 {
			recvName = rf.Names[0].Name
		}
	}

	retType, retTypes := b.buildReturnTypes(pkg, decl.Type)
	bodyStart := pos.Line
	if decl.Body != nil {
		bodyStart = b.fset.Position(decl.Body.Pos()).Line
	}

	callable := &schema.GoCallable{
		Name:               name,
		Path:               utils.RelativePath(b.projectDir, b.fset.File(decl.Pos()).Name()),
		Signature:          sig,
		Comments:           b.docComments(decl.Doc),
		Parameters:         b.buildParams(pkg, decl.Type),
		ReturnType:         retType,
		ReturnTypes:        retTypes,
		IsExported:         isExported,
		ReceiverType:       recvType,
		ReceiverName:       recvName,
		CallSites:          []schema.GoCallsite{},
		InnerCallables:     map[string]schema.GoCallable{},
		LocalVariables:     []schema.GoVariableDeclaration{},
		AccessedSymbols:    []schema.GoSymbol{},
		StartLine:          pos.Line,
		EndLine:            end.Line,
		CodeStartLine:      bodyStart,
		CyclomaticComplexity: b.cyclomaticComplexity(decl),
	}

	if decl.Body != nil {
		callable.Code = b.nodeSource(decl)
		callable.CallSites = b.buildCallSites(pkg, decl.Body)
		callable.LocalVariables = b.buildLocalVars(pkg, decl.Body)
	}

	return callable
}

// ─── Parameters ───────────────────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildParams(pkg *packages.Package, ft *ast.FuncType) []schema.GoParameter {
	if ft == nil || ft.Params == nil {
		return nil
	}
	var params []schema.GoParameter
	for _, field := range ft.Params.List {
		typStr := b.typeString(pkg, field.Type)
		isVariadic := false
		if _, ok := field.Type.(*ast.Ellipsis); ok {
			isVariadic = true
		}
		pos := b.fset.Position(field.Pos())
		end := b.fset.Position(field.End())
		if len(field.Names) == 0 {
			params = append(params, schema.GoParameter{
				Name: "_", Type: typStr, IsVariadic: isVariadic,
				StartLine: pos.Line, EndLine: end.Line,
			})
			continue
		}
		for _, name := range field.Names {
			params = append(params, schema.GoParameter{
				Name: name.Name, Type: typStr, IsVariadic: isVariadic,
				StartLine: pos.Line, EndLine: end.Line,
			})
		}
	}
	return params
}

func (b *SymbolTableBuilder) buildFuncTypeParams(pkg *packages.Package, expr ast.Expr) []schema.GoParameter {
	if ft, ok := expr.(*ast.FuncType); ok {
		return b.buildParams(pkg, ft)
	}
	return nil
}

// ─── Return types ─────────────────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildReturnTypes(pkg *packages.Package, ft *ast.FuncType) (string, []string) {
	if ft == nil || ft.Results == nil {
		return "", nil
	}
	var parts []string
	for _, field := range ft.Results.List {
		typStr := b.typeString(pkg, field.Type)
		if len(field.Names) == 0 {
			parts = append(parts, typStr)
		} else {
			for range field.Names {
				parts = append(parts, typStr)
			}
		}
	}
	return b.joinReturnTypes(parts), parts
}

func (b *SymbolTableBuilder) joinReturnTypes(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// ─── Call sites ───────────────────────────────────────────────────────────────

// buildCallSites walks the function body and records every call expression.
// callee_signature is left nil here — the resolver backfills it in the call-graph stage.
func (b *SymbolTableBuilder) buildCallSites(pkg *packages.Package, body *ast.BlockStmt) []schema.GoCallsite {
	var sites []schema.GoCallsite
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.GoStmt:
			// Goroutine launch — record with is_goroutine=true.
			if call, ok := node.Call.Fun.(*ast.CallExpr); ok {
				_ = call
			}
			site := b.callExprToSite(pkg, node.Call, true)
			if site != nil {
				sites = append(sites, *site)
			}
			return false
		case *ast.CallExpr:
			site := b.callExprToSite(pkg, node, false)
			if site != nil {
				sites = append(sites, *site)
			}
		}
		return true
	})
	return sites
}

func (b *SymbolTableBuilder) callExprToSite(pkg *packages.Package, call *ast.CallExpr, isGoroutine bool) *schema.GoCallsite {
	pos := b.fset.Position(call.Pos())
	end := b.fset.Position(call.End())

	var methodName, receiverExpr, receiverType string
	isConstructor := false

	switch fn := call.Fun.(type) {
	case *ast.Ident:
		methodName = fn.Name
		// Check if it's a type conversion / constructor call.
		if pkg.TypesInfo != nil {
			if obj := pkg.TypesInfo.ObjectOf(fn); obj != nil {
				if _, ok := obj.(*types.TypeName); ok {
					isConstructor = true
				}
			}
		}
	case *ast.SelectorExpr:
		methodName = fn.Sel.Name
		receiverExpr = b.exprString(fn.X)
		if pkg.TypesInfo != nil {
			if t := pkg.TypesInfo.TypeOf(fn.X); t != nil {
				receiverType = t.String()
			}
		}
	default:
		methodName = b.exprString(call.Fun)
	}

	// Collect argument types.
	var argTypes []string
	for _, arg := range call.Args {
		if pkg.TypesInfo != nil {
			if t := pkg.TypesInfo.TypeOf(arg); t != nil {
				argTypes = append(argTypes, t.String())
				continue
			}
		}
		argTypes = append(argTypes, "")
	}

	return &schema.GoCallsite{
		MethodName:        methodName,
		ReceiverExpr:      receiverExpr,
		ReceiverType:      receiverType,
		ArgumentTypes:     argTypes,
		IsConstructorCall: isConstructor,
		IsGoroutine:       isGoroutine,
		CalleeSignature:   nil, // backfilled by the call-graph stage
		StartLine:         pos.Line,
		StartColumn:       pos.Column,
		EndLine:           end.Line,
		EndColumn:         end.Column,
	}
}

// ─── Local variables ──────────────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildLocalVars(pkg *packages.Package, body *ast.BlockStmt) []schema.GoVariableDeclaration {
	var vars []schema.GoVariableDeclaration
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if node.Tok.String() == ":=" {
				for i, lhs := range node.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok {
						pos := b.fset.Position(ident.Pos())
						typStr := ""
						if pkg.TypesInfo != nil {
							if obj := pkg.TypesInfo.ObjectOf(ident); obj != nil {
								typStr = obj.Type().String()
							}
						}
						init := ""
						if i < len(node.Rhs) {
							init = b.exprString(node.Rhs[i])
						}
						vars = append(vars, schema.GoVariableDeclaration{
							Name: ident.Name, Type: typStr, Initializer: init,
							Scope: "function", StartLine: pos.Line, EndLine: pos.Line,
						})
					}
				}
			}
		case *ast.DeclStmt:
			if gen, ok := node.Decl.(*ast.GenDecl); ok {
				for _, spec := range gen.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for i, name := range vs.Names {
							pos := b.fset.Position(name.Pos())
							typStr := ""
							if vs.Type != nil {
								typStr = b.typeString(pkg, vs.Type)
							} else if pkg.TypesInfo != nil {
								if obj := pkg.TypesInfo.ObjectOf(name); obj != nil {
									typStr = obj.Type().String()
								}
							}
							init := ""
							if i < len(vs.Values) {
								init = b.exprString(vs.Values[i])
							}
							vars = append(vars, schema.GoVariableDeclaration{
								Name: name.Name, Type: typStr, Initializer: init,
								Scope: "function", StartLine: pos.Line, EndLine: pos.Line,
							})
						}
					}
				}
			}
		}
		return true
	})
	return vars
}

// ─── Package-level variables ──────────────────────────────────────────────────

func (b *SymbolTableBuilder) buildPackageVars(pkg *packages.Package, astFile *ast.File) []schema.GoVariableDeclaration {
	var vars []schema.GoVariableDeclaration
	for _, decl := range astFile.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		if gen.Tok.String() != "var" && gen.Tok.String() != "const" {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				pos := b.fset.Position(name.Pos())
				typStr := ""
				if vs.Type != nil {
					typStr = b.typeString(pkg, vs.Type)
				} else if pkg.TypesInfo != nil {
					if obj := pkg.TypesInfo.ObjectOf(name); obj != nil {
						typStr = obj.Type().String()
					}
				}
				init := ""
				if i < len(vs.Values) {
					init = b.exprString(vs.Values[i])
				}
				vars = append(vars, schema.GoVariableDeclaration{
					Name: name.Name, Type: typStr, Initializer: init,
					Scope: "package", StartLine: pos.Line, EndLine: pos.Line,
				})
			}
		}
	}
	return vars
}

// ─── Cyclomatic complexity ────────────────────────────────────────────────────

// cyclomaticComplexity computes McCabe complexity: 1 + decision points.
func (b *SymbolTableBuilder) cyclomaticComplexity(decl *ast.FuncDecl) int {
	if decl.Body == nil {
		return 0
	}
	complexity := 1
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch n.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt,
			*ast.TypeSwitchStmt, *ast.SelectStmt, *ast.CaseClause,
			*ast.CommClause:
			complexity++
		}
		return true
	})
	return complexity
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

// receiverTypeName extracts the base type name from a receiver field list.
func (b *SymbolTableBuilder) receiverTypeName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	// Strip pointer.
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return ""
}

// typeString returns a human-readable string for an ast.Expr type node.
func (b *SymbolTableBuilder) typeString(pkg *packages.Package, expr ast.Expr) string {
	if pkg.TypesInfo != nil {
		if t := pkg.TypesInfo.TypeOf(expr); t != nil {
			return t.String()
		}
	}
	// Fallback: print the expression.
	return b.exprString(expr)
}

// exprString returns a best-effort source representation of an expression.
func (b *SymbolTableBuilder) exprString(expr ast.Expr) string {
	if expr == nil {
		return ""
	}
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return b.exprString(e.X) + "." + e.Sel.Name
	case *ast.StarExpr:
		return "*" + b.exprString(e.X)
	case *ast.ArrayType:
		return "[]" + b.exprString(e.Elt)
	case *ast.MapType:
		return "map[" + b.exprString(e.Key) + "]" + b.exprString(e.Value)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.BasicLit:
		return e.Value
	default:
		return fmt.Sprint(expr)
	}
}

// nodeSource extracts the raw source text of a node (best effort).
func (b *SymbolTableBuilder) nodeSource(node ast.Node) string {
	pos := b.fset.Position(node.Pos())
	if pos.Filename == "" {
		return ""
	}
	data, err := os.ReadFile(pos.Filename)
	if err != nil {
		return ""
	}
	startOff := b.fset.File(node.Pos()).Offset(node.Pos())
	endOff := b.fset.File(node.End()).Offset(node.End())
	if startOff < 0 || endOff > len(data) || startOff >= endOff {
		return ""
	}
	return string(data[startOff:endOff])
}

// parseStructTags parses a struct tag literal into a key→value map.
// e.g. `json:"name,omitempty" db:"name"` → {"json": "name,omitempty", "db": "name"}
func parseStructTags(lit *ast.BasicLit) map[string]string {
	tags := map[string]string{}
	if lit == nil {
		return tags
	}
	raw := strings.Trim(lit.Value, "`")
	st := reflect.StructTag(raw)
	// Iterate common tag keys; for a full parse, walk the raw string.
	for _, key := range extractTagKeys(raw) {
		if v := st.Get(key); v != "" {
			tags[key] = v
		}
	}
	return tags
}

// extractTagKeys extracts tag key names from a raw struct tag string.
func extractTagKeys(raw string) []string {
	var keys []string
	for len(raw) > 0 {
		raw = strings.TrimLeft(raw, " \t")
		if raw == "" {
			break
		}
		idx := strings.IndexByte(raw, ':')
		if idx < 0 {
			break
		}
		keys = append(keys, raw[:idx])
		// Skip past the value.
		rest := raw[idx+1:]
		if len(rest) == 0 || rest[0] != '"' {
			break
		}
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			break
		}
		raw = rest[end+2:]
	}
	return keys
}

// ensure fmt is used (used in exprString fallback and signature.go).
var _ = fmt.Sprintf
