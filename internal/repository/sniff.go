package repository

import (
	"bytes"
	"crypto/sha256"
	"hash"
	"unicode/utf8"
)

// sniff hashes a file and decides whether it is binary in one streaming pass. It
// is an io.Writer, so io.Copy feeds it in fixed-size chunks and it never holds
// the file. Its answers equal the whole-file ones: binary means a NUL byte
// anywhere, or invalid UTF-8 anywhere, the rule formatOf has always applied.
//
// Validation is chunk-safe. A rune split across two Writes is completed from the
// carry, and at most utf8.UTFMax-1 bytes are ever carried, so the sniffer makes no
// allocation per chunk.
type sniff struct {
	hash    hash.Hash
	size    int64
	hasNUL  bool
	badUTF8 bool
	// carry holds the incomplete UTF-8 sequence at the end of the last chunk.
	carry []byte
}

func newSniff() *sniff {
	return &sniff{hash: sha256.New()}
}

// Write consumes one chunk. It never fails; io.Writer requires the error result.
func (s *sniff) Write(p []byte) (int, error) {
	n := len(p)
	s.size += int64(n)
	s.hash.Write(p)
	if !s.hasNUL && bytes.IndexByte(p, 0) >= 0 {
		s.hasNUL = true
	}
	if s.badUTF8 {
		return n, nil
	}

	// Finish a rune that started in the previous chunk. The completed rune is
	// decoded from a fixed-size array, not from p, so a split rune costs no
	// allocation.
	if len(s.carry) > 0 {
		var tmp [utf8.UTFMax]byte
		m := copy(tmp[:], s.carry)
		k := 0
		for m < utf8.UTFMax && k < len(p) && !utf8.FullRune(tmp[:m]) {
			tmp[m] = p[k]
			m++
			k++
		}
		if !utf8.FullRune(tmp[:m]) {
			// Still incomplete, and the chunk is used up: keep carrying.
			s.carry = append(s.carry[:0], tmp[:m]...)
			return n, nil
		}
		if r, sz := utf8.DecodeRune(tmp[:m]); r == utf8.RuneError && sz == 1 {
			s.badUTF8 = true
			return n, nil
		}
		s.carry = s.carry[:0]
		p = p[k:]
	}

	// p now starts on a rune boundary. Validate whole runes; an incomplete tail
	// becomes the new carry.
	i := 0
	for i < len(p) {
		if p[i] < utf8.RuneSelf {
			i++
			continue
		}
		if !utf8.FullRune(p[i:]) {
			break
		}
		r, sz := utf8.DecodeRune(p[i:])
		if r == utf8.RuneError && sz == 1 {
			s.badUTF8 = true
			return n, nil
		}
		i += sz
	}
	s.carry = append(s.carry[:0], p[i:]...)
	return n, nil
}

// finish closes the stream. A sequence still incomplete at EOF is invalid UTF-8.
func (s *sniff) finish() {
	if len(s.carry) > 0 {
		s.badUTF8 = true
	}
}

// binary reports the classification. Valid after finish.
func (s *sniff) binary() bool {
	return s.hasNUL || s.badUTF8
}

// sum is the SHA-256 of everything written.
func (s *sniff) sum() []byte {
	return s.hash.Sum(nil)
}
