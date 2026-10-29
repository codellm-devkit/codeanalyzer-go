// Package collision has two calls at the same line:col in sibling files, one in
// a method declared away from its receiver type (#32).
package collision

type T struct{}

func (T) M() {}

func wrong() {}

func HelpX() { wrong() }
