package repository

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"unicode/utf8"
)

// Item 5: the streamed hash and binary sniff must equal the whole-file answer for
// every chunking, including a multi-byte rune split across a chunk boundary.
func TestSniff_MatchesWholeFileForEveryChunking(t *testing.T) {
	inputs := [][]byte{
		[]byte("plain ascii\n"),
		[]byte("héllo wörld ✓ 世界 🙂\n"), // multi-byte runes
		[]byte("nul\x00inside\n"),
		[]byte("bad utf8 \xff\xfe here\n"),
		[]byte("truncated rune at end \xe4\xb8"), // incomplete sequence at EOF
		{},
	}
	for _, in := range inputs {
		wantBinary := bytes.IndexByte(in, 0) >= 0 || !utf8.Valid(in)
		wantSum := sha256.Sum256(in)
		for chunk := 1; chunk <= len(in)+1; chunk++ {
			s := newSniff()
			for off := 0; off < len(in); off += chunk {
				end := min(off+chunk, len(in))
				if _, err := s.Write(in[off:end]); err != nil {
					t.Fatal(err)
				}
			}
			s.finish()
			if s.binary() != wantBinary {
				t.Errorf("chunk=%d input=%q: binary=%v, want %v", chunk, in, s.binary(), wantBinary)
			}
			if got := hex.EncodeToString(s.sum()); got != hex.EncodeToString(wantSum[:]) {
				t.Errorf("chunk=%d input=%q: sha256 mismatch", chunk, in)
			}
		}
	}
}
