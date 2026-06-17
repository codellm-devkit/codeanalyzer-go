// Second file in the server package — exercises multi-file package detection.
package server

import (
	"fmt"
	"strings"
)

// Tags formats key-value pairs into a single string.
// Exercises: variadic parameter (pairs ...string), is_variadic=true.
func Tags(pairs ...string) string {
	return fmt.Sprintf("[%s]", strings.Join(pairs, ", "))
}

// Describe returns a human-readable description of the server.
// Exercises: value receiver (s Server) vs pointer receiver in server.go.
func (s Server) Describe() string {
	return fmt.Sprintf("server at %s", s.Addr())
}
