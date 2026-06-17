// Package greeter provides simple greeting functionality.
package greeter

import "fmt"

// Greeter holds a greeting prefix.
type Greeter struct {
	Prefix string `json:"prefix"`
}

// New creates a Greeter with the given prefix.
func New(prefix string) *Greeter {
	return &Greeter{Prefix: prefix}
}

// Greet returns a greeting for name.
func (g *Greeter) Greet(name string) string {
	return fmt.Sprintf("%s, %s!", g.Prefix, name)
}

// Logger is a simple logging interface.
type Logger interface {
	Log(msg string)
}

// Shout returns the message in a louder form.
func Shout(msg string) string {
	return msg + "!!!"
}
