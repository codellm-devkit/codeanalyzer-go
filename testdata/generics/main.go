package main

import (
	"fmt"

	"example.com/generics/fn"
	"example.com/generics/set"
)

func main() {
	s := set.New[string]()
	s.Add("hello")
	s.Add("world")
	fmt.Println(s.Contains("hello"), s.Len())

	fmt.Println(fn.Min(3, 7))
	fmt.Println(fn.Max(3.14, 2.72))

	nums := fn.Map([]int{1, 2, 3}, func(x int) string { return fmt.Sprintf("%d", x) })
	evens := fn.Filter([]int{1, 2, 3, 4}, func(x int) bool { return x%2 == 0 })
	fmt.Println(nums, evens)
}
