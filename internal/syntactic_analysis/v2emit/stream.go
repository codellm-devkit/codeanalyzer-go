package v2emit

import (
	"iter"

	"github.com/codellm-devkit/codeanalyzer-go/internal/dataflow"
	"github.com/codellm-devkit/codeanalyzer-go/internal/schema"
	v2 "github.com/codellm-devkit/codeanalyzer-go/internal/schema/v2"
	"github.com/codellm-devkit/codeanalyzer-go/internal/sdg"
)

// Stream is the Emit payload as a module stream that a caller can walk more than one time.
// Each Modules walk emits the modules again, so no walk holds the whole tree.
// A Stream is not safe for concurrent use.
type Stream struct {
	app       *schema.GoApplication
	e         *emitter
	env       *v2.Analysis
	walked    bool
	lateCache lateFields
	lateDone  bool
}

// Open prepares a Stream. It takes the same arguments as Emit.
func Open(app *schema.GoApplication, identity v2.Identity, projectDir string, maxLevel int, analyzerVersion string, kLimit int, l3 map[dataflow.L3Key]dataflow.Result, l4 *sdg.Result) *Stream {
	e := newEmitter(app, identity, projectDir, maxLevel, l3, l4)
	return &Stream{app: app, e: e, env: e.envelope(app, maxLevel, analyzerVersion, kLimit)}
}

// Envelope returns the payload without symbol_table, param_in, param_out and degraded.
func (s *Stream) Envelope() *v2.Analysis { return s.env }

// ModuleKeys returns the sorted symbol_table keys.
func (s *Stream) ModuleKeys() []string { return sortedFileKeys(s.app.SymbolTable) }

// Modules emits each module in symbol_table key order.
// The first complete walk fills the L4 id map. Later walks print no warnings.
func (s *Stream) Modules() iter.Seq2[string, v2.Module] {
	return func(yield func(string, v2.Module) bool) {
		if s.walked {
			s.e.sdg.setQuiet()
		}
		for _, relPath := range sortedFileKeys(s.app.SymbolTable) {
			if !yield(relPath, s.e.module(relPath, s.app.SymbolTable[relPath])) {
				return
			}
		}
		s.walked = true
	}
}

// ParamIn streams application.param_in. Call it after one complete Modules walk.
func (s *Stream) ParamIn() iter.Seq[v2.ParamEdge] { return orEmpty(s.late().paramIn) }

// ParamOut streams application.param_out. Call it after one complete Modules walk.
func (s *Stream) ParamOut() iter.Seq[v2.ParamEdge] { return orEmpty(s.late().paramOut) }

// Degraded returns the envelope degraded list. Call it after one complete Modules walk.
func (s *Stream) Degraded() []v2.Degraded { return s.late().degraded }

// late mints the param lists and the degraded list once, after the first walk.
func (s *Stream) late() lateFields {
	if !s.walked {
		panic("v2emit: Stream late fields read before one complete Modules walk")
	}
	if s.lateDone {
		return s.lateCache
	}
	s.lateCache, s.lateDone = s.e.sdg.late(), true
	return s.lateCache
}

// orEmpty replaces a nil sequence (no L4) with an empty one, so a range over it is safe.
func orEmpty(seq iter.Seq[v2.ParamEdge]) iter.Seq[v2.ParamEdge] {
	if seq == nil {
		return func(func(v2.ParamEdge) bool) {}
	}
	return seq
}
