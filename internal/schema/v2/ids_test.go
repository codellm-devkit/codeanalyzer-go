package v2

import "testing"

func TestAppID(t *testing.T) {
	if got, want := AppID("myapp"), "can://go/myapp"; got != want {
		t.Errorf("AppID = %q, want %q", got, want)
	}
}

func TestDurableIDChain(t *testing.T) {
	// Reproduces the keystone worked example, segment by segment.
	app := AppID("myapp")
	mod := ModuleID(app, "src/util.go")
	typ := TypeID(mod, "Hasher")
	call := CallableID(typ, "Hash(string)uint64")

	cases := []struct{ got, want string }{
		{mod, "can://go/myapp/src/util.go"},
		{typ, "can://go/myapp/src/util.go/Hasher"},
		{call, "can://go/myapp/src/util.go/Hasher/Hash(string)uint64"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("id = %q, want %q", c.got, c.want)
		}
	}
}

func TestModuleIDNormalizesPath(t *testing.T) {
	app := AppID("myapp")
	// Backslashes become forward slashes; a leading slash is trimmed.
	if got, want := ModuleID(app, "\\pkg\\a.go"), "can://go/myapp/pkg/a.go"; got != want {
		t.Errorf("ModuleID = %q, want %q", got, want)
	}
	if got, want := ModuleID(app, "/pkg/a.go"), "can://go/myapp/pkg/a.go"; got != want {
		t.Errorf("ModuleID = %q, want %q", got, want)
	}
}

func TestModuleLevelFunctionParent(t *testing.T) {
	// A module-level function hangs off the module id, not a type.
	mod := ModuleID(AppID("myapp"), "src/util.go")
	fn := CallableID(mod, "New64()")
	if want := "can://go/myapp/src/util.go/New64()"; fn != want {
		t.Errorf("CallableID = %q, want %q", fn, want)
	}
}

func TestOrdinalAndTagIDs(t *testing.T) {
	call := "can://go/myapp/src/util.go/Hasher/Hash(string)uint64"
	if got, want := OrdinalID(call, 15, 2), call+"@15:2"; got != want {
		t.Errorf("OrdinalID = %q, want %q", got, want)
	}
	if got, want := TagID(call, "entry"), call+"@entry"; got != want {
		t.Errorf("TagID = %q, want %q", got, want)
	}
	if got, want := LocalID(16, 2), "16:2"; got != want {
		t.Errorf("LocalID = %q, want %q", got, want)
	}
}

func TestSpanSlice(t *testing.T) {
	src := "package util\nfunc f() {}\n"
	// bytes [13,25) covers "func f() {}\n"
	s := NewSpan(2, 1, 2, 12, 13, 25)
	if got, want := s.Slice(src), "func f() {}\n"; got != want {
		t.Errorf("Slice = %q, want %q", got, want)
	}
}

func TestSpanSliceOutOfRangeIsEmpty(t *testing.T) {
	src := "abc"
	cases := []Span{
		{Bytes: [2]int{0, 99}}, // to past end
		{Bytes: [2]int{-1, 2}}, // negative from
		{Bytes: [2]int{2, 1}},  // inverted
	}
	for i, s := range cases {
		if got := s.Slice(src); got != "" {
			t.Errorf("case %d: Slice = %q, want empty", i, got)
		}
	}
}

func TestItoa(t *testing.T) {
	cases := map[int]string{0: "0", 7: "7", 42: "42", 1000: "1000", -3: "-3"}
	for in, want := range cases {
		if got := itoa(in); got != want {
			t.Errorf("itoa(%d) = %q, want %q", in, got, want)
		}
	}
}
