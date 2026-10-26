package v2emit

import (
	"math/rand/v2"
	"strings"
	"testing"

	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
)

// TestCompareOrdinal_MatchesMintedIds: compareOrdinal orders (owner, key)
// pairs exactly as their minted ids v2.GlobalOrdinal(owner, key) compare,
// including an owner that is a prefix of the other (a callable and its
// closure) and the "@"-tag keys, whose ids carry no extra "@".
func TestCompareOrdinal_MatchesMintedIds(t *testing.T) {
	owners := []string{
		"can://a/go/f.go/p.F", "can://a/go/f.go/p.F/p.F.closure_1", "can://a/go/f.go/p.F2",
		"can://a/go/f.go/p.F@", "can://a/@external/fmt/Sprintf", "can://a/@external/fmt",
		"can://a/go/f.go/T/p.T.M", "",
	}
	keys := []string{"@formal_in:0", "@formal_in:10", "@formal_out", "@entry", "12:3/actual_in:0", "12:3/2/actual_out:1", "9:1", "", "@", "/x"}
	r := rand.New(rand.NewPCG(1, 2))
	pick := func(s []string) string { return s[r.IntN(len(s))] }
	for range 20000 {
		o1, k1, o2, k2 := pick(owners), pick(keys), pick(owners), pick(keys)
		want := strings.Compare(v2.GlobalOrdinal(o1, k1), v2.GlobalOrdinal(o2, k2))
		if got := compareOrdinal(o1, k1, o2, k2); got != want {
			t.Fatalf("compareOrdinal(%q, %q, %q, %q) = %d, want %d", o1, k1, o2, k2, got, want)
		}
	}
}
