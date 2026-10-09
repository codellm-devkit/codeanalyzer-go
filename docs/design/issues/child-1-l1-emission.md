<!--
DRAFT first child issue body — file on codellm-devkit/codeanalyzer-go when this unit is picked up.
  gh issue create --repo codellm-devkit/codeanalyzer-go \
    --title "Emit the v2 L1 tree: `can://` ids, `span`, `source`, `body{}` call nodes" \
    --label enhancement --body-file this-file
Then attach as a sub-issue of the parent (by child id, not number). File just-in-time,
i.e. when starting L1 — NOT alongside the parent.
Sections below are the feature_request.md form, verbatim order.
-->

**Is your feature request related to a problem? Please describe.**

The Go analyzer serializes the v1 shape. L1 is the structural keystone of the v2
migration: it introduces the additive containment tree and every Group A identity
decision the later levels build on. Do L1 first and get the symbol-table gate green
before touching the L2 call graph. The parser/resolver stay; only the emission layer
changes (golden rule: replace what serializes, keep what computes).

**Describe the solution you'd like**

- [ ] `application.id` = `can://go/<app>`, `<app>` = `--app-name` (default: input dir base name)
- [ ] Durable `can://` ids on module/type/callable; `signatureOf()` is the last segment
- [ ] `span: {start:[l,c], end:[l,c], bytes:[from,to]}` with UTF-8 byte offsets; drop flat `start_line`/`end_line`
- [ ] `source` emitted once per module; drop per-callable `code`
- [ ] `type.kind` ∈ `struct|interface|alias|defined`; methods grouped under receiver type's `callables{}`
- [ ] `base_types[]` = embedded ids, `interfaces[]` = computed structural satisfaction
- [ ] `body{}` holds `call` nodes with `callee:null`, typed `is_goroutine`/`is_deferred`; closures nest in `callables{}`

**Describe alternatives you've considered**

Does NOT emit the `call_graph` (that is the L2 child), nor any L3/L4 `body` statements
or edges. `body{}` at L1 holds only `call` nodes.

**Additional context**

- Emitter approach: walk the existing in-memory model, add a v2 emitter behind a flag; keep v1 emitter as the compat shim during transition.
- One genuinely new datum: thread parser byte offsets into `span.bytes`.
- Structural interface satisfaction needs method-set matching — cost lives here, not in the resolver.
- Validate each node against the SDK v2 Go models as they land; superset check vs v1 output.
- Schema decisions recorded in repo `CLAUDE.md`; full transcript in the spec.
