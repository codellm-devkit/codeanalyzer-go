package sdg

// The library calls of the L4 stand-in fixture; they live apart from sdg.go
// so that its line:col keys stay put.

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
)

// format calls fmt.Sprintf: its stand-in has formal_ins format and a.
func format(a int) string {
	return fmt.Sprintf("%d", a)
}

// decode calls json.Unmarshal, whose v (any) is a by-ref out.
func decode(data []byte) map[string]int {
	var m map[string]int
	_ = json.Unmarshal(data, &m)
	return m
}

// atoi calls strconv.Atoi: s flows to both results unless a model says not.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// yield calls runtime.Gosched, an external with no formals at all.
func yield() {
	runtime.Gosched()
}
