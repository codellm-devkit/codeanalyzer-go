<!--
DRAFT second child issue body — file on codellm-devkit/codeanalyzer-go when this unit is picked up.
  gh issue create --repo codellm-devkit/codeanalyzer-go \
    --title "Emit the v2 L2 call_graph: backfill `callee` ids and `{src,dst,prov}` edges" \
    --label enhancement --body-file this-file
Then attach as a sub-issue of epic codellm-devkit/.github#94 (by child id, not number).
Sections below are the feature_request.md form, verbatim order, none added/removed.
-->

**Is your feature request related to a problem? Please describe.**

L1 emits the v2 containment tree with `call` body nodes whose `callee` is `null` — the
sanctioned refinement slot. L2 fills it: resolve each call site and each call-graph edge
endpoint from a v1 `signatureOf()` string to the durable `can://` id of the callable it
names. L2 is pure refinement over the L1 tree — it computes no new facts, it only
translates identity. The parser/resolver stay; only the emission layer gains the
signature→id index (golden rule: replace what serializes, keep what computes).

**Describe the solution you'd like**

- [ ] `buildSigIndex` maps every v1 callable signature (functions, methods, nested closures) → its `can://` id, using the SAME id builders `emitCallable` uses (byte-identical ids)
- [ ] `callee` on each `call` body node backfilled from `null` to the resolved `can://` id
- [ ] `call_graph` emitted at application scope as `[{src, dst, prov, weight}]`
- [ ] External/stdlib callees (outside the project) resolve to no endpoint — never a dangling edge
- [ ] L2 gate green: every edge endpoint and every non-null `callee` resolves to a real in-tree node id
- [ ] Determinism: `call_graph` edge order stable across runs (sorted before emit)

**Describe alternatives you've considered**

Does NOT compute any new call-graph facts — it reuses the v1 call graph and only
translates endpoints to `can://` ids. Does NOT emit L3/L4 `body` statements or edges.

**Additional context**

- v1 signatures are globally unique (a method signature embeds its receiver), so the flat signature→id map has no collisions.
- The index mirrors `emitModule`/`emitType`/`emitCallable` parent-id computation exactly; drift there would silently dangle endpoints.
- Determinism risk: edge order was nondeterministic before sorting — the determinism gate fails without a stable sort.
- Depends on the L1 child: L2 refines the tree L1 produces; file/land L1 first.
- Full transcript in the spec; schema decisions in repo `CLAUDE.md`.
