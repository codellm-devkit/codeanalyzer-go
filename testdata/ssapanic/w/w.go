// Package w has a value method that no code calls: its pointer wrapper
// (*V).Never is built only on demand, when the VTA function set is computed.
package w

type V struct{}

func (V) Never() {}
