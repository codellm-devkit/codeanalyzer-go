// Package libstore is the L4 fixture for dynamic types that pass through
// library storage (spec §4.8, CHA complement): a Store put in a
// context.Context or a sync.Map comes back out of library code, which has
// no SSA bodies, so VTA sees no concrete type at the interface call. The
// call's targets then include every project type implementing Store. An
// interface call on a value project code built (HandleLocal) keeps VTA's
// targets only. Tests pin keys by line:col, so keep each statement on its
// own line and append new functions at the end.
package libstore

import (
	"context"
	"sync"
)

// Store is the interface the handlers call.
type Store interface {
	Find(id string) string
}

// lastQuery is the global sqlStore.Find writes.
var lastQuery string

type sqlStore struct{}

// Find builds the query from id and runs it: the sink is Exec's query.
func (s *sqlStore) Find(id string) string {
	lastQuery = id
	return Exec("select " + id)
}

// Exec is the sink.
func Exec(query string) string {
	return query
}

type storeKey struct{}

// WithStore puts s in ctx.
func WithStore(ctx context.Context, s Store) context.Context {
	return context.WithValue(ctx, storeKey{}, s)
}

// HandleCtx takes the Store back out of ctx and calls Find.
func HandleCtx(ctx context.Context, id string) string {
	return ctx.Value(storeKey{}).(Store).Find(id)
}

var stores sync.Map

// Register puts s in the stores map.
func Register(name string, s Store) {
	stores.Store(name, s)
}

// HandleMap loads the Store from the stores map and calls Find.
func HandleMap(name, id string) string {
	v, _ := stores.Load(name)
	return v.(Store).Find(id)
}

type memStore struct{}

// Find returns id.
func (memStore) Find(id string) string {
	return id
}

// HandleLocal calls Find on a Store project code built: VTA sees memStore.
func HandleLocal(id string) string {
	var s Store = memStore{}
	return s.Find(id)
}

// MainCtx and MainMap each store a sqlStore in library storage and call one
// handler: no host variable is shared between the handlers' call sites.
func MainCtx() {
	ctx := WithStore(context.Background(), &sqlStore{})
	HandleCtx(ctx, "a")
}

func MainMap() {
	Register("db", &sqlStore{})
	HandleMap("db", "b")
}

// errStore embeds the universe error: its Error method has no package.
type errStore struct {
	error
}

// HandleErr calls Error on an error that came out of a library call: the
// CHA complement meets errStore, whose Error is the universe method.
func HandleErr(ctx context.Context) string {
	return context.Cause(ctx).Error()
}
