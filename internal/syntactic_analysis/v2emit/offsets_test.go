package v2emit

import "testing"

const sample = "package util\n" + // line 1: bytes 0..12  (\n at 12)
	"\n" + // line 2: byte 13 (\n at 13)
	"func f() {}\n" + // line 3: bytes 14..25 (\n at 25)
	"var x int" // line 4: bytes 26..34, no trailing newline

func TestLineStarts(t *testing.T) {
	li := newLineIndex(sample)
	want := []int{0, 13, 14, 26}
	if len(li.lineStart) != len(want) {
		t.Fatalf("lineStart len = %d, want %d (%v)", len(li.lineStart), len(want), li.lineStart)
	}
	for i, w := range want {
		if li.lineStart[i] != w {
			t.Errorf("lineStart[%d] = %d, want %d", i, li.lineStart[i], w)
		}
	}
}

func TestOffset(t *testing.T) {
	li := newLineIndex(sample)
	// line 3 ("func f() {}") col 1 → byte 14; col 6 ('f') → 19
	if got := li.offset(3, 1); got != 14 {
		t.Errorf("offset(3,1) = %d, want 14", got)
	}
	if got := li.offset(3, 6); got != 19 {
		t.Errorf("offset(3,6) = %d, want 19", got)
	}
	// The slice from a callable start to its end reproduces the source text.
	start := li.offset(3, 1)
	end := li.lineEndOffset(3)
	if got := sample[start:end]; got != "func f() {}" {
		t.Errorf("slice = %q, want %q", got, "func f() {}")
	}
}

func TestOffsetClamping(t *testing.T) {
	li := newLineIndex(sample)
	if got := li.offset(0, 0); got != 0 {
		t.Errorf("offset(0,0) = %d, want 0 (clamped)", got)
	}
	if got := li.offset(999, 999); got != len(sample) {
		t.Errorf("offset(999,999) = %d, want %d (clamped to EOF)", got, len(sample))
	}
}

func TestLineEndAndLen(t *testing.T) {
	li := newLineIndex(sample)
	// last line has no trailing newline → ends at EOF
	if got := li.lineEndOffset(4); got != len(sample) {
		t.Errorf("lineEndOffset(4) = %d, want %d", got, len(sample))
	}
	if got := li.lineLen(3); got != len("func f() {}") {
		t.Errorf("lineLen(3) = %d, want %d", got, len("func f() {}"))
	}
	if got := li.lineLen(4); got != len("var x int") {
		t.Errorf("lineLen(4) = %d, want %d", got, len("var x int"))
	}
}

func TestEmptySource(t *testing.T) {
	li := newLineIndex("")
	if got := li.offset(1, 1); got != 0 {
		t.Errorf("offset on empty source = %d, want 0", got)
	}
}
