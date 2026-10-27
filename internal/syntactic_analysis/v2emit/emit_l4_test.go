package v2emit

import (
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/sdg"
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

// TestL4Overlay_DuplicateKeyKeepsSmallerID: two callables sharing a target
// key (declaring file and signature) route its param edges and degraded
// entries to the lexically smaller id, whichever is applied first —
// emission walks maps, so last-write-wins would be nondeterministic — and
// the clash is warned once, naming both ids.
func TestL4Overlay_DuplicateKeyKeepsSmallerID(t *testing.T) {
	key := dataflow.L3Key{Path: "a.go", Signature: "example.com/a.F"}
	small, big := "can://app/go/a.go/example.com/a.F", "can://app/go/b.go/example.com/a.F"
	var warned []string
	prev := warnf
	warnf = func(format string, args ...any) { warned = append(warned, fmt.Sprintf(format, args...)) }
	defer func() { warnf = prev }()
	for _, order := range [][]string{{small, big}, {big, small}} {
		warned = nil
		o := newL4Overlay(4, &sdg.Result{}, sigIndex{})
		for _, id := range order {
			o.apply(&v2.Callable{ID: id, Body: map[string]v2.BodyNode{}}, key)
		}
		if got := o.ids[sdg.Target{Callable: key}]; got != small {
			t.Errorf("apply order %v: target routes to %s, want %s", order, got, small)
		}
		if len(warned) != 1 || !strings.Contains(warned[0], small) || !strings.Contains(warned[0], big) ||
			!strings.Contains(warned[0], "share the target key") {
			t.Errorf("apply order %v: warnings %q, want one naming both ids", order, warned)
		}
	}
}
