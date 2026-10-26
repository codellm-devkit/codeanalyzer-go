package initclosure

func init() {
	hooks = append(hooks, func() int { return 2 })
}
