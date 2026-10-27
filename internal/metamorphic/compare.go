package metamorphic

// The normalized view of an analysis and the three oracles over it.
//
// A View keys callables by Identity: the signature, which stays the same when
// a declaration moves between files (MR3), plus the declaring file for the
// per-file `init` and its closures. It does not key them by can:// id, which
// names the module. Body keys are local ("l:c[/n][/actual_*]", "@entry", …).
// Edges are strings "kind|src|dst|label[|prov]" over local keys. The
// application edges (call_graph, param_in, param_out) are over Node strings
// "identity|key".
//
// Map carries a view through a transform's position map into the
// transformed module's coordinates, so the oracles compare like with like:
//
//   - Exact: the mapped original equals the transformed view.
//   - Delta: the mapped original, with each expected Delta applied, equals
//     the transformed view. So after − before and before − after are exactly
//     the expected additions and removals, and nothing outside the delta's
//     callable changed.
//   - Reach: every sink reachable from a sampled source in the original is
//     still reachable, through the mapped nodes, in the transformed view. The
//     check is one-way: over-approximation may add flows.

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math/bits"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// View is the normalized analysis.
type View struct {
	Calls map[string]*CV
	// App holds "cg|src|dst|prov" over identities, and "pin|src|dst|var" /
	// "pout|src|dst|var" over Node strings.
	App map[string]bool

	callers map[string]bool // lazily, by called
}

// CV is one callable (or a library stand-in, Kind "external").
type CV struct {
	Ident string
	File  string // declaring file (source_file, else the module)
	Line  int    // the declaration's first line
	Kind  string
	L3    bool // has a cfg (an L3 result)
	// Body maps each body key to "kind|parent|of|flags|callee".
	Body  map[string]string
	Edges map[string]bool
}

func (c *CV) clone() *CV {
	n := *c
	n.Body, n.Edges = maps.Clone(c.Body), maps.Clone(c.Edges)
	return &n
}

// Node is the application-scope name of body key key of callable ident.
func Node(ident, key string) string { return ident + "|" + key }

func splitNode(n string) (string, string) {
	i := strings.LastIndexByte(n, '|')
	return n[:i], n[i+1:]
}

// externalIdent is the Identity of an @external id: "@ext:" + its tail.
func externalIdent(id string) (string, bool) {
	i := strings.Index(id, "/@external/")
	if i < 0 {
		return "", false
	}
	return "@ext:" + id[i+len("/@external/"):], true
}

// NodeAttr is the body attribute string of a node of kind with no parent,
// `of`, flags or callee.
func NodeAttr(kind string) string { return kind + "||||" }

// Collect normalizes an analysis.json stream, decoding one module at a
// time. Its application fields come in json.Marshal order: symbol_table
// before call_graph, external_symbols, param_in and param_out, so every
// callable id is known before an edge names it.
func Collect(r io.Reader) (*View, error) {
	dec := json.NewDecoder(r)
	v := &View{Calls: map[string]*CV{}, App: map[string]bool{}}
	ids := map[string]string{}
	callee := map[*CV]map[string]string{}
	delim := func(want json.Delim) error {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); !ok || d != want {
			return fmt.Errorf("collect: got %v, want %v", t, want)
		}
		return nil
	}
	skip := func() error {
		var raw json.RawMessage
		return dec.Decode(&raw)
	}
	if err := delim('{'); err != nil {
		return nil, err
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if t != "application" {
			if err := skip(); err != nil {
				return nil, err
			}
			continue
		}
		if err := delim('{'); err != nil {
			return nil, err
		}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return nil, err
			}
			switch k {
			case "symbol_table":
				if err := delim('{'); err != nil {
					return nil, err
				}
				for dec.More() {
					rel, err := dec.Token()
					if err != nil {
						return nil, err
					}
					var m v2.Module
					if err := dec.Decode(&m); err != nil {
						return nil, err
					}
					v.addModule(rel.(string), &m, ids, callee)
				}
				if err := delim('}'); err != nil {
					return nil, err
				}
			case "call_graph":
				var es []v2.Edge
				if err := dec.Decode(&es); err != nil {
					return nil, err
				}
				for _, e := range es {
					v.App["cg|"+resolveID(ids, e.Src)+"|"+resolveID(ids, e.Dst)+"|"+strings.Join(e.Prov, ",")] = true
				}
			case "external_symbols":
				var ext map[string]v2.ExternalSymbol
				if err := dec.Decode(&ext); err != nil {
					return nil, err
				}
				for id, e := range ext {
					ident, _ := externalIdent(id)
					c := &CV{Ident: ident, Kind: "external", Body: map[string]string{}, Edges: map[string]bool{}}
					for key, n := range e.Body {
						c.Body[key] = n.Kind + "||" + n.Of + "||"
					}
					v.Calls[ident] = c
				}
			case "param_in", "param_out":
				tag := "pin"
				if k == "param_out" {
					tag = "pout"
				}
				if err := delim('['); err != nil {
					return nil, err
				}
				for dec.More() {
					var e v2.ParamEdge
					if err := dec.Decode(&e); err != nil {
						return nil, err
					}
					v.App[tag+"|"+globalNode(ids, e.Src)+"|"+globalNode(ids, e.Dst)+"|"+e.Var] = true
				}
				if err := delim(']'); err != nil {
					return nil, err
				}
			default:
				if err := skip(); err != nil {
					return nil, err
				}
			}
		}
		if err := delim('}'); err != nil {
			return nil, err
		}
	}
	for c, m := range callee {
		for key, id := range m {
			c.Body[key] += resolveID(ids, id)
		}
	}
	return v, nil
}

func resolveID(ids map[string]string, id string) string {
	if ident, ok := ids[id]; ok {
		return ident
	}
	if ident, ok := externalIdent(id); ok {
		return ident
	}
	return "?" + id
}

// globalNode is the Node of a global body-node id (v2.GlobalOrdinal).
func globalNode(ids map[string]string, gid string) string {
	i := strings.LastIndexByte(gid, '@')
	if i < 0 {
		return "?" + gid
	}
	owner, local := gid[:i], gid[i+1:]
	if local == "" || local[0] < '0' || local[0] > '9' {
		local = "@" + local
	}
	return Node(resolveID(ids, owner), local)
}

func (v *View) addModule(rel string, m *v2.Module, ids map[string]string, callee map[*CV]map[string]string) {
	var walk func(c *v2.Callable, file string, topInit bool)
	walk = func(c *v2.Callable, file string, topInit bool) {
		if c.SourceFile != "" {
			file = c.SourceFile
		}
		ident := c.Signature
		if topInit {
			ident = identityOf(c.Signature, file)
		}
		cv := &CV{Ident: ident, File: file, Line: c.Span.Start[0], Kind: c.Kind, L3: len(c.Cfg) > 0,
			Body: make(map[string]string, len(c.Body)), Edges: map[string]bool{}}
		calls := map[string]string{}
		for key, n := range c.Body {
			flags := ""
			if n.IsGoroutine {
				flags += "g"
			}
			if n.IsDeferred {
				flags += "d"
			}
			cv.Body[key] = n.Kind + "|" + n.Parent + "|" + n.Of + "|" + flags + "|"
			if n.Callee != nil {
				calls[key] = *n.Callee
			}
		}
		for _, e := range c.Cfg {
			cv.Edges["cfg|"+e.Src+"|"+e.Dst+"|"+e.Kind] = true
		}
		for _, e := range c.Cdg {
			cv.Edges["cdg|"+e.Src+"|"+e.Dst+"|"] = true
		}
		for _, e := range c.Ddg {
			cv.Edges["ddg|"+e.Src+"|"+e.Dst+"|"+e.Var+"|"+strings.Join(e.Prov, ",")] = true
		}
		for _, e := range c.Summary {
			cv.Edges["sum|"+e.Src+"|"+e.Dst+"|"] = true
		}
		if prev, dup := v.Calls[ident]; dup {
			// Two callables share an identity: keep both visible, so the
			// comparer reports the collision instead of hiding one.
			ident += "#dup:" + prev.File + ":" + strconv.Itoa(prev.Line) + "/" + file + ":" + strconv.Itoa(cv.Line)
			cv.Ident = ident
		}
		ids[c.ID] = ident
		v.Calls[ident] = cv
		callee[cv] = calls
		for _, k := range slices.Sorted(maps.Keys(c.Callables)) {
			inner := c.Callables[k]
			walk(&inner, file, topInit)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(m.Functions)) {
		fn := m.Functions[k]
		walk(&fn, rel, strings.HasSuffix(fn.Signature, ".init"))
	}
	for _, tn := range slices.Sorted(maps.Keys(m.Types)) {
		t := m.Types[tn]
		for _, k := range slices.Sorted(maps.Keys(t.Callables)) {
			c := t.Callables[k]
			walk(&c, rel, false)
		}
	}
}

// called reports whether some param_in edge targets callable ident.
func (v *View) called(ident string) bool {
	if v.callers == nil {
		v.callers = map[string]bool{}
		for e := range v.App {
			if f := strings.Split(e, "|"); f[0] == "pin" {
				v.callers[f[3]] = true
			}
		}
	}
	return v.callers[ident]
}

// ─── names (MR4) ──────────────────────────────────────────────────────────────

// Fix rewrites a transformed view's spellings back to the original's.
type Fix func(string) string

// StripSuffix is the Fix undoing a rename that appended suffix.
func StripSuffix(suffix string) Fix {
	return func(s string) string { return strings.ReplaceAll(s, suffix, "") }
}

// Apply returns v with fix applied to every `of` and every var label.
func (v *View) Apply(fix Fix) *View {
	out := &View{Calls: make(map[string]*CV, len(v.Calls)), App: make(map[string]bool, len(v.App))}
	for id, c := range v.Calls {
		n := *c
		n.Body, n.Edges = make(map[string]string, len(c.Body)), make(map[string]bool, len(c.Edges))
		for k, b := range c.Body {
			f := strings.Split(b, "|")
			f[2] = fix(f[2])
			n.Body[k] = strings.Join(f, "|")
		}
		for e := range c.Edges {
			f := strings.Split(e, "|")
			if f[0] == "ddg" {
				f[3] = fix(f[3])
			}
			n.Edges[strings.Join(f, "|")] = true
		}
		out.Calls[id] = &n
	}
	for e := range v.App {
		if strings.HasPrefix(e, "pin|") || strings.HasPrefix(e, "pout|") {
			i := strings.LastIndexByte(e, '|')
			e = e[:i+1] + fix(e[i+1:])
		}
		out.App[e] = true
	}
	return out
}

// ─── mapping ──────────────────────────────────────────────────────────────────

// Mapper maps an old source position to a new one.
type Mapper interface {
	Map(Pos) (Pos, bool)
}

// identity maps every position to itself.
type identity struct{}

func (identity) Map(p Pos) (Pos, bool) { return p, true }

// Compose is the map a, then b.
func Compose(a, b Mapper) Mapper { return composed{a, b} }

type composed struct{ a, b Mapper }

func (c composed) Map(p Pos) (Pos, bool) {
	q, ok := c.a.Map(p)
	if !ok {
		return Pos{}, false
	}
	return c.b.Map(q)
}

var posKey = regexp.MustCompile(`^([0-9]+):([0-9]+)(.*)$`)
var deferVar = regexp.MustCompile(`<defer>:([0-9]+):([0-9]+)`)

// mapper carries the keys of one view through pm.
type mapper struct {
	pm       Mapper
	unmapped []string
}

// key maps local key k of a callable declared in file. It returns the new
// key and the new file.
func (m *mapper) key(file, k string) (string, string, bool) {
	g := posKey.FindStringSubmatch(k)
	if g == nil {
		return k, file, true
	}
	l, _ := strconv.Atoi(g[1])
	c, _ := strconv.Atoi(g[2])
	p, ok := m.pm.Map(Pos{File: file, Line: l, Col: c})
	if !ok {
		return k, file, false
	}
	return v2.LocalID(p.Line, p.Col) + g[3], p.File, true
}

func (m *mapper) must(ident, file, k string) string {
	nk, _, ok := m.key(file, k)
	if !ok {
		m.unmapped = append(m.unmapped, ident+" "+k)
		return "?" + k
	}
	return nk
}

func (m *mapper) label(file, s string) string {
	return deferVar.ReplaceAllStringFunc(s, func(x string) string {
		g := deferVar.FindStringSubmatch(x)
		nk, _, ok := m.key(file, g[1]+":"+g[2])
		if !ok {
			return x
		}
		return "<defer>:" + nk
	})
}

func (m *mapper) callable(id string, c *CV) *CV {
	n := &CV{Ident: c.Ident, File: c.File, Line: c.Line, Kind: c.Kind, L3: c.L3,
		Body: make(map[string]string, len(c.Body)), Edges: make(map[string]bool, len(c.Edges))}
	if c.File != "" {
		if p, ok := m.pm.Map(Pos{File: c.File, Line: c.Line, Col: 1}); ok {
			n.File, n.Line = p.File, p.Line
		} else {
			m.unmapped = append(m.unmapped, id+" declaration")
		}
	}
	for k, b := range c.Body {
		nk := m.must(id, c.File, k)
		f := strings.Split(b, "|")
		if f[1] != "" {
			f[1] = m.must(id, c.File, f[1])
		}
		n.Body[nk] = strings.Join(f, "|")
	}
	for e := range c.Edges {
		f := strings.Split(e, "|")
		f[1], f[2] = m.must(id, c.File, f[1]), m.must(id, c.File, f[2])
		if f[0] == "ddg" {
			f[3] = m.label(c.File, f[3])
		}
		n.Edges[strings.Join(f, "|")] = true
	}
	return n
}

// Map returns v in the coordinates pm maps into, and the keys that did not
// map (sorted). A nil pm maps every key to itself.
func (v *View) Map(pm Mapper) (*View, []string) {
	if pm == nil {
		pm = identity{}
	}
	m := &mapper{pm: pm}
	out := &View{Calls: make(map[string]*CV, len(v.Calls)), App: make(map[string]bool, len(v.App))}
	for id, c := range v.Calls {
		out.Calls[id] = m.callable(id, c)
	}
	node := func(nd string) string {
		ident, k := splitNode(nd)
		c, ok := v.Calls[ident]
		if !ok {
			return nd
		}
		return Node(ident, m.must(ident, c.File, k))
	}
	for e := range v.App {
		f := strings.Split(e, "|")
		if f[0] == "pin" || f[0] == "pout" {
			// tag | src ident | src key | dst ident | dst key | var
			out.App[f[0]+"|"+node(f[1]+"|"+f[2])+"|"+node(f[3]+"|"+f[4])+"|"+f[5]] = true
			continue
		}
		out.App[e] = true
	}
	sort.Strings(m.unmapped)
	return out, m.unmapped
}

// ─── exact ────────────────────────────────────────────────────────────────────

// Scope limits a comparison. Only, when set, compares just those callables
// and no application edges. Skip leaves those callables out, together with
// every application edge that touches them. KeysOnly compares body keys but
// not their attributes.
type Scope struct {
	Only     map[string]bool
	Skip     map[string]bool
	KeysOnly bool
}

func (s Scope) in(ident string) bool {
	if s.Only != nil && !s.Only[ident] {
		return false
	}
	return !s.Skip[ident]
}

func (s Scope) appIn(e string) bool {
	if s.Only != nil {
		return false
	}
	f := strings.Split(e, "|")
	for _, x := range f[1:] {
		if s.Skip[x] {
			return false
		}
	}
	return true
}

// Diff lists where want and got differ within scope, at most limit lines
// (all when limit <= 0).
func Diff(want, got *View, s Scope, limit int) []string {
	var out []string
	add := func(format string, a ...any) { out = append(out, fmt.Sprintf(format, a...)) }
	for _, id := range unionKeys(want.Calls, got.Calls) {
		if !s.in(id) {
			continue
		}
		w, g := want.Calls[id], got.Calls[id]
		switch {
		case g == nil:
			add("%s: callable missing", id)
			continue
		case w == nil:
			add("%s: callable added (%s:%d)", id, g.File, g.Line)
			continue
		}
		if w.Kind != g.Kind || w.File != g.File || w.Line != g.Line {
			add("%s: %s %s:%d -> %s %s:%d", id, w.Kind, w.File, w.Line, g.Kind, g.File, g.Line)
		}
		for _, k := range unionKeys(w.Body, g.Body) {
			wb, wok := w.Body[k]
			gb, gok := g.Body[k]
			switch {
			case !gok:
				add("%s: body node %s (%s) missing", id, k, wb)
			case !wok:
				add("%s: body node %s (%s) added", id, k, gb)
			case wb != gb && !s.KeysOnly:
				add("%s: body node %s: %s -> %s", id, k, wb, gb)
			}
		}
		for _, e := range unionKeys(w.Edges, g.Edges) {
			switch {
			case !g.Edges[e]:
				add("%s: edge %s missing", id, e)
			case !w.Edges[e]:
				add("%s: edge %s added", id, e)
			}
		}
	}
	for _, e := range unionKeys(want.App, got.App) {
		if !s.appIn(e) {
			continue
		}
		switch {
		case !got.App[e]:
			add("app: %s missing", e)
		case !want.App[e]:
			add("app: %s added", e)
		}
	}
	if limit > 0 && len(out) > limit {
		n := len(out)
		out = append(out[:limit:limit], fmt.Sprintf("… and %d more", n-limit))
	}
	return out
}

func unionKeys[V any](a, b map[string]V) []string {
	set := make(map[string]bool, len(a)+len(b))
	for k := range a {
		set[k] = true
	}
	for k := range b {
		set[k] = true
	}
	return slices.Sorted(maps.Keys(set))
}

// ─── delta ────────────────────────────────────────────────────────────────────

// Ref names a body node in a delta: a key of the original (Pre), mapped
// through the position map, or a mark the transform set in its new text.
type Ref struct {
	Pre  string
	Mark string
}

// Pre and Mark build Refs.
func Pre(key string) Ref   { return Ref{Pre: key} }
func Mark(name string) Ref { return Ref{Mark: name} }

func (r Ref) String() string {
	if r.Mark != "" {
		return "mark:" + r.Mark
	}
	return "pre:" + r.Pre
}

// DEdge is one expected edge change. Kind is cfg, cdg, ddg or sum. Label is
// the cfg kind or the ddg var (empty otherwise), and Prov is the ddg
// provenance ("ssa" when empty).
type DEdge struct {
	Kind     string
	Src, Dst Ref
	Label    string
	Prov     string
}

// Edge constructors, so a relation reads like the table.
func CFG(src, dst Ref, kind string) DEdge { return DEdge{Kind: "cfg", Src: src, Dst: dst, Label: kind} }
func CDG(src, dst Ref) DEdge              { return DEdge{Kind: "cdg", Src: src, Dst: dst} }
func DDG(src, dst Ref, v string) DEdge    { return DEdge{Kind: "ddg", Src: src, Dst: dst, Label: v} }
func Port(src, dst Ref, v string) DEdge {
	return DEdge{Kind: "ddg", Src: src, Dst: dst, Label: v, Prov: "reaching-defs"}
}

// Delta is the expected change of one callable (an Identity of the
// original). Add and Remove are the edges added and removed, and Nodes the
// body nodes added (mark → kind). With CDGFromCFG the expected CDG is not
// listed: it is the reference CDG of the expected CFG (RefCDG). With AtMost
// the relation is a precision one: any subset of Remove may go, and nothing
// may be added.
type Delta struct {
	Site       string
	Callable   string
	Add        []DEdge
	Remove     []DEdge
	Nodes      map[string]string
	CDGFromCFG bool
	AtMost     bool
}

func (r Ref) resolve(m *mapper, file string, marks map[string]Pos) (string, error) {
	if r.Mark != "" {
		p, ok := marks[r.Mark]
		if !ok {
			return "", fmt.Errorf("no mark %s", r.Mark)
		}
		return v2.LocalID(p.Line, p.Col), nil
	}
	k, _, ok := m.key(file, r.Pre)
	if !ok {
		return "", fmt.Errorf("pre key %s does not map", r.Pre)
	}
	return k, nil
}

func (e DEdge) str(m *mapper, file string, marks map[string]Pos) (string, error) {
	s, err := e.Src.resolve(m, file, marks)
	if err != nil {
		return "", err
	}
	d, err := e.Dst.resolve(m, file, marks)
	if err != nil {
		return "", err
	}
	out := e.Kind + "|" + s + "|" + d + "|" + e.Label
	if e.Kind == "ddg" {
		out += "|" + cmpOr(e.Prov, "ssa")
	}
	return out, nil
}

func cmpOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Expect applies ds to mapped (pre mapped through pm) and returns the view
// the transformed module must have. A removed edge the original lacks, and
// an added one it already has, are errors of the relation (its delta is
// wrong), as is a declared CDG that disagrees with RefCDG of the expected
// CFG.
func Expect(pre, mapped *View, pm Mapper, marks map[string]Pos, ds []Delta) (*View, error) {
	if pm == nil {
		pm = identity{}
	}
	out := &View{Calls: maps.Clone(mapped.Calls), App: mapped.App}
	m := &mapper{pm: pm}
	for _, d := range ds {
		c0, c := pre.Calls[d.Callable], mapped.Calls[d.Callable]
		if c0 == nil || c == nil {
			return nil, fmt.Errorf("delta %s: no callable %s", d.Site, d.Callable)
		}
		if d.AtMost {
			continue // CheckAtMost
		}
		n := c.clone()
		for _, e := range d.Remove {
			s, err := e.str(m, c0.File, marks)
			if err != nil {
				return nil, fmt.Errorf("delta %s: %w", d.Site, err)
			}
			if !n.Edges[s] {
				return nil, fmt.Errorf("delta %s: removed edge %s is not in the original", d.Site, s)
			}
			delete(n.Edges, s)
		}
		for _, e := range d.Add {
			s, err := e.str(m, c0.File, marks)
			if err != nil {
				return nil, fmt.Errorf("delta %s: %w", d.Site, err)
			}
			if n.Edges[s] {
				return nil, fmt.Errorf("delta %s: added edge %s is already there", d.Site, s)
			}
			n.Edges[s] = true
		}
		for mark, kind := range d.Nodes {
			k, err := Mark(mark).resolve(m, c0.File, marks)
			if err != nil {
				return nil, fmt.Errorf("delta %s: %w", d.Site, err)
			}
			n.Body[k] = NodeAttr(kind)
		}
		ref := RefCDG(n)
		if d.CDGFromCFG {
			for e := range n.Edges {
				if strings.HasPrefix(e, "cdg|") {
					delete(n.Edges, e)
				}
			}
			for e := range ref {
				n.Edges[e] = true
			}
		} else if diff := cdgDiff(n, ref); diff != "" {
			return nil, fmt.Errorf("delta %s: declared CDG disagrees with the reference CDG of the expected CFG: %s", d.Site, diff)
		}
		out.Calls[d.Callable] = n
	}
	return out, nil
}

func cdgDiff(c *CV, ref map[string]bool) string {
	var out []string
	for e := range c.Edges {
		if strings.HasPrefix(e, "cdg|") && !ref[e] {
			out = append(out, "-"+e)
		}
	}
	for e := range ref {
		if !c.Edges[e] {
			out = append(out, "+"+e)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

// CheckAtMost checks the AtMost deltas of ds: in each callable got is
// mapped minus some subset of Remove, with nothing added.
func CheckAtMost(pre, mapped, got *View, pm Mapper, marks map[string]Pos, ds []Delta) []string {
	if pm == nil {
		pm = identity{}
	}
	var out []string
	m := &mapper{pm: pm}
	for _, d := range ds {
		if !d.AtMost {
			continue
		}
		c0, w, g := pre.Calls[d.Callable], mapped.Calls[d.Callable], got.Calls[d.Callable]
		if c0 == nil || w == nil || g == nil {
			out = append(out, d.Site+": callable missing")
			continue
		}
		allowed := map[string]bool{}
		for _, e := range d.Remove {
			if s, err := e.str(m, c0.File, marks); err == nil {
				allowed[s] = true
			}
		}
		for _, e := range unionKeys(w.Edges, g.Edges) {
			switch {
			case !g.Edges[e] && !allowed[e]:
				out = append(out, fmt.Sprintf("%s: %s removed", d.Site, e))
			case !w.Edges[e]:
				out = append(out, fmt.Sprintf("%s: %s added", d.Site, e))
			}
		}
	}
	return out
}

// ─── reference CDG ────────────────────────────────────────────────────────────

// RefCDG is the control-dependence graph of c's cfg edges, computed
// independently of the analyzer: post-dominator sets by fixed-point
// iteration, then x is control dependent on a when some edge a→b has x
// post-dominating b (reflexively) but not strictly post-dominating a
// (Ferrante–Ottenstein–Warren); self-loops are dropped, and every CFG node
// but @entry/@exit with no controller is anchored at @entry (the analyzer's
// documented convention, spec §4.4).
func RefCDG(c *CV) map[string]bool {
	succ := map[string][]string{}
	set := map[string]bool{"@exit": true}
	for e := range c.Edges {
		if !strings.HasPrefix(e, "cfg|") {
			continue
		}
		f := strings.Split(e, "|")
		succ[f[1]] = append(succ[f[1]], f[2])
		set[f[1]], set[f[2]] = true, true
	}
	nodes := slices.Sorted(maps.Keys(set))
	idx := make(map[string]int, len(nodes))
	for i, n := range nodes {
		idx[n] = i
	}
	words := (len(nodes) + 63) / 64
	full := make([]uint64, words)
	for i := range nodes {
		full[i/64] |= 1 << (i % 64)
	}
	exit := idx["@exit"]
	pdom := make([][]uint64, len(nodes))
	for i := range nodes {
		pdom[i] = slices.Clone(full)
	}
	pdom[exit] = make([]uint64, words)
	pdom[exit][exit/64] |= 1 << (exit % 64)
	for changed := true; changed; {
		changed = false
		for i, n := range nodes {
			if i == exit {
				continue
			}
			nw := slices.Clone(full)
			if len(succ[n]) == 0 {
				nw = make([]uint64, words) // reaches no exit
			}
			for _, s := range succ[n] {
				for w := range nw {
					nw[w] &= pdom[idx[s]][w]
				}
			}
			nw[i/64] |= 1 << (i % 64)
			if !slices.Equal(nw, pdom[i]) {
				pdom[i], changed = nw, true
			}
		}
	}
	has := func(set []uint64, j int) bool { return set[j/64]&(1<<(j%64)) != 0 }
	out := map[string]bool{}
	controlled := map[string]bool{}
	for _, a := range nodes {
		ia := idx[a]
		for _, b := range succ[a] {
			ib := idx[b]
			for w, word := range pdom[ib] {
				for word != 0 {
					j := w*64 + bits.TrailingZeros64(word)
					word &= word - 1
					if j == ia || (has(pdom[ia], j) && j != ia) {
						continue
					}
					if nodes[j] == "@exit" {
						continue
					}
					out["cdg|"+a+"|"+nodes[j]+"|"] = true
					controlled[nodes[j]] = true
				}
			}
		}
	}
	for _, n := range nodes {
		if n != "@entry" && n != "@exit" && !controlled[n] {
			out["cdg|@entry|"+n+"|"] = true
		}
	}
	return out
}

// ─── reachability ─────────────────────────────────────────────────────────────

// Graph is the application-scope dependence graph over Node strings: ddg ∪
// summary ∪ param_in ∪ param_out, plus cdg unless dataOnly.
type Graph map[string][]string

// Graph builds v's dependence graph.
func (v *View) Graph(dataOnly bool) Graph {
	g := Graph{}
	for id, c := range v.Calls {
		for e := range c.Edges {
			f := strings.SplitN(e, "|", 4)
			if f[0] == "cfg" || (dataOnly && f[0] == "cdg") {
				continue
			}
			s := Node(id, f[1])
			g[s] = append(g[s], Node(id, f[2]))
		}
	}
	for e := range v.App {
		f := strings.Split(e, "|")
		if f[0] != "pin" && f[0] != "pout" {
			continue
		}
		s := f[1] + "|" + f[2]
		g[s] = append(g[s], f[3]+"|"+f[4])
	}
	return g
}

// Reach is the set of nodes reachable from src, src included.
func (g Graph) Reach(src string) map[string]bool {
	seen := map[string]bool{src: true}
	stack := []string{src}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, d := range g[n] {
			if !seen[d] {
				seen[d] = true
				stack = append(stack, d)
			}
		}
	}
	return seen
}

var vertexTail = regexp.MustCompile(`^(.*)/(actual_in|actual_out)(:[0-9]+)?$`)

// locator finds the counterpart in post of a node of pre. It tries the
// mapped key in the same callable first. Failing that, it tries any
// callable of post that holds the mapped position in the mapped file, since
// a statement can move into a new callable (MR6, MR14). A call-site vertex
// (<call>/actual_*:i) matches by its call key, kind and `of`, not by index.
type locator struct {
	pre, post *View
	m         *mapper
	byFileKey map[string][]string
	// vertex indexes each call-site vertex of post by callable, call key,
	// kind and `of` (the least key when several match).
	vertex map[[4]string]string
	memo   map[string]string // locate's results ("" = none)
}

func newLocator(pre, post *View, pm Mapper) *locator {
	if pm == nil {
		pm = identity{}
	}
	l := &locator{pre: pre, post: post, m: &mapper{pm: pm}, byFileKey: map[string][]string{}, vertex: map[[4]string]string{}, memo: map[string]string{}}
	for id, c := range post.Calls {
		for k, b := range c.Body {
			if h := vertexTail.FindStringSubmatch(k); h != nil {
				vk := [4]string{id, h[1], h[2], strings.Split(b, "|")[2]}
				if prev, ok := l.vertex[vk]; !ok || k < prev {
					l.vertex[vk] = k
				}
				continue
			}
			if posKey.MatchString(k) {
				l.byFileKey[c.File+" "+k] = append(l.byFileKey[c.File+" "+k], id)
			}
		}
	}
	for _, ids := range l.byFileKey {
		slices.Sort(ids)
	}
	return l
}

func (l *locator) locate(node string) (string, bool) {
	if r, ok := l.memo[node]; ok {
		return r, r != ""
	}
	r, ok := l.find(node)
	l.memo[node] = r
	return r, ok
}

func (l *locator) find(node string) (string, bool) {
	ident, key := splitNode(node)
	c0 := l.pre.Calls[ident]
	if c0 == nil {
		return "", false
	}
	nk, nf, ok := l.m.key(c0.File, key)
	if !ok {
		return "", false
	}
	g := vertexTail.FindStringSubmatch(nk)
	cands := []string{ident}
	if posKey.MatchString(nk) {
		base := nk
		if g != nil {
			base = g[1]
		}
		cands = append(cands, l.byFileKey[nf+" "+base]...)
	}
	of := ""
	if b, ok := c0.Body[key]; ok {
		of = strings.Split(b, "|")[2]
	}
	for _, id := range cands {
		c := l.post.Calls[id]
		if c == nil {
			continue
		}
		if g == nil {
			if _, ok := c.Body[nk]; ok {
				return Node(id, nk), true
			}
			continue
		}
		if k, ok := l.vertex[[4]string{id, g[1], g[2], of}]; ok {
			return Node(id, k), true
		}
	}
	return "", false
}

// ReachResult is the outcome of the reachability oracle. Lost and
// Unlocated hold at most maxReport entries each; NLost and NUnlocated count
// them all.
type ReachResult struct {
	Callables, Sources, Pairs int
	Lost                      []string // "source -> sink", no longer reachable
	Unlocated                 []string // sources or sinks with no counterpart
	NLost, NUnlocated         int
}

const maxReport = 200

func (r *ReachResult) lost(s string) {
	r.NLost++
	if len(r.Lost) < maxReport {
		r.Lost = append(r.Lost, s)
	}
}

func (r *ReachResult) unlocated(s string) {
	r.NUnlocated++
	if len(r.Unlocated) < maxReport {
		r.Unlocated = append(r.Unlocated, s)
	}
}

// igraph is a Graph over interned node indices.
type igraph struct {
	idx  map[string]int32
	name []string
	succ [][]int32
}

func intern(g Graph) *igraph {
	ig := &igraph{idx: make(map[string]int32, len(g))}
	id := func(n string) int32 {
		i, ok := ig.idx[n]
		if !ok {
			i = int32(len(ig.name))
			ig.idx[n] = i
			ig.name = append(ig.name, n)
			ig.succ = append(ig.succ, nil)
		}
		return i
	}
	for _, n := range slices.Sorted(maps.Keys(g)) {
		s := id(n)
		for _, d := range g[n] {
			ig.succ[s] = append(ig.succ[s], id(d))
		}
	}
	return ig
}

// reach marks in seen (len = node count, cleared by the caller) every node
// reachable from src, and returns them in visit order.
func (ig *igraph) reach(src int32, seen []bool, out []int32) []int32 {
	out = append(out[:0], src)
	seen[src] = true
	for i := 0; i < len(out); i++ {
		for _, d := range ig.succ[out[i]] {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	return out
}

// CheckReach samples up to n callables of pre with an L3 result (those in
// prefer first, then the others, shuffled with seed). For every @formal_in
// of each, it checks that every sink reachable in pre is reachable from the
// source's counterpart in post. A sink is a formal_out vertex or the
// actual_in of a call node. The actual_in of a capture site is not a sink:
// it feeds only its closure's formal_in (param_in; a capture site has no
// summary edge, spec §4.6), so whatever a flow into it reaches downstream is
// checked at that sink.
//
// cut lists pre nodes whose out-edges the pre graph leaves out (a relation
// that legitimately removes flows through them, MR14).
func CheckReach(pre, post *View, pm Mapper, prefer []string, n int, seed uint64, cut ...string) ReachResult {
	gPre, gPost := pre.Graph(false), post.Graph(false)
	for _, c := range cut {
		delete(gPre, c)
	}
	iPre, iPost := intern(gPre), intern(gPost)
	seenPre, seenPost := make([]bool, len(iPre.name)), make([]bool, len(iPost.name))
	var bufPre, bufPost []int32
	// sinkPost[i] is the post counterpart of pre node i (-1 none, -2 not a
	// sink, -3 not yet computed).
	sinkPost := make([]int32, len(iPre.name))
	for i := range sinkPost {
		sinkPost[i] = -3
	}
	l := newLocator(pre, post, pm)
	var res ReachResult
	for _, id := range SampleCallables(pre, prefer, n, seed) {
		res.Callables++
		c := pre.Calls[id]
		for _, k := range slices.Sorted(maps.Keys(c.Body)) {
			if !strings.HasPrefix(k, "@formal_in:") {
				continue
			}
			src := Node(id, k)
			psrc, ok := l.locate(src)
			if !ok {
				res.unlocated("source " + src)
				continue
			}
			res.Sources++
			si, ok := iPre.idx[src]
			if !ok {
				continue // no out-edge: reaches nothing
			}
			bufPost = bufPost[:0]
			if pi, ok := iPost.idx[psrc]; ok {
				bufPost = iPost.reach(pi, seenPost, bufPost)
			}
			bufPre = iPre.reach(si, seenPre, bufPre)
			var lost []string
			for _, ni := range bufPre {
				if ni == si {
					continue
				}
				if sinkPost[ni] == -3 {
					sinkPost[ni] = -2
					if sink := iPre.name[ni]; pre.isSink(sink) {
						sinkPost[ni] = -1
						if ps, ok := l.locate(sink); ok {
							if pi, ok := iPost.idx[ps]; ok {
								sinkPost[ni] = pi
							} else {
								sinkPost[ni] = int32(len(iPost.name)) // located, no edges: unreachable
							}
						}
					}
				}
				switch p := sinkPost[ni]; {
				case p == -2:
					continue
				case p == -1:
					res.Pairs++
					res.unlocated("sink " + iPre.name[ni])
				default:
					res.Pairs++
					if int(p) >= len(seenPost) || !seenPost[p] {
						lost = append(lost, iPre.name[ni])
					}
				}
			}
			for _, ni := range bufPre {
				seenPre[ni] = false
			}
			for _, ni := range bufPost {
				seenPost[ni] = false
			}
			slices.Sort(lost)
			for _, sink := range lost {
				res.lost(src + " -> " + sink)
			}
		}
	}
	return res
}

func (v *View) isSink(node string) bool {
	id, k := splitNode(node)
	if strings.HasPrefix(k, "@formal_out") {
		return true
	}
	g := vertexTail.FindStringSubmatch(k)
	if g == nil || g[2] != "actual_in" {
		return false
	}
	c := v.Calls[id]
	return c != nil && strings.HasPrefix(c.Body[g[1]], "call|")
}

// SampleCallables picks up to n callables of v with an L3 result: those in
// prefer first, then the rest, each group shuffled with seed. n <= 0 picks
// all of them.
func SampleCallables(v *View, prefer []string, n int, seed uint64) []string {
	r := rand.New(rand.NewPCG(seed, 2))
	taken := map[string]bool{}
	pick := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			if c := v.Calls[id]; c != nil && c.L3 && !taken[id] {
				taken[id] = true
				out = append(out, id)
			}
		}
		slices.Sort(out)
		r.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	out := pick(prefer)
	if n <= 0 || len(out) < n {
		out = append(out, pick(slices.Collect(maps.Keys(v.Calls)))...)
	}
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	slices.Sort(out)
	return out
}

// ReachesFrom reports whether any node of srcs reaches dst over g.
func (g Graph) ReachesFrom(srcs []string, dst string) bool {
	for _, s := range srcs {
		if g.Reach(s)[dst] {
			return true
		}
	}
	return false
}

// Witness is a shortest path src → dst over g (nil when none).
func (g Graph) Witness(src, dst string) []string {
	prev := map[string]string{src: ""}
	q := []string{src}
	for len(q) > 0 {
		n := q[0]
		q = q[1:]
		if n == dst {
			var p []string
			for x := dst; x != ""; x = prev[x] {
				p = append([]string{x}, p...)
			}
			return p
		}
		for _, d := range g[n] {
			if _, ok := prev[d]; !ok {
				prev[d] = n
				q = append(q, d)
			}
		}
	}
	return nil
}
