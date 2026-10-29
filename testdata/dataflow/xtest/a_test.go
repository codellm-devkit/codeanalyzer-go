package xtest_test

import (
	"testing"

	"example.com/dataflow/xtest"
)

func helper(c *xtest.Counter) int { return c.Add(2) }

func TestAdd(t *testing.T) {
	c := xtest.New()
	check := func(n int) {
		if n != 2 {
			t.Fatal(n)
		}
	}
	check(helper(c))
}
