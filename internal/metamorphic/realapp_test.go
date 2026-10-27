//go:build metamorphic

package metamorphic

// The metamorphic relations on the prepared eval apps (scripts/eval_l3.sh
// --prepare memos nats-server), run by scripts/metamorphic.sh:
//
//	go test -tags metamorphic -run TestMR_RealApps ./internal/metamorphic/
//
// Each app is $EVAL_SRC/<name> (default /tmp/eval). A missing app is
// skipped. MR_APPS (space-separated) picks the apps, MR_RELATIONS the
// relations (all when unset). Each relation samples 20 files and 50
// callables with seed 1.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func realTargets(t *testing.T) []*target {
	src := os.Getenv("EVAL_SRC")
	if src == "" {
		src = "/tmp/eval"
	}
	names := strings.Fields(os.Getenv("MR_APPS"))
	if len(names) == 0 {
		names = []string{"memos", "nats-server"}
	}
	var out []*target
	for _, n := range names {
		dir := filepath.Join(src, n)
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
			t.Logf("%s: not prepared at %s; skipped", n, dir)
			continue
		}
		out = append(out, &target{name: n, dir: dir, nFiles: 20, nCallables: 50})
	}
	return out
}

func TestMR_RealApps(t *testing.T) {
	tgs := realTargets(t)
	if len(tgs) == 0 {
		t.Skip("no prepared eval app under $EVAL_SRC")
	}
	want := strings.Fields(os.Getenv("MR_RELATIONS"))
	pick := func(name string) bool { return len(want) == 0 || slices.Contains(want, name) }
	for _, tg := range tgs {
		t.Run(tg.name, func(t *testing.T) {
			tg.load(t)
			t.Logf("MRLOAD %s files=%d eligible=%d skipped=%v analysis=%s",
				tg.name, len(tg.ws.Files), len(tg.ws.Eligible()), tg.ws.SkipCounts(), tg.pre.dur.Round(1e6))
			for _, r := range relations {
				if !pick(r.name) {
					continue
				}
				t.Run(r.name, func(t *testing.T) {
					if why, ok := knownSkips[r.name+"/"+tg.name]; ok {
						t.Skip(why)
					}
					o := runRelation(t, tg, r)
					logStat(t, r.name, tg.name, o)
					for _, v := range o.violations {
						t.Errorf("%s on %s: %s", r.name, tg.name, v)
					}
				})
			}
			if pick("MR15") {
				t.Run("MR15", func(t *testing.T) {
					o := runMR15(t, tg)
					logStat(t, "MR15", tg.name, o)
					for _, v := range o.violations {
						t.Errorf("MR15 on %s: %s", tg.name, v)
					}
				})
			}
		})
	}
}
