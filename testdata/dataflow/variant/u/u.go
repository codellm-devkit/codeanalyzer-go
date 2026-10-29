// Package u calls p's interface method on the value p.New returns.
package u

import "example.com/dataflow/variant/p"

// Use dispatches M through the interface p.New returns.
func Use(x string) string {
	return p.New().M(x)
}
