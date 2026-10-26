// Package initclosure has a func init() with a closure in more than one file.
// The closures share the signature example.com/dataflow/initclosure.init.closure_1,
// so a call edge to one must carry its declaring file.
package initclosure

var hooks []func() int

func init() {
	hooks = append(hooks, func() int { return 1 })
}

// Run calls every hook through a function value.
func Run() int {
	n := 0
	for _, h := range hooks {
		n += h()
	}
	return n
}
