// Package wrap is the L4 fixture for VTA targets behind synthetic wrappers
// (spec §4.8): an interface call reaches sqlStore.Find directly, through a
// method value of the interface (VTA returns the (Store).Find$bound wrapper)
// and through a struct embedding the interface (VTA returns logged's
// promotion wrapper). Each wrapper's own call of Find is a synthetic call
// site; its VTA targets are the original site's. Tests pin keys by line:col,
// so keep each statement on its own line and append new functions at the end.
package wrap

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

// HandleDirect calls Find through the interface.
func HandleDirect(s Store, id string) string {
	return s.Find(id)
}

// HandleBound calls Find through a method value of the interface.
func HandleBound(s Store, id string) string {
	f := s.Find
	return f(id)
}

// logged decorates a Store by embedding it: Find is promoted.
type logged struct {
	Store
}

// HandleDecorated calls Find on a decorated Store, through the interface.
func HandleDecorated(s Store, id string) string {
	var l Store = logged{s}
	return l.Find(id)
}

// MainDirect, MainBound and MainDecorated each pass a sqlStore to one
// handler: no host variable is shared between the handlers' call sites.
func MainDirect() {
	HandleDirect(&sqlStore{}, "a")
}

func MainBound() {
	HandleBound(&sqlStore{}, "b")
}

func MainDecorated() {
	HandleDecorated(&sqlStore{}, "c")
}
