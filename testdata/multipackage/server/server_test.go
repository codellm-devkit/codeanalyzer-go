package server

import "testing"

// TestServer_Addr is a minimal test in the realistic fixture's server package.
// Its only purpose is to give the --skip-tests=false integration test a
// _test.go file to look for in the symbol table.
func TestServer_Addr(t *testing.T) {
	s, err := New(Config{Host: "localhost", Port: 8080})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.Addr() == "" {
		t.Error("Addr() returned empty string")
	}
}
