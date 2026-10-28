// Package libflow is the L4 fixture for interprocedural library origin
// (spec §4.8, CHA complement): a Store that came out of library code
// (context.Context, the vendored box) reaches an interface call through
// project code — a project function's result, a parameter, a struct field,
// a global, a closure capture, a map, slice or channel element. VTA sees no
// concrete type in library code, so each call's targets include every
// project type implementing Store. Tests pin keys by line:col, so keep each
// statement on its own line and append new functions at the end.
package libflow

import (
	"context"

	"example.com/vlib/box"
)

// Store is the interface the handlers call.
type Store interface {
	Find(id string) string
}

// Last is the global sqlStore.Find writes.
var Last string

type sqlStore struct{}

// Find builds the query from id and runs it: the sink is Exec's query.
func (s *sqlStore) Find(id string) string {
	Last = id
	return Exec("select " + id)
}

// Exec is the sink.
func Exec(query string) string {
	return query
}

type key struct{}

// fromCtx returns the Store in ctx: its result is library-originated.
func fromCtx(ctx context.Context) Store {
	return ctx.Value(key{}).(Store)
}

// ViaReturn calls Find on a project function's library-originated result.
func ViaReturn(ctx context.Context, id string) string {
	return fromCtx(ctx).Find(id)
}

// use calls Find on its parameter: one caller passes a library-originated
// Store, another a project-built one.
func use(s Store, id string) string {
	return s.Find(id)
}

// ViaParam passes a library-originated Store to use.
func ViaParam(ctx context.Context, id string) string {
	return use(ctx.Value(key{}).(Store), id)
}

type memStore struct{}

// Find returns id.
func (memStore) Find(id string) string {
	return id
}

// UseLocal passes a project-built Store to use.
func UseLocal(id string) string {
	return use(memStore{}, id)
}

type handler struct {
	store Store
}

// init stores a library-originated Store in a field.
func (h *handler) init(ctx context.Context) {
	h.store = ctx.Value(key{}).(Store)
}

// ViaField calls Find on the field another method set.
func (h *handler) ViaField(id string) string {
	return h.store.Find(id)
}

var global Store

// SetGlobal stores a library-originated Store in a global.
func SetGlobal(ctx context.Context) {
	global = ctx.Value(key{}).(Store)
}

// ViaGlobal calls Find on the global.
func ViaGlobal(id string) string {
	return global.Find(id)
}

// ViaCapture calls Find inside a closure on a captured library-originated
// Store.
func ViaCapture(ctx context.Context, id string) string {
	s := ctx.Value(key{}).(Store)
	f := func() string {
		return s.Find(id)
	}
	return f()
}

// ViaMap looks up a library map (comma-ok free): the element is
// library-originated.
func ViaMap(id string) string {
	return box.Map()["db"].Find(id)
}

// ViaSlice indexes a library slice.
func ViaSlice(id string) string {
	return box.List()[0].Find(id)
}

// ViaChan receives from a library channel.
func ViaChan(id string) string {
	return (<-box.Chan("db")).Find(id)
}

// ViaArray indexes a library array value.
func ViaArray(id string) string {
	return box.First("db")[0].Find(id)
}

// ProjectOnly calls Find on a Store only project code ever built: no CHA
// target.
func ProjectOnly(id string) string {
	var s Store = memStore{}
	return relay(s).Find(id)
}

// relay returns its parameter.
func relay(s Store) Store {
	return s
}

// The entries: each puts a sqlStore in library storage and calls one
// handler, so no host variable is shared between the handlers' sites.
func MainReturn() {
	ViaReturn(context.WithValue(context.Background(), key{}, &sqlStore{}), "a")
}

func MainParam() {
	ViaParam(context.WithValue(context.Background(), key{}, &sqlStore{}), "b")
}

func MainField() {
	h := &handler{}
	h.init(context.WithValue(context.Background(), key{}, &sqlStore{}))
	h.ViaField("c")
}

func MainGlobal() {
	SetGlobal(context.WithValue(context.Background(), key{}, &sqlStore{}))
	ViaGlobal("d")
}

func MainCapture() {
	ViaCapture(context.WithValue(context.Background(), key{}, &sqlStore{}), "e")
}

func MainBox() {
	box.Put("db", &sqlStore{})
	ViaMap("f")
	ViaSlice("g")
	ViaChan("h")
	ViaArray("i")
}
