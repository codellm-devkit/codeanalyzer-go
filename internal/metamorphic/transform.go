package metamorphic

// Source transforms. Each one rewrites some files of a module copy and
// returns a PosMap from every old position it kept to its new position, the
// marks it set in its new text, and its sites (what the relations check).
//
// The transforms find their sites with go/ast and go/types. The workspace is
// loaded with the analyzer's own go/packages mode. They never re-print a
// whole file: go/printer would reformat it and move free-floating comments,
// and with them every position in the file. Instead a new file is spliced
// from byte ranges of the old one, copied verbatim, and new text. That keeps
// the PosMap exact: a copied byte maps by its offset in the copy, and the
// start of a replaced range (an anchor) maps to the start of its
// replacement.

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/syntactic_analysis"
	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// ─── positions ────────────────────────────────────────────────────────────────

// Pos is a source position: a file relative to the module root, a 1-based
// line and a 1-based byte column (the go/token convention body keys use).
type Pos struct {
	File      string
	Line, Col int
}

// Key is p's body key, "line:col".
func (p Pos) Key() string { return v2.LocalID(p.Line, p.Col) }

// lineTable holds the offset of each line start of a file.
type lineTable []int

func newLineTable(src []byte) lineTable {
	t := lineTable{0}
	for i, c := range src {
		if c == '\n' {
			t = append(t, i+1)
		}
	}
	return t
}

func (t lineTable) offset(line, col int) (int, bool) {
	if line < 1 || line > len(t) || col < 1 {
		return 0, false
	}
	return t[line-1] + col - 1, true
}

func (t lineTable) pos(off int) (int, int) {
	i := sort.Search(len(t), func(i int) bool { return t[i] > off }) - 1
	return i + 1, off - t[i] + 1
}

// seg maps old bytes [a, z) of a file to newFile at offset at (a copy), or,
// for an anchor, the single old offset a to at.
type seg struct {
	a, z    int
	newFile string
	at      int
}

// PosMap maps old positions to new ones. A file no transform rewrote maps to
// itself. In a rewritten file a position maps when its byte was copied or it
// is an anchor, and is unmapped (deleted or replaced text) otherwise. Every
// old byte is copied at most once, so the copies of a file are disjoint.
type PosMap struct {
	old     map[string]lineTable
	new     map[string]lineTable
	copies  map[string][]seg // by old file, sorted by a
	anchors map[string]map[int]seg
}

func newPosMap() *PosMap {
	return &PosMap{old: map[string]lineTable{}, new: map[string]lineTable{},
		copies: map[string][]seg{}, anchors: map[string]map[int]seg{}}
}

// Map returns p's new position.
func (m *PosMap) Map(p Pos) (Pos, bool) {
	lt, ok := m.old[p.File]
	if !ok {
		return p, true
	}
	off, ok := lt.offset(p.Line, p.Col)
	if !ok {
		return Pos{}, false
	}
	s, ok := m.anchors[p.File][off]
	if !ok {
		cs := m.copies[p.File]
		i := sort.Search(len(cs), func(i int) bool { return cs[i].a > off }) - 1
		if i < 0 || off >= cs[i].z {
			return Pos{}, false
		}
		s = cs[i]
	}
	l, c := m.new[s.newFile].pos(s.at + off - s.a)
	return Pos{File: s.newFile, Line: l, Col: c}, true
}

// ─── workspace ────────────────────────────────────────────────────────────────

// Workspace is a module loaded for transformation: every project file with
// its syntax, types and source.
type Workspace struct {
	Dir   string
	Fset  *token.FileSet
	Files map[string]*SrcFile // by path relative to Dir
}

// SrcFile is one project file.
type SrcFile struct {
	Rel   string
	Pkg   *packages.Package
	AST   *ast.File
	Src   []byte
	TF    *token.File
	lines lineTable
	// Skip is why the file is not eligible for transformation ("" when it is).
	Skip string
}

func (f *SrcFile) off(p token.Pos) int { return f.TF.Offset(p) }

// pos is the old position of p.
func (f *SrcFile) pos(p token.Pos) Pos {
	l, c := f.lines.pos(f.off(p))
	return Pos{File: f.Rel, Line: l, Col: c}
}

// key is the body key of p.
func (f *SrcFile) key(p token.Pos) string { return f.pos(p).Key() }

// LoadWorkspace loads the module at dir with the analyzer's go/packages mode
// (test files excluded, as at the CLI default).
func LoadWorkspace(dir string) (*Workspace, error) {
	dir, err := realDir(dir)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes |
			packages.NeedTypesInfo | packages.NeedImports | packages.NeedDeps,
		Dir: dir, Fset: fset,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		return nil, err
	}
	w := &Workspace{Dir: dir, Fset: fset, Files: map[string]*SrcFile{}}
	for _, p := range pkgs {
		for _, af := range p.Syntax {
			tf := fset.File(af.Pos())
			if tf == nil || !utils.IsWithin(dir, tf.Name()) {
				continue
			}
			rel := filepath.ToSlash(utils.RelativePath(dir, tf.Name()))
			if utils.IsTestFile(rel) || utils.IsVendored(rel) {
				continue
			}
			src, err := os.ReadFile(tf.Name())
			if err != nil {
				return nil, err
			}
			f := &SrcFile{Rel: rel, Pkg: p, AST: af, Src: src, TF: tf, lines: newLineTable(src)}
			f.Skip = skipReason(f)
			w.Files[rel] = f
		}
	}
	return w, nil
}

// skipReason is why f is not transformed: cgo, //line directives and
// generated code (spans follow other rules there), type errors in its
// package (the transforms need its types), or a source that does not match
// its parse.
func skipReason(f *SrcFile) string {
	switch {
	case len(f.Pkg.Errors) > 0:
		return "package has errors"
	case f.TF.Size() != len(f.Src):
		return "source differs from its parse"
	case ast.IsGenerated(f.AST):
		return "generated"
	case f.Pkg.TypesInfo == nil:
		return "no types"
	}
	for _, s := range f.AST.Imports {
		if s.Path.Value == `"C"` {
			return "cgo"
		}
	}
	for _, cg := range f.AST.Comments {
		for _, c := range cg.List {
			if strings.HasPrefix(c.Text, "//line ") || strings.HasPrefix(c.Text, "/*line ") {
				return "//line directive"
			}
		}
	}
	return ""
}

// Eligible returns the transformable files, sorted, restricted to the
// package directories in dirs when dirs is non-empty.
func (w *Workspace) Eligible(dirs ...string) []string {
	var out []string
	for rel, f := range w.Files {
		if f.Skip != "" {
			continue
		}
		if len(dirs) > 0 && !slices.Contains(dirs, filepath.ToSlash(filepath.Dir(rel))) {
			continue
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// SkipCounts counts the ineligible files by reason.
func (w *Workspace) SkipCounts() map[string]int {
	out := map[string]int{}
	for _, f := range w.Files {
		if f.Skip != "" {
			out[f.Skip]++
		}
	}
	return out
}

// Sample returns n of files chosen with seed (all when n <= 0 or n >= len).
func Sample(files []string, n int, seed uint64) []string {
	out := slices.Clone(files)
	sort.Strings(out)
	if n <= 0 || n >= len(out) {
		return out
	}
	r := rand.New(rand.NewPCG(seed, 0))
	r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	out = out[:n]
	sort.Strings(out)
	return out
}

// ─── transform driver ─────────────────────────────────────────────────────────

// Transform is one source transform, applied file by file. edit returns
// nil, or errSkip, to leave a file unchanged.
type Transform struct {
	Name string
	edit func(c *tctx, f *SrcFile) (*fileEdit, error)
}

// piece is a run of a new file's text: a literal, a copy of old bytes
// [a, z) of f, an anchor (old offset a of f maps to here; writes nothing)
// or a mark (records here under its name; writes nothing).
type piece struct {
	lit    string
	f      *SrcFile
	a, z   int
	anchor bool
	mark   string
}

func lit(s string) piece               { return piece{lit: s} }
func cp(f *SrcFile, a, z int) piece    { return piece{f: f, a: a, z: z} }
func anchorAt(f *SrcFile, a int) piece { return piece{f: f, a: a, z: a, anchor: true} }
func mark(name string) piece           { return piece{mark: name} }

type fileEdit struct {
	pieces  []piece   // the old file's new text
	created []newFile // files added next to it
	touched []string  // Identity of every callable changed
	sites   []Site
	// skips counts the sites declined, by reason.
	skips map[string]int
}

func (e *fileEdit) skip(reason string) {
	if e.skips == nil {
		e.skips = map[string]int{}
	}
	e.skips[reason]++
}

type newFile struct {
	rel    string
	pieces []piece
}

// Site is one place a transform changed, with what its relation expects.
type Site struct {
	Desc     string // file:line and what was done
	Callable string // Identity, in the original
	// Delta is the expected change (delta and precision relations).
	Delta *Delta
	// NoFlow lists vertices (refs into Callable) that no @formal_in of
	// Callable may reach over data edges after the transform (MR13).
	NoFlow []Ref
	// Reloc maps a call key of Callable in the original to its call node
	// after the transform, in RelocTo (MR14a: the call moved into a method).
	Reloc   map[string]Ref
	RelocTo string
	// Summary lists summary edges that must exist after the transform in
	// Callable, each the counterpart of one in the original (MR14a).
	Summary []SumCheck
	// Cut lists the Nodes of the original whose out-edges the reachability
	// oracle leaves out of the original's graph (MR14b, see there).
	Cut []string
}

// SumCheck is one expected summary edge.
type SumCheck struct {
	Pre      string // the original edge, for the message
	Src, Dst Ref
}

// tctx is the per-run state of a transform.
type tctx struct {
	w   *Workspace
	pre *View // the original's analysis (nil when the transform needs none)
	rng *rand.Rand
	n   int // a counter for fresh names and marks
}

func (c *tctx) fresh(prefix string) string {
	c.n++
	return prefix + strconv.Itoa(c.n)
}

// Result is a transformed module.
type Result struct {
	Dir     string  // the transformed copy's root
	Map     *PosMap // old → new positions
	Marks   map[string]Pos
	Files   []string // rewritten
	Created []string
	// Touched is the Identity of every callable a transform changed, sorted.
	Touched []string
	Sites   []Site
	// Skipped counts, per reason, the files and sites left unchanged.
	Skipped map[string]int
}

// errSkip is returned by an edit that leaves its file unchanged.
type errSkip string

func (e errSkip) Error() string { return string(e) }

// Apply copies the module to dst, applies t to files with seed and returns
// the result. pre is the original's analysis, which the delta transforms
// read to choose and describe their sites.
func (w *Workspace) Apply(t Transform, files []string, dst string, seed uint64, pre *View) (*Result, error) {
	if err := copyTree(w.Dir, dst); err != nil {
		return nil, err
	}
	dst, err := realDir(dst)
	if err != nil {
		return nil, err
	}
	c := &tctx{w: w, pre: pre, rng: rand.New(rand.NewPCG(seed, 1))}
	res := &Result{Dir: dst, Skipped: map[string]int{}, Map: newPosMap(), Marks: map[string]Pos{}}
	touched := map[string]bool{}
	for _, rel := range files {
		f := w.Files[rel]
		if f == nil || f.Skip != "" {
			return nil, fmt.Errorf("%s: not an eligible file", rel)
		}
		fe, err := t.edit(c, f)
		var skip errSkip
		if errors.As(err, &skip) {
			res.Skipped["file: "+string(skip)]++
			if fe != nil {
				for r, n := range fe.skips {
					res.Skipped["site: "+r] += n
				}
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w", t.Name, rel, err)
		}
		if fe == nil || fe.pieces == nil {
			res.Skipped["file: no site"]++
			continue
		}
		for r, n := range fe.skips {
			res.Skipped["site: "+r] += n
		}
		if err := res.write(dst, rel, fe.pieces); err != nil {
			return nil, err
		}
		res.Files = append(res.Files, rel)
		for _, nf := range fe.created {
			if _, err := os.Stat(filepath.Join(dst, nf.rel)); err == nil {
				return nil, fmt.Errorf("%s: %s exists", t.Name, nf.rel)
			}
			if err := res.write(dst, nf.rel, nf.pieces); err != nil {
				return nil, err
			}
			res.Created = append(res.Created, nf.rel)
		}
		for _, id := range fe.touched {
			touched[id] = true
		}
		res.Sites = append(res.Sites, fe.sites...)
	}
	res.Touched = slices.Sorted(func(yield func(string) bool) {
		for id := range touched {
			if !yield(id) {
				return
			}
		}
	})
	for _, s := range res.Map.copies {
		sort.Slice(s, func(i, j int) bool { return s[i].a < s[j].a })
	}
	return res, nil
}

// write assembles pieces into dst/rel and records their segments and marks.
func (r *Result) write(dst, rel string, ps []piece) error {
	var buf bytes.Buffer
	marks := map[string]int{}
	for _, p := range ps {
		switch {
		case p.mark != "":
			if _, dup := r.Marks[p.mark]; dup {
				return fmt.Errorf("%s: duplicate mark %s", rel, p.mark)
			}
			marks[p.mark] = buf.Len()
			continue
		case p.f == nil:
			buf.WriteString(p.lit)
			continue
		case p.a < 0 || p.z > len(p.f.Src) || p.a > p.z:
			return fmt.Errorf("%s: bad piece [%d,%d) of %s", rel, p.a, p.z, p.f.Rel)
		}
		old := p.f.Rel
		if _, ok := r.Map.old[old]; !ok {
			r.Map.old[old] = p.f.lines
			r.Map.anchors[old] = map[int]seg{}
		}
		s := seg{a: p.a, z: p.z, newFile: rel, at: buf.Len()}
		if p.anchor {
			r.Map.anchors[old][p.a] = s
			continue
		}
		if p.z > p.a {
			r.Map.copies[old] = append(r.Map.copies[old], s)
			buf.Write(p.f.Src[p.a:p.z])
		}
	}
	lt := newLineTable(buf.Bytes())
	r.Map.new[rel] = lt
	for name, off := range marks {
		l, c := lt.pos(off)
		r.Marks[name] = Pos{File: rel, Line: l, Col: c}
	}
	return os.WriteFile(filepath.Join(dst, rel), buf.Bytes(), 0o644)
}

// copyTree copies the regular files under src to dst, skipping VCS and
// JavaScript dependency trees.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if d.IsDir() {
			if n := d.Name(); rel != "." && (n == ".git" || n == "node_modules") {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
}

// edit is one change to a file: old bytes [a, z) replaced by out. With
// anchor, old offset a maps to the start of the replacement.
type edit struct {
	a, z   int
	out    []piece
	anchor bool
}

func insert(at int, out ...piece) edit { return edit{a: at, z: at, out: out} }

// applyEdits is f's text with edits applied (they must not overlap; an
// insertion at an offset comes before a replacement starting there).
func applyEdits(f *SrcFile, edits []edit) ([]piece, error) {
	sort.SliceStable(edits, func(i, j int) bool {
		if edits[i].a != edits[j].a {
			return edits[i].a < edits[j].a
		}
		return edits[i].z < edits[j].z
	})
	var ps []piece
	cur := 0
	for _, e := range edits {
		if e.a < cur {
			return nil, fmt.Errorf("overlapping edits at offset %d", e.a)
		}
		if e.a > cur {
			ps = append(ps, cp(f, cur, e.a))
		}
		if e.anchor {
			ps = append(ps, anchorAt(f, e.a))
		}
		ps = append(ps, e.out...)
		cur = e.z
	}
	if cur < len(f.Src) {
		ps = append(ps, cp(f, cur, len(f.Src)))
	}
	return ps, nil
}

// ─── shared helpers ───────────────────────────────────────────────────────────

func identityOf(sig, file string) string { return sig + "#" + file }

func lineStart(src []byte, off int) int {
	return bytes.LastIndexByte(src[:off], '\n') + 1
}

// lineEnd is the offset just past the '\n' ending the line holding off
// (len(src) on the last line).
func lineEnd(src []byte, off int) int {
	if i := bytes.IndexByte(src[off:], '\n'); i >= 0 {
		return off + i + 1
	}
	return len(src)
}

// blankTail reports whether src[off:] up to the end of its line is blank or
// a line comment.
func blankTail(src []byte, off int) bool {
	rest := strings.TrimLeft(string(src[off:lineEnd(src, off)]), " \t\r\n")
	return rest == "" || strings.HasPrefix(rest, "//")
}

func blankHead(src []byte, off int) bool {
	return strings.TrimLeft(string(src[lineStart(src, off):off]), " \t") == ""
}

func declDoc(d ast.Decl) *ast.CommentGroup {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return d.Doc
	case *ast.GenDecl:
		return d.Doc
	}
	return nil
}

// chunk is the whole lines of decl i of f with its doc comment: [a, z). ok
// is false when another declaration shares one of those lines.
func chunk(f *SrcFile, i int) (a, z int, ok bool) {
	d := f.AST.Decls[i]
	start := d.Pos()
	if doc := declDoc(d); doc != nil {
		start = doc.Pos()
	}
	so, eo := f.off(start), f.off(d.End())
	if !blankHead(f.Src, so) || !blankTail(f.Src, eo) {
		return 0, 0, false
	}
	a, z = lineStart(f.Src, so), lineEnd(f.Src, eo)
	if i > 0 && f.off(f.AST.Decls[i-1].End()) > a {
		return 0, 0, false
	}
	if i+1 < len(f.AST.Decls) {
		n := f.AST.Decls[i+1]
		ns := n.Pos()
		if doc := declDoc(n); doc != nil {
			ns = doc.Pos()
		}
		if f.off(ns) < z {
			return 0, 0, false
		}
	}
	return a, z, true
}

func isImport(d ast.Decl) bool {
	g, ok := d.(*ast.GenDecl)
	return ok && g.Tok == token.IMPORT
}

// funcIdentity is the comparer's Identity of FuncDecl fd of f.
func funcIdentity(f *SrcFile, fd *ast.FuncDecl) string {
	obj := f.Pkg.TypesInfo.Defs[fd.Name]
	if obj == nil {
		return ""
	}
	sig := syntactic_analysis.SignatureOf(obj)
	if fd.Recv == nil && fd.Name.Name == "init" {
		return identityOf(sig, f.Rel)
	}
	return sig
}

// localVar is obj as a function-local variable: a parameter, result,
// receiver or local, not a field or a package-level variable.
func localVar(f *SrcFile, obj types.Object) (*types.Var, bool) {
	v, ok := obj.(*types.Var)
	if !ok || v.IsField() || v.Pkg() != f.Pkg.Types || v.Parent() == nil || v.Parent() == v.Pkg().Scope() {
		return nil, false
	}
	return v, true
}

func contains[T ast.Node](n ast.Node, pred func(T) bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if x, ok := n.(T); ok && pred(x) {
			found = true
		}
		return !found
	})
	return found
}

func hasGoto(n ast.Node) bool {
	return contains(n, func(b *ast.BranchStmt) bool { return b.Tok == token.GOTO })
}

// usesPrefix reports whether any identifier of f's package starts with prefix.
func usesPrefix(f *SrcFile, prefix string) bool {
	for _, af := range f.Pkg.Syntax {
		if contains(af, func(id *ast.Ident) bool { return strings.HasPrefix(id.Name, prefix) }) {
			return true
		}
	}
	return false
}

func funcDecls(f *SrcFile) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, d := range f.AST.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok {
			out = append(out, fd)
		}
	}
	return out
}

// stmtLists calls fn with every statement list of body outside function
// literals: blocks, and case and comm clause bodies.
func stmtLists(body *ast.BlockStmt, fn func([]ast.Stmt)) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.BlockStmt:
			fn(n.List)
		case *ast.CaseClause:
			fn(n.Body)
		case *ast.CommClause:
			fn(n.Body)
		}
		return true
	})
}

// ─── MR1: blank lines and comments ────────────────────────────────────────────

// MR1 inserts a comment and blank lines at the top of each file and before
// every top-level declaration.
var MR1 = Transform{Name: "MR1", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	edits := []edit{insert(0, lit("// mr1: inserted at the top\n\n"))}
	prevEnd := 0
	n := 0
	for _, d := range f.AST.Decls {
		start := d.Pos()
		if doc := declDoc(d); doc != nil {
			start = doc.Pos()
		}
		at := lineStart(f.Src, f.off(start))
		if at >= prevEnd && at > 0 {
			edits = append(edits, insert(at, lit("\n// mr1: inserted between declarations\n\n\n")))
			n++
		}
		prevEnd = f.off(d.End())
	}
	ps, err := applyEdits(f, edits)
	return &fileEdit{pieces: ps, sites: []Site{{Desc: fmt.Sprintf("%s: %d insertions", f.Rel, n+1)}}}, err
}}

// ─── MR2: reverse the top-level declarations ──────────────────────────────────

// MR2 reverses the order of a file's top-level declarations after its
// imports, so methods land above their types. Each declaration moves with its
// doc comment and the comments before it.
var MR2 = Transform{Name: "MR2", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	decls := f.AST.Decls
	first := 0
	for first < len(decls) && isImport(decls[first]) {
		first++
	}
	if len(decls)-first < 2 {
		return nil, errSkip("fewer than 2 declarations")
	}
	header := lineEnd(f.Src, f.off(f.AST.Name.End()))
	if first > 0 {
		header = lineEnd(f.Src, f.off(decls[first-1].End()))
	}
	// Chunk i runs from the end of decl i-1's last line to the end of decl
	// i's last line: it holds the comments before decl i.
	var bounds []int
	for i := first; i < len(decls); i++ {
		if _, _, ok := chunk(f, i); !ok {
			return nil, errSkip("declarations share a line")
		}
		bounds = append(bounds, lineEnd(f.Src, f.off(decls[i].End())))
	}
	bounds[len(bounds)-1] = len(f.Src)
	ps := []piece{cp(f, 0, header)}
	for i := len(bounds) - 1; i >= 0; i-- {
		a := header
		if i > 0 {
			a = bounds[i-1]
		}
		ps = append(ps, cp(f, a, bounds[i]))
		if !bytes.HasSuffix(f.Src[a:bounds[i]], []byte("\n")) {
			ps = append(ps, lit("\n"))
		}
	}
	var touched []string
	for _, fd := range funcDecls(f) {
		touched = append(touched, funcIdentity(f, fd))
	}
	return &fileEdit{pieces: ps, touched: touched,
		sites: []Site{{Desc: fmt.Sprintf("%s: %d declarations reversed", f.Rel, len(bounds))}}}, nil
}}

// ─── MR3: move a function and a method to a new file ──────────────────────────

// MR3 moves one top-level function and one method of a file (never an init)
// into a new file of the same package, which imports what they use. An
// import of the old file that only they used becomes a blank import, so the
// old file keeps its lines and still type-checks.
var MR3 = Transform{Name: "MR3", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	info := f.Pkg.TypesInfo
	for _, s := range f.AST.Imports {
		if s.Name != nil && s.Name.Name == "." {
			return nil, errSkip("dot import")
		}
	}
	var fns, meths []int
	for i, d := range f.AST.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name == "init" || fd.Name.Name == "_" {
			continue
		}
		if _, _, ok := chunk(f, i); !ok {
			continue
		}
		if fd.Recv == nil {
			fns = append(fns, i)
		} else {
			meths = append(meths, i)
		}
	}
	var move []int
	if len(fns) > 0 {
		move = append(move, fns[c.rng.IntN(len(fns))])
	}
	if len(meths) > 0 {
		move = append(move, meths[c.rng.IntN(len(meths))])
	}
	if len(move) == 0 {
		return nil, errSkip("nothing to move")
	}
	slices.Sort(move)

	pkgUses := func(n ast.Node) map[*types.PkgName]bool {
		out := map[*types.PkgName]bool{}
		ast.Inspect(n, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if pn, ok := info.Uses[id].(*types.PkgName); ok {
					out[pn] = true
				}
			}
			return true
		})
		return out
	}
	moved, kept := map[*types.PkgName]bool{}, map[*types.PkgName]bool{}
	for i, d := range f.AST.Decls {
		dst := kept
		if slices.Contains(move, i) {
			dst = moved
		}
		for pn := range pkgUses(d) {
			dst[pn] = true
		}
	}

	var edits []edit
	var imports []string
	for _, s := range f.AST.Imports {
		pn := importName(info, s)
		if pn == nil || pn.Name() == "_" {
			continue
		}
		if moved[pn] {
			imports = append(imports, pn.Name()+" "+s.Path.Value)
		}
		if !kept[pn] {
			edits = append(edits, edit{a: f.off(s.Pos()), z: f.off(s.End()), out: []piece{lit("_ " + s.Path.Value)}})
		}
	}
	created := []piece{lit("package " + f.AST.Name.Name + "\n\n")}
	if len(imports) > 0 {
		sort.Strings(imports)
		created = append(created, lit("import (\n\t"+strings.Join(imports, "\n\t")+"\n)\n"))
	}
	base := strings.TrimSuffix(filepath.Base(f.Rel), ".go") + "_mr3.go"
	rel := filepath.ToSlash(filepath.Join(filepath.Dir(f.Rel), base))
	var touched []string
	var sites []Site
	for _, i := range move {
		a, z, _ := chunk(f, i)
		edits = append(edits, edit{a: a, z: z})
		created = append(created, lit("\n"), cp(f, a, z))
		if !bytes.HasSuffix(f.Src[a:z], []byte("\n")) {
			created = append(created, lit("\n"))
		}
		fd := f.AST.Decls[i].(*ast.FuncDecl)
		id := funcIdentity(f, fd)
		touched = append(touched, id)
		sites = append(sites, Site{Desc: fmt.Sprintf("%s:%d: %s moved to %s", f.Rel, f.pos(fd.Pos()).Line, fd.Name.Name, rel), Callable: id})
	}
	ps, err := applyEdits(f, edits)
	if err != nil {
		return nil, err
	}
	return &fileEdit{pieces: ps, touched: touched, sites: sites, created: []newFile{{rel: rel, pieces: created}}}, nil
}}

// importName is the package name import spec s declares in its file.
func importName(info *types.Info, s *ast.ImportSpec) *types.PkgName {
	if s.Name != nil {
		pn, _ := info.Defs[s.Name].(*types.PkgName)
		return pn
	}
	pn, _ := info.Implicits[s].(*types.PkgName)
	return pn
}

// ─── MR4: alpha-rename locals ─────────────────────────────────────────────────

// Suffix4 is the suffix MR4 appends to every local variable and parameter
// name. A file whose package already has an identifier containing it is
// skipped, so the comparer can undo the rename by stripping it.
const Suffix4 = "_mr4"

// MR4 renames every local variable, parameter, named result and receiver of
// the file's functions (closures included) to name+Suffix4.
var MR4 = Transform{Name: "MR4", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	for _, af := range f.Pkg.Syntax {
		if contains(af, func(id *ast.Ident) bool { return strings.Contains(id.Name, Suffix4) }) {
			return nil, errSkip("name clash")
		}
	}
	info := f.Pkg.TypesInfo
	var edits []edit
	var touched []string
	n := 0
	rename := func(id *ast.Ident) {
		edits = append(edits, edit{a: f.off(id.Pos()), z: f.off(id.End()), anchor: true, out: []piece{lit(id.Name + Suffix4)}})
		n++
	}
	for _, fd := range funcDecls(f) {
		before := n
		ast.Inspect(fd, func(nd ast.Node) bool {
			switch x := nd.(type) {
			case *ast.TypeSwitchStmt:
				// `switch v := x.(type)`: v has no object of its own; each
				// clause has an implicit one, which its uses resolve to.
				if as, ok := x.Assign.(*ast.AssignStmt); ok {
					if id, ok := as.Lhs[0].(*ast.Ident); ok && id.Name != "_" {
						rename(id)
					}
				}
			case *ast.Ident:
				if x.Name == "_" {
					return true
				}
				obj := info.Defs[x]
				if obj == nil {
					obj = info.Uses[x]
				}
				if _, ok := localVar(f, obj); ok {
					rename(x)
				}
			}
			return true
		})
		if n > before {
			touched = append(touched, funcIdentity(f, fd))
		}
	}
	if len(edits) == 0 {
		return nil, errSkip("no locals")
	}
	ps, err := applyEdits(f, edits)
	return &fileEdit{pieces: ps, touched: touched,
		sites: []Site{{Desc: fmt.Sprintf("%s: %d identifiers renamed", f.Rel, n)}}}, err
}}

// ─── MR5: introduce an alias ──────────────────────────────────────────────────

// MR5 rewrites each `x.f = e` of the file, where x is a local and f a direct
// field of it, as `q := x; q.f = e` when x is a pointer to a struct, or as
// `q := &x; q.f = e` when x is a struct value. q is fresh. The write now goes
// through an alias of x. Other bases are left alone: a value copy does not
// alias.
var MR5 = Transform{Name: "MR5", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	if usesPrefix(f, "mr5q") {
		return nil, errSkip("name clash")
	}
	info := f.Pkg.TypesInfo
	fe := &fileEdit{}
	var edits []edit
	for _, fd := range funcDecls(f) {
		if fd.Body == nil || hasGoto(fd.Body) {
			continue
		}
		before := len(fe.sites)
		stmtListsAll(fd.Body, func(list []ast.Stmt) {
			for _, s := range list {
				as, ok := s.(*ast.AssignStmt)
				if !ok || as.Tok != token.ASSIGN || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					continue
				}
				sel, ok := as.Lhs[0].(*ast.SelectorExpr)
				if !ok {
					continue
				}
				x, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				v, ok := localVar(f, info.Uses[x])
				if !ok {
					continue
				}
				if s := info.Selections[sel]; s == nil || s.Kind() != types.FieldVal || len(s.Index()) != 1 {
					continue
				}
				var rhs string
				switch t := v.Type().(type) {
				case *types.Pointer:
					if _, ok := t.Elem().Underlying().(*types.Struct); !ok {
						fe.skip("MR5: not a struct pointer")
						continue
					}
					rhs = x.Name
				case *types.Named:
					if _, ok := t.Underlying().(*types.Struct); !ok {
						fe.skip("MR5: not a struct")
						continue
					}
					rhs = "&" + x.Name
				default:
					fe.skip("MR5: not a struct or a pointer to one")
					continue
				}
				q := c.fresh("mr5q")
				at := f.off(as.Pos())
				edits = append(edits,
					insert(at, lit(q+" := "+rhs+"; ")),
					edit{a: at, z: f.off(x.End()), anchor: true, out: []piece{lit(q)}})
				fe.sites = append(fe.sites, Site{Desc: fmt.Sprintf("%s:%d: %s via %s", f.Rel, f.lines.lineOf(at), x.Name, q), Callable: funcIdentity(f, fd)})
			}
		})
		if len(fe.sites) > before {
			fe.touched = append(fe.touched, funcIdentity(f, fd))
		}
	}
	if len(edits) == 0 {
		return fe, errSkip("no field write")
	}
	ps, err := applyEdits(f, edits)
	fe.pieces = ps
	return fe, err
}}

// stmtListsAll is stmtLists including function literal bodies.
func stmtListsAll(body *ast.BlockStmt, fn func([]ast.Stmt)) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.BlockStmt:
			fn(n.List)
		case *ast.CaseClause:
			fn(n.Body)
		case *ast.CommClause:
			fn(n.Body)
		}
		return true
	})
}

// ─── MR6: extract a helper ────────────────────────────────────────────────────

// Prefix6 starts every MR6 helper's name.
const Prefix6 = "mr6h_"

// MR6 extracts, in each function of the file, one straight-line run of 2-4
// statements into a new top-level helper at the end of the file. The helper
// takes every outer local the run names and returns them (in-out, so a
// write to a copy is assigned back), then the locals the run declares that
// the function uses after it:
//
//	var n1 T1; o1, o2, n1 = mr6h_f_1(o1, o2)
//
// A run qualifies when:
//   - its statements are simple (assignment, expression, ++/--, send, var
//     declaration) and hold no function literal or recover();
//   - it names no outer local that a closure captures or that is
//     address-taken anywhere in the function (copy-in/copy-out would then
//     change what the program does);
//   - it names no function-local type or constant;
//   - every type in the helper's signature can be spelled in the file.
//
// Generic functions and functions with a goto are skipped.
var MR6 = Transform{Name: "MR6", edit: func(c *tctx, f *SrcFile) (*fileEdit, error) {
	if usesPrefix(f, Prefix6) {
		return nil, errSkip("name clash")
	}
	fe := &fileEdit{}
	var edits []edit
	var helpers []piece
	for _, fd := range funcDecls(f) {
		if fd.Body == nil || fd.Type.TypeParams != nil || hasGoto(fd.Body) || recvGeneric(fd) {
			continue
		}
		runs := mr6Runs(f, fd)
		if len(runs) == 0 {
			continue
		}
		r := runs[c.rng.IntN(len(runs))]
		name := c.fresh(Prefix6 + fd.Name.Name + "_")
		var params, results, args, outs []string
		for _, v := range r.outer {
			params = append(params, v.v.Name()+" "+v.t)
			results = append(results, v.t)
			args = append(args, v.v.Name())
			outs = append(outs, v.v.Name())
		}
		var decl string
		for _, v := range r.fresh {
			decl += "var " + v.v.Name() + " " + v.t + "; "
			results = append(results, v.t)
			outs = append(outs, v.v.Name())
		}
		call := name + "(" + strings.Join(args, ", ") + ")"
		if len(outs) > 0 {
			call = decl + strings.Join(outs, ", ") + " = " + call
		}
		edits = append(edits, edit{a: r.a, z: r.z, out: []piece{lit(call)}})
		head := "\n\n// " + name + " is extracted by MR6.\nfunc " + name + "(" + strings.Join(params, ", ") + ")"
		if len(results) > 0 {
			head += " (" + strings.Join(results, ", ") + ")"
		}
		helpers = append(helpers, lit(head+" {\n\t"), cp(f, r.a, r.z))
		if len(outs) > 0 {
			helpers = append(helpers, lit("\n\treturn "+strings.Join(outs, ", ")))
		}
		helpers = append(helpers, lit("\n}\n"))
		id := funcIdentity(f, fd)
		fe.touched = append(fe.touched, id)
		fe.sites = append(fe.sites, Site{Desc: fmt.Sprintf("%s:%d: %d statements into %s", f.Rel, f.lines.lineOf(r.a), r.n, name), Callable: id})
	}
	if len(edits) == 0 {
		return fe, errSkip("no run")
	}
	edits = append(edits, insert(len(f.Src), helpers...))
	ps, err := applyEdits(f, edits)
	fe.pieces = ps
	return fe, err
}}

func (t lineTable) lineOf(off int) int {
	l, _ := t.pos(off)
	return l
}

func recvGeneric(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return false
	}
	t := fd.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	switch t.(type) {
	case *ast.IndexExpr, *ast.IndexListExpr:
		return true
	}
	return false
}

type runVar struct {
	v *types.Var
	t string // the type, spelled in the file
}

type run struct {
	a, z  int
	n     int
	outer []runVar // outer locals named, by first use
	fresh []runVar // locals declared in the run and used after it
}

// mr6Runs returns the qualifying runs of fd (see MR6).
func mr6Runs(f *SrcFile, fd *ast.FuncDecl) []run {
	info := f.Pkg.TypesInfo
	pinned := pinnedVars(f, fd.Body)
	var out []run
	stmtLists(fd.Body, func(list []ast.Stmt) {
		for i := range list {
			for n := 2; n <= 4 && i+n <= len(list); n++ {
				w := list[i : i+n]
				if slices.ContainsFunc(w, func(s ast.Stmt) bool { return !simpleStmt(info, s) }) {
					break
				}
				if r, ok := makeRun(f, fd, w, pinned); ok {
					out = append(out, r)
				}
			}
		}
	})
	return out
}

func simpleStmt(info *types.Info, s ast.Stmt) bool {
	switch s := s.(type) {
	case *ast.AssignStmt, *ast.ExprStmt, *ast.IncDecStmt, *ast.SendStmt:
	case *ast.DeclStmt:
		if g, ok := s.Decl.(*ast.GenDecl); !ok || g.Tok != token.VAR {
			return false
		}
	default:
		return false
	}
	return !contains(s, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.FuncLit:
			return true
		case *ast.Ident:
			b, isB := info.Uses[n].(*types.Builtin)
			return isB && b.Name() == "recover"
		}
		return false
	})
}

// pinnedVars are the locals of body whose identity matters beyond their
// value: captured by a function literal, or address-taken (&x, an array
// sliced, a pointer-receiver method called on an addressable value).
func pinnedVars(f *SrcFile, body ast.Node) map[*types.Var]bool {
	info := f.Pkg.TypesInfo
	out := map[*types.Var]bool{}
	base := func(e ast.Expr) *types.Var {
		for {
			switch x := e.(type) {
			case *ast.ParenExpr:
				e = x.X
			case *ast.SelectorExpr:
				if s := info.Selections[x]; s != nil && s.Indirect() {
					return nil // through a pointer: not the variable itself
				}
				e = x.X
			case *ast.IndexExpr:
				if _, isArr := info.TypeOf(x.X).Underlying().(*types.Array); !isArr {
					return nil
				}
				e = x.X
			case *ast.Ident:
				v, _ := localVar(f, info.Uses[x])
				return v
			default:
				return nil
			}
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncLit:
			ast.Inspect(x.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok {
					if v, ok := localVar(f, info.Uses[id]); ok && (v.Pos() < x.Pos() || v.Pos() >= x.End()) {
						out[v] = true
					}
				}
				return true
			})
		case *ast.UnaryExpr:
			if x.Op == token.AND {
				if v := base(x.X); v != nil {
					out[v] = true
				}
			}
		case *ast.SliceExpr:
			if t := info.TypeOf(x.X); t != nil {
				if _, isArr := t.Underlying().(*types.Array); isArr {
					if v := base(x.X); v != nil {
						out[v] = true
					}
				}
			}
		case *ast.SelectorExpr:
			if s := info.Selections[x]; s != nil && s.Kind() == types.MethodVal {
				if sig, ok := s.Obj().Type().(*types.Signature); ok && sig.Recv() != nil {
					if _, ptrRecv := sig.Recv().Type().(*types.Pointer); ptrRecv {
						if v := base(x.X); v != nil {
							out[v] = true
						}
					}
				}
			}
		}
		return true
	})
	return out
}

func makeRun(f *SrcFile, fd *ast.FuncDecl, w []ast.Stmt, pinned map[*types.Var]bool) (run, bool) {
	info := f.Pkg.TypesInfo
	r := run{a: f.off(w[0].Pos()), z: f.off(w[len(w)-1].End()), n: len(w)}
	lo, hi := w[0].Pos(), w[len(w)-1].End()
	seen := map[*types.Var]bool{}
	var fresh []*types.Var
	ok := true
	for _, s := range w {
		ast.Inspect(s, func(n ast.Node) bool {
			id, isID := n.(*ast.Ident)
			if !isID || !ok {
				return ok
			}
			obj := info.Uses[id]
			if obj == nil {
				obj = info.Defs[id]
			}
			if obj == nil {
				return true
			}
			// A function-local type, constant or function name would not be
			// in scope in the helper.
			if _, isVar := obj.(*types.Var); !isVar {
				if _, isPkg := obj.(*types.PkgName); !isPkg && obj.Pkg() == f.Pkg.Types &&
					obj.Parent() != nil && obj.Parent() != f.Pkg.Types.Scope() {
					ok = false
				}
				return true
			}
			v, local := localVar(f, obj)
			if !local || seen[v] {
				return true
			}
			seen[v] = true
			if v.Pos() >= lo && v.Pos() < hi {
				fresh = append(fresh, v)
				return true
			}
			if pinned[v] {
				ok = false
				return false
			}
			t, spelled := spell(f, v.Type())
			if !spelled {
				ok = false
				return false
			}
			r.outer = append(r.outer, runVar{v, t})
			return true
		})
	}
	if !ok || len(r.outer) == 0 {
		return run{}, false
	}
	names := map[string]bool{}
	for _, v := range r.outer {
		if names[v.v.Name()] {
			return run{}, false
		}
		names[v.v.Name()] = true
	}
	for _, v := range fresh {
		if names[v.Name()] {
			return run{}, false
		}
		if !usedAfter(info, fd, v, hi) {
			continue
		}
		t, spelled := spell(f, v.Type())
		if !spelled {
			return run{}, false
		}
		r.fresh = append(r.fresh, runVar{v, t})
	}
	return r, true
}

func usedAfter(info *types.Info, fd *ast.FuncDecl, v *types.Var, after token.Pos) bool {
	return contains(fd.Body, func(id *ast.Ident) bool { return id.Pos() >= after && info.Uses[id] == v })
}

// spell is t written in f, package-qualified by f's import names. It fails
// for a type f cannot name: an unexported or function-local named type, a
// type parameter, or a package f does not import.
func spell(f *SrcFile, t types.Type) (string, bool) {
	names := map[string]string{}
	for _, s := range f.AST.Imports {
		if pn := importName(f.Pkg.TypesInfo, s); pn != nil && pn.Name() != "_" && pn.Name() != "." {
			names[pn.Imported().Path()] = pn.Name()
		}
	}
	ok := true
	named := func(o *types.TypeName) {
		if o.Pkg() == nil {
			return
		}
		if o.Parent() != o.Pkg().Scope() || (o.Pkg() != f.Pkg.Types && (!o.Exported() || names[o.Pkg().Path()] == "")) {
			ok = false
		}
	}
	var check func(t types.Type, depth int)
	check = func(t types.Type, depth int) {
		if !ok || depth > 20 {
			ok = false
			return
		}
		switch t := t.(type) {
		case *types.Named:
			named(t.Obj())
			if args := t.TypeArgs(); args != nil {
				for i := range args.Len() {
					check(args.At(i), depth+1)
				}
			}
		case *types.Alias:
			named(t.Obj())
		case *types.TypeParam, *types.Tuple:
			ok = false
		case *types.Pointer:
			check(t.Elem(), depth+1)
		case *types.Slice:
			check(t.Elem(), depth+1)
		case *types.Array:
			check(t.Elem(), depth+1)
		case *types.Chan:
			check(t.Elem(), depth+1)
		case *types.Map:
			check(t.Key(), depth+1)
			check(t.Elem(), depth+1)
		case *types.Signature:
			for _, tup := range []*types.Tuple{t.Params(), t.Results()} {
				for i := range tup.Len() {
					check(tup.At(i).Type(), depth+1)
				}
			}
		case *types.Struct:
			for i := range t.NumFields() {
				if !t.Field(i).Exported() && t.Field(i).Pkg() != f.Pkg.Types {
					ok = false
				}
				check(t.Field(i).Type(), depth+1)
			}
		case *types.Interface:
			for i := range t.NumExplicitMethods() {
				check(t.ExplicitMethod(i).Type(), depth+1)
			}
			for i := range t.NumEmbeddeds() {
				check(t.EmbeddedType(i), depth+1)
			}
		case *types.Basic:
			if t.Info()&types.IsUntyped != 0 || t.Kind() == types.UnsafePointer {
				ok = false
			}
		}
	}
	check(t, 0)
	if !ok {
		return "", false
	}
	return types.TypeString(t, func(p *types.Package) string {
		if p == f.Pkg.Types {
			return ""
		}
		return names[p.Path()]
	}), true
}
