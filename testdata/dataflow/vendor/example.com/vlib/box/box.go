// Package box is a vendored library of interface containers: what it hands
// out comes from library code, which L4 builds no SSA for. Its elements
// have its own interface type, so a caller needs no type assertion.
package box

// Finder is the library's interface; project types implement it.
type Finder interface {
	Find(id string) string
}

var store = map[string]Finder{}

// Put stores f under k.
func Put(k string, f Finder) { store[k] = f }

// Map returns the stored values by key.
func Map() map[string]Finder { return store }

// List returns the stored values.
func List() []Finder {
	var out []Finder
	for _, f := range store {
		out = append(out, f)
	}
	return out
}

// First returns the value under k as a one-element array.
func First(k string) [1]Finder { return [1]Finder{store[k]} }

// Chan returns a channel that yields the value under k once.
func Chan(k string) <-chan Finder {
	ch := make(chan Finder, 1)
	ch <- store[k]
	return ch
}
