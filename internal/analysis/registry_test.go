package analysis

// Tests for orderPasses (unexported — must be in the same package).
//
// We use lightweight stub passes so these tests have no external dependencies
// and run without loading any Go source files.

import (
	"strings"
	"testing"

	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
)

// stubPass is a minimal AnalysisPass for testing orderPasses.
type stubPass struct {
	name     string
	provides []string
	requires []string
}

func (s *stubPass) Name() string     { return s.name }
func (s *stubPass) Provides() []string { return s.provides }
func (s *stubPass) Requires() []string { return s.requires }
func (s *stubPass) Run(_ *schema.GoApplication, _ AnalysisContext) (AnalysisResult, error) {
	return AnalysisResult{}, nil
}

func mkPass(name string, provides, requires []string) AnalysisPass {
	return &stubPass{name: name, provides: provides, requires: requires}
}

// ── orderPasses ───────────────────────────────────────────────────────────────

func TestOrderPasses_Empty(t *testing.T) {
	ordered, err := orderPasses(nil)
	if err != nil {
		t.Fatalf("empty passes: unexpected error: %v", err)
	}
	if len(ordered) != 0 {
		t.Errorf("got %d passes, want 0", len(ordered))
	}
}

func TestOrderPasses_SingleNoDeps(t *testing.T) {
	p := mkPass("solo", []string{"x"}, nil)
	ordered, err := orderPasses([]AnalysisPass{p})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ordered) != 1 || ordered[0].Name() != "solo" {
		t.Errorf("expected [solo]; got %v", names(ordered))
	}
}

// A → B: A provides "feat", B requires "feat".  A must come before B.
func TestOrderPasses_LinearDependency(t *testing.T) {
	a := mkPass("a", []string{"feat"}, nil)
	b := mkPass("b", nil, []string{"feat"})
	// Deliver in reverse order to stress the sort.
	ordered, err := orderPasses([]AnalysisPass{b, a})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ordered) != 2 {
		t.Fatalf("got %d passes, want 2", len(ordered))
	}
	if ordered[0].Name() != "a" || ordered[1].Name() != "b" {
		t.Errorf("wrong order: got %v, want [a b]", names(ordered))
	}
}

// A → C ← B: two independent passes both provide something C needs.
func TestOrderPasses_DiamondDependency(t *testing.T) {
	a := mkPass("a", []string{"x"}, nil)
	b := mkPass("b", []string{"y"}, nil)
	c := mkPass("c", nil, []string{"x", "y"})
	ordered, err := orderPasses([]AnalysisPass{c, b, a})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ordered) != 3 {
		t.Fatalf("got %d passes, want 3", len(ordered))
	}
	// c must be last.
	if ordered[len(ordered)-1].Name() != "c" {
		t.Errorf("c should be last; got %v", names(ordered))
	}
}

func TestOrderPasses_UnsatisfiedRequirement(t *testing.T) {
	p := mkPass("needy", nil, []string{"missing-cap"})
	_, err := orderPasses([]AnalysisPass{p})
	if err == nil {
		t.Fatal("expected error for unsatisfied requirement, got nil")
	}
	if !strings.Contains(err.Error(), "needy") {
		t.Errorf("error should mention the blocked pass name; got %q", err.Error())
	}
}

func TestOrderPasses_Cycle(t *testing.T) {
	// A requires "b", B requires "a" — neither can run.
	a := mkPass("a", []string{"a-cap"}, []string{"b-cap"})
	b := mkPass("b", []string{"b-cap"}, []string{"a-cap"})
	_, err := orderPasses([]AnalysisPass{a, b})
	if err == nil {
		t.Fatal("expected error for cycle, got nil")
	}
}

// ── RunPipeline with empty registry ──────────────────────────────────────────

func TestRunPipeline_EmptyRegistry(t *testing.T) {
	// Save and restore to avoid affecting other tests.
	old := registeredPasses
	registeredPasses = nil
	defer func() { registeredPasses = old }()

	app := &schema.GoApplication{
		Entrypoints: map[string][]schema.GoEntrypoint{},
		CallGraph:   []schema.GoCallEdge{},
	}
	if err := RunPipeline(app, AnalysisContext{}); err != nil {
		t.Fatalf("RunPipeline with empty registry: %v", err)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func names(passes []AnalysisPass) []string {
	out := make([]string, len(passes))
	for i, p := range passes {
		out[i] = p.Name()
	}
	return out
}
