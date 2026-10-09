// Package v2emit transforms the v1 in-memory model (internal/schema) into the
// v2 canonical schema (internal/schema/v2). It is an EMISSION layer only: it
// computes no new facts beyond source byte offsets, it just re-serializes what
// the parser/resolver already produced into the additive-tree shape.
//
// The one genuinely new datum is span byte offsets. The v1 model records
// line/col but not UTF-8 byte offsets (and only line-level positions for
// callables/types). Rather than change the v1 model, the emitter reads each
// file's source once and derives byte offsets from the existing line/col
// positions via a lineIndex. That same source string is emitted once per module
// as module.source, off which every node's text slices.
package v2emit

// lineIndex maps 1-based (line, col) positions in a source file to UTF-8 byte
// offsets. Built once per file. Columns are byte columns within the line, which
// is what go/token.Position reports (Position.Column counts bytes, not runes),
// so no rune/byte reconciliation is needed here.
type lineIndex struct {
	source string
	// lineStart[i] is the byte offset where 1-based line (i+1) begins.
	lineStart []int
}

// newLineIndex builds a lineIndex over the given source text.
func newLineIndex(source string) *lineIndex {
	// Line 1 starts at offset 0; each '\n' begins the next line at the byte
	// immediately after it.
	starts := make([]int, 0, 1+countByte(source, '\n'))
	starts = append(starts, 0)
	for i := 0; i < len(source); i++ {
		if source[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &lineIndex{source: source, lineStart: starts}
}

// offset converts a 1-based (line, col) position to a byte offset into source.
// col is a 1-based byte column (go/token semantics). Positions outside the
// file's range are clamped to [0, len(source)] so a bad position yields a
// harmless in-range offset rather than a panic.
func (li *lineIndex) offset(line, col int) int {
	if len(li.lineStart) == 0 {
		return 0
	}
	if line < 1 {
		line = 1
	}
	if line > len(li.lineStart) {
		line = len(li.lineStart)
	}
	base := li.lineStart[line-1]
	if col < 1 {
		col = 1
	}
	off := base + (col - 1)
	if off > len(li.source) {
		off = len(li.source)
	}
	return off
}

// lineEndOffset returns the byte offset of the end of a 1-based line: the
// position just before its terminating '\n' (or end of file for the last line).
// Used to give a line-only position (no column in the v1 model) a precise end.
func (li *lineIndex) lineEndOffset(line int) int {
	if line < 1 || len(li.lineStart) == 0 {
		return 0
	}
	if line >= len(li.lineStart) {
		return len(li.source)
	}
	// Next line begins one byte after this line's '\n'; back up over the '\n'.
	return li.lineStart[line] - 1
}

// lineLen returns the number of bytes on a 1-based line, excluding the newline.
// It gives a line-only end position a byte column (used to synthesize end.col).
func (li *lineIndex) lineLen(line int) int {
	if line < 1 || line > len(li.lineStart) {
		return 0
	}
	return li.lineEndOffset(line) - li.lineStart[line-1]
}

func countByte(s string, b byte) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			n++
		}
	}
	return n
}
