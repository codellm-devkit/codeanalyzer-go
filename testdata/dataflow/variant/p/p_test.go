package p

import "testing"

func TestM(t *testing.T) {
	if New().M("a") != "a" {
		t.Fatal("M")
	}
}
