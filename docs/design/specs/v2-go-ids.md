# Spec: schema v2 id format for Go callables and types (codeanalyzer-go)

> **Cross-repo spec.** The python-sdk Go backend keys types and fields by the last id segment,
> so the canonical home is `codellm-devkit/.github → docs/design/specs/`, beside the
> coordinating record. Staged locally on `feat/v2-go-ids` until that repo is available (same
> precedent as `v2-l1-emission.md` and `v2-artifacts.md`). This is the design transcript of the
> datamodel loop, linked from the tracking issue, not pasted into it.

## Contract-Impact Triage

**Does this change schema v2 output?** Yes. It changes the id shape of `type`, method, and
function nodes, and of `external_symbols` keys. It does not change the module, field,
artifact, or ordinal-id shapes, and it adds no field.

| Change type | Analyzers | SDKs | Docs |
| --- | --- | --- | --- |
| Schema v2 id-format change | **codeanalyzer-go** (`internal/schema/v2/ids.go`, `v2emit`, Neo4j projector) | **python-sdk** Go backend (`cldk/analysis/go/neo4j/neo4j_backend.py` keys types and fields by last id segment; verify methods at the SDK rung) | `CLAUDE.md` § Schema decisions, `canonical-schema.md` references, this spec |

Convention anchor: `codeanalyzer-typescript/src/schema/ids.ts` (`idFromSig`: strip the module
prefix from the dotted signature, split the tail on `.`) and `codeanalyzer-python/codeanalyzer/schema/ids.py`
(`child_id`, `ordinal_id`, `external_id`, `artifact_id`).

## Problem

Go ids embed the full import-qualified signature as one segment. For a method this repeats the
package path twice:

```
can://boutique/frontend/go/middleware.go/github.com/GoogleCloudPlatform/microservices-demo/src/frontend.logHandler/github.com/GoogleCloudPlatform/microservices-demo/src/frontend.logHandler.ServeHTTP/github.com/GoogleCloudPlatform/microservices-demo/src/frontend.logHandler.ServeHTTP.closure_1@82:3/2/actual_out:2
```

The segment is not a path segment. A parser that splits on `/` (the Neo4j projector's
`lastSegment`) cannot recover a Go callable's name from its id, and the id is long and
unreadable.

## Design-loop decisions (the transcript)

Each decision was made with the user, one per question, in spine order.

### Module (unchanged)
`can://<namespace>/<app>/go/<file>`. Already the convention. No question was needed.

### Type
- **Tail = `<Type>`** (user decision, recommended). The Go package path is stripped from the
  signature, then the type name is appended to the module: `<module>/logHandler`. Precedent:
  TypeScript `idFromSig`, Python `child_id`. The package path is not repeated, since the module
  already names the scope.
- The package path that is stripped is the `pkgPath` of the symbol's signature, which equals
  the package's import path. It is the same for every symbol in one file.

### Callable
- **Function: `<module>/<Name>`**, e.g. `middleware.go/ensureSessionID`. No receiver.
- **Method: `<module>/<Type>/<Method>`** (user decision, recommended). A method nests under its
  receiver type, matching containment (`type.callables`). Precedent: TypeScript split-on-dots,
  Python `child_id`. Example: `middleware.go/logHandler/ServeHTTP`.
- **Closure: `<enclosing>/closure_N`** (user decision, recommended). N is the source-order index
  within the enclosing callable, as the analyzer numbers them today. Example:
  `middleware.go/logHandler/ServeHTTP/closure_1`. Position is not used, because the body
  ordinal key already carries it.
- **Ordinal and synthetic keys are unchanged**: `<callable-id>@<line:col>`, `@entry`, `@exit`,
  `@formal_in:<i>`, `@actual_in:<i>` / `@actual_out`, and the `/N` disambiguator for nested
  calls at one position. Nothing in those changes.

### Field
`<type-id>/<field>`, already in this shape. No change.

### External symbols (decided earlier with the user)
`<app-id>/@external/<import-path>/<Type>/<Member>`, the TypeScript `<module>/<name>` form. The
import path stays whole, since it contains dots and slashes. A package-level function is
`<app-id>/@external/<import-path>/<Name>`.

### Call body node and call-graph edge
Unchanged in shape. A call node's `callee` and a call-graph edge's endpoints take the new
callable and external ids. No new field.

### Projector: signature property (user decision)
The Neo4j `signature` property on `:GoCallable` and `:GoType` is copied from the node's JSON
`signature` field, not derived from the id. Today the projector takes the last `/` or `@`
segment of the id (`lastSegment`), which yields `Type.Method` and loses the import-qualified
name the JSON carries. The id no longer needs to carry it.

## Consequences

- **Parsing**: a Go id splits cleanly on `/`: module, then type, then method. The `@` ordinal
  suffix is unchanged.
- **Uniqueness**: ids are unique within a file. Go forbids duplicate package-scope identifiers,
  and forbids a duplicate method name on one type. Two files in one package have distinct
  module ids, so the stripped package path never causes a collision.
- **Cross-file methods** (amended 2026-11-20, see Amendment below): a method declared in another
  file gets an id under the file that declares it, `<declaring-module>/<Type>/<Method>`. The
  method node stays in its receiver type's `callables{}`, and `source_file` names the declaring
  file for text recovery.
- **python-sdk**: `cldk/analysis/go/neo4j/neo4j_backend.py` keys types by `ty.id.split("/")[-1]`,
  which still returns the type name, and fields by `fld.id.split("/")[-1]`, which is unchanged.
  Callables keyed by a dotted sig will change meaning: the last segment is now the bare method
  name. The SDK rung must verify this.

## Open items (flagged for review)

1. **Name collision inside a file**: a type `X` and a function `X` cannot share a file's package
   scope, so the ids cannot collide. Confirm no other Go construct gives two nodes the same
   `<module>/<Type>/<Method>` (a type's method set is unique by name). Expected: none.
2. **Generic types**: a generic type's signature includes type parameters. The `[...]` part is
   not part of the segment name. Decide at implementation: strip the type parameters (default)
   or keep them.
3. **Dot-free function names in `external_symbols` keys**: keys become `<import-path>/<Type>/<Member>`.
   Confirm the key is still unique when two packages share a last path element.

## Tracking

One issue in `codeanalyzer-go`, rungs as checklist items (user decision). Base branch
`feat/artifacts`, since the artifact code is the base for this work (user decision). Analyzer
ships first; the python-sdk Go backend follows once the id shape is released.

## Implementation notes (2026-11-20)

- **Type signature field (amended)**: the spec said this change adds no field. The Neo4j
  `:GoType.signature` needs the full name, which the type id no longer carries, so `Type`
  gained a `signature` JSON field (the full `signatureOf()`), mirroring `Callable`. The
  projector copies it. Decided with the user.
- **Member name rule**: implemented as "the part after the last dot of the signature". This
  is the same as stripping the import path, since Go identifiers contain no dots.
- **Open item 2 (generics) resolved**: `signatureOf()` has no type parameters, so a generic
  type `Set[T]` ids as `set/set.go/Set` and its method as `set/set.go/Set/Add`.
- **Open item 1 (collisions) resolved**: the generics fixture and the chi/init fixtures
  produce distinct ids. `init` functions are distinct per file by module.

## Amendment (2026-11-20): cross-file method ids

Decided with the user in the design loop, after the id change landed.

**Problem.** A method is nested under its receiver type's module, so its id names the file
where the receiver lives, not the file where the method is declared. Example: `frontendServer.logoutHandler`
is declared in `handlers.go` but its id was `…/main.go/frontendServer/logoutHandler`. 22 methods in the
frontend app are affected (those with `source_file` set).

**Decisions.**

1. **Id.** A callable's id contains the file where it is declared:
   `<declaring-module>/<Type>/<Method>`, e.g. `…/handlers.go/frontendServer/logoutHandler`. The type
   segment stays. Rejected: `<declaring-module>/<Method>`, because a method and a package-level
   function of the same name in one file would collide.
2. **JSON containment.** The node stays in its receiver type's `callables{}` (containment by
   receiver is unchanged). Only its id changes. Moving it under the declaring module was rejected
   because it changes containment for every consumer and hides which type the method belongs to.
3. **Text recovery.** `source_file` is kept. The id's module now equals `source_file`, but the
   node still nests under the receiver's module, so its span still needs `source_file` to resolve.
4. **Neo4j.** `GO_HAS_METHOD` from the receiver `:GoType` is kept. A `GO_DECLARES` edge is added from
   the declaring `:GoModule` to the method, so "which file declares this method" is one query. No
   new relationship kind.

**Release.** Same PR and release as the id format (feat/v2-go-ids). Tracked in #46.

## Out of scope

- Module, field, artifact, and ordinal-id shapes.
- Any new node kind, edge kind, or field.
- Renaming the `signature` field (kept as-is).
