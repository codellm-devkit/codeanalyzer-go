package sdg

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/codellm-devkit/codeanalyzer-go/internal/utils"
)

// warnf logs a warning (a test seam over utils.Warn).
var warnf = utils.Warn

// LoadModels reads a --library-models file (spec §6): a JSON object whose
// keys are the external id's <import-path>/<sig> tail (`strconv/Atoi`,
// `database/sql/DB.Query`) and whose values are lists of [in, out] pairs of
// `of` strings (`"s"`, `"_0"`, `"_recv"`, `"<return>"`, `"<return>:1"`, a
// by-ref param's name). An empty list is no flow. An empty path gives
// Models{}.
//
// Only the shape is checked here: which `of`s an external has is known once
// its formals are, so a pair naming one it lacks is reported by the summary
// pass (opaqueFlows), which then keeps that external's default.
func LoadModels(path string) (Models, error) {
	if path == "" {
		return Models{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--library-models: %w", err)
	}
	m, err := parseModels(data)
	if err != nil {
		return nil, fmt.Errorf("--library-models %s: %w", path, err)
	}
	return m, nil
}

func parseModels(data []byte) (Models, error) {
	var raw map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("data after the top-level object")
	}
	if raw == nil {
		return nil, errors.New("want a JSON object, got null")
	}
	out := make(Models, len(raw))
	for key, v := range raw {
		var pairs [][]string
		if err := json.Unmarshal(v, &pairs); err != nil {
			return nil, fmt.Errorf("%s: want a list of [in, out] pairs: %w", key, err)
		}
		if pairs == nil {
			return nil, fmt.Errorf("%s: want a list of [in, out] pairs, got null", key)
		}
		flows := make([][2]string, 0, len(pairs))
		for _, p := range pairs {
			if len(p) != 2 {
				return nil, fmt.Errorf("%s: pair %q: want [in, out]", key, p)
			}
			flows = append(flows, [2]string{p[0], p[1]})
		}
		out[key] = flows
	}
	return out, nil
}

// modelKey is the Models key of external t: <import-path>/<sig>.
func (b *sdgBuilder) modelKey(t Target) (string, bool) {
	e, ok := b.p.fx.ext[t.External]
	if !ok {
		return "", false
	}
	return e.ImportPath + "/" + e.Sig, true
}

// model returns the Models entry of stand-in t, if any.
func (b *sdgBuilder) model(t Target) ([][2]string, string, bool) {
	if t.External == "" || len(b.in.Models) == 0 {
		return nil, "", false
	}
	key, ok := b.modelKey(t)
	if !ok {
		return nil, "", false
	}
	m, ok := b.in.Models[key]
	return m, key, ok
}

// modelFlows is stand-in t's flows under its model, or false when it has
// none or the model names an `of` t lacks: the caller then keeps the
// all-to-all default (a typo must never refute a flow). The unknown `of`s
// are one warning.
func (b *sdgBuilder) modelFlows(t Target, fi *formalInfo) (*flowSet, bool) {
	model, key, ok := b.model(t)
	if !ok {
		return nil, false
	}
	b.modelled[key] = true
	f := newFlowSet(len(fi.inOf), len(fi.outOf))
	var unknown []string
	for _, pair := range model {
		i, j := slices.Index(fi.inOf, pair[0]), slices.Index(fi.outOf, pair[1])
		if i < 0 {
			unknown = append(unknown, "in "+pair[0])
		}
		if j < 0 {
			unknown = append(unknown, "out "+pair[1])
		}
		if i >= 0 && j >= 0 {
			f.set(i, j)
		}
	}
	if len(unknown) > 0 {
		warnf("--library-models: %s has no formal %v (formal_in %v, formal_out %v); keeping its default, every input to every output",
			key, unknown, fi.inOf, fi.outOf)
		return nil, false
	}
	return f, true
}

// unusedModels warns, once per key and in sorted key order, of every Models
// entry no targeted external has: an uncalled external, or a misspelt key
// (`strconv.Atoi` for `strconv/Atoi`), which would otherwise silently keep
// the default.
func (b *sdgBuilder) unusedModels() {
	keys := make([]string, 0, len(b.in.Models))
	for key := range b.in.Models {
		if !b.modelled[key] {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		warnf("--library-models: no call site targets %s; entry ignored (keys are <import-path>/<sig>, e.g. strconv/Atoi)", key)
	}
}
