#!/usr/bin/env python3
"""l3_metrics.py - L3 (body / cfg / cdg / ddg) metrics and consistency checks.

Usage:
    python3 -I scripts/l3_metrics.py LABEL=PATH/analysis.json [LABEL=PATH ...] > metrics.jsonl

Prints one JSON object per input: structure counts, body-node and cfg-edge kind breakdowns,
switch / select / type-switch fan-out, defer-chain and exception-edge shape, the per-callable
DDG distribution and its top callables, `var` classes (wildcard, `<defer>:` pseudo path), DDG
volume by generated-file class and by callable name (e.g. protobuf `Marshal*` / `Unmarshal`), approximate bytes per section, and the internal-consistency
checks. On an `-a 4` file it adds an `l4` object: vertices by kind and by `of` class, param_in /
param_out, summary edges (and how many are all-to-all at their call site), port / points-to ddg,
stand-ins and their formals, the share of call nodes with a VTA call_graph target, the degraded
codes, and the L4 consistency checks (no dangling endpoint, no global → global summary). The figures in docs/design/notes/l3-dataflow-metrics.md come from this script. It loads
each file whole: a 1.1 GB analysis.json needs about 11 GB of RAM.
"""
import json
import os
import re
import sys
from collections import Counter, defaultdict, deque

CFG_KINDS = ["fallthrough", "true", "false", "switch_case", "loop_back", "exception",
             "return", "break", "continue", "defer_resume"]
NODE_KINDS = ["call", "statement", "branch", "loop", "switch", "return", "entry", "exit"]
VERTEX_KINDS = ("formal_in", "formal_out", "actual_in", "actual_out")  # L4
IF_ERR = re.compile(rb"^if\s+(?:[^{]*;\s*)?err\s*!=\s*nil\s*\{")


def callables(st):
    """Yields (callable, is_closure, module, file its spans index). A closure inside a
    cross-file method carries no source_file of its own; it inherits the method's."""
    def nested(c, f, closure):
        for x in (c.get("callables") or {}).values():
            g = x.get("source_file") or f
            yield x, closure, g
            yield from nested(x, g, True)
    for mpath, mod in st.items():
        for fn in (mod.get("functions") or {}).values():
            f = fn.get("source_file") or mpath
            yield fn, False, mpath, f
            for x, cl, g in nested(fn, f, True):
                yield x, cl, mpath, g
        for t in (mod.get("types") or {}).values():
            for x, cl, g in nested(t, mpath, False):
                yield x, cl, mpath, g


def file_class(path):
    if path.endswith(".pb.go") or ".pb." in path:
        return "protobuf"
    if path.endswith(".eg.go"):
        return "execgen"
    if ("easyjson" in path or path.endswith("_gen.go") or "/gen/" in path
            or os.path.basename(path).startswith("zz_generated")):
        return "other-generated"
    return "hand-written"


def var_class(v):
    if v.startswith("<defer>:"):
        return "defer"
    if v.startswith("<"):
        return "other-pseudo"
    if "[*]" in v and v.endswith(".*"):
        return "elem+deref"
    if "[*]" in v:
        return "elem"
    if v.endswith(".*"):
        return "deref/k-limit"
    return "plain"


def pct(xs, q):
    return xs[min(len(xs) - 1, int(q * (len(xs) - 1) + 0.5))] if xs else 0


def analyze(label, path):
    with open(path, "rb") as fh:
        doc = json.load(fh)
    st = doc["application"]["symbol_table"]
    C = Counter()          # scalar counters
    K = defaultdict(Counter)  # keyed counters
    chk = Counter()        # consistency-check failures
    fanout = defaultdict(list)
    ddg_per, top = [], []
    src_cache = {}

    def src(f):
        if f not in src_cache:
            src_cache[f] = (st.get(f, {}).get("source") or "").encode("utf-8")
        return src_cache[f]

    C["source_bytes"] = sum(len((mod.get("source") or "").encode("utf-8")) for mod in st.values())
    for mod in st.values():
        for t in (mod.get("types") or {}).values():
            K["types_by_kind"][t.get("kind")] += 1

    for c, closure, mpath, sfile in callables(st):
        K["callables_by_kind"]["closure" if closure else c.get("kind")] += 1
        # A closure inside a cross-file method: its spans index the method's declaring file,
        # but it carries no source_file, so the text-recovery rule would slice the wrong file.
        C["closures_spans_in_other_file_without_source_file"] += (
            closure and sfile != mpath and not c.get("source_file"))
        if "cfg" not in c:
            K["no_cfg"][c.get("kind") + (" with body" if c.get("body") else " no body")] += 1
            continue
        C["with_cfg"] += 1
        # The L3 figures leave out the L4 overlay (port vertices, reaching-defs / points-to
        # ddg), so they compare across -a 3 and -a 4; analyze_l4 counts the overlay.
        body = {k: n for k, n in (c.get("body") or {}).items() if n.get("kind") not in VERTEX_KINDS}
        cfg, cdg = c.get("cfg") or [], c.get("cdg") or []
        ddg = [e for e in c.get("ddg") or [] if e.get("prov") == ["ssa"]]
        if len(body) == 2:
            C["entry_exit_only_bodies"] += 1

        sats = {k for k, n in body.items() if "parent" in n}
        for k, n in body.items():
            K["node_kinds"][n.get("kind")] += 1
            C["deferred_calls"] += bool(n.get("is_deferred"))
            C["goroutine_calls"] += bool(n.get("is_goroutine"))
        C["satellite_calls"] += len(sats)
        nd = sum(1 for n in body.values() if n.get("is_deferred"))
        C["max_deferred_in_callable"] = max(C["max_deferred_in_callable"], nd)
        # Approximate bytes of each section, re-serialized compactly as cango writes it.
        for sec in ("body", "cfg", "cdg", "ddg"):
            C["bytes_" + sec] += len(json.dumps(c.get(sec), separators=(",", ":"), ensure_ascii=False).encode())
        C["bytes_body_node_ids"] += sum(len(n.get("id", "")) + 7 for n in body.values())  # "id":"…",
        # Call nodes exist (with their id) from L1; the other kinds are added by L3.
        C["bytes_body_node_ids_l3_added"] += sum(len(n.get("id", "")) + 7 for n in body.values()
                                                  if n.get("kind") != "call")
        if (sum(1 for n in body.values() if n.get("kind") == "entry") != 1
                or sum(1 for n in body.values() if n.get("kind") == "exit") != 1
                or "@entry" not in body or "@exit" not in body):
            chk["entry_exit_not_exactly_one"] += 1

        keys = set(body)
        succ, pred = defaultdict(list), defaultdict(list)
        seen = set()
        for e in cfg:
            s, t, kd = e["src"], e["dst"], e.get("kind")
            K["cfg_kinds"][kd] += 1
            chk["cfg_duplicate_edge"] += (s, t, kd) in seen
            seen.add((s, t, kd))
            chk["cfg_endpoint_not_body_key"] += s not in keys or t not in keys
            chk["cfg_endpoint_is_satellite"] += s in sats or t in sats
            succ[s].append((t, kd))
            pred[t].append(s)
            sk = body.get(s, {}).get("kind")
            tdef = bool(body.get(t, {}).get("is_deferred"))
            if kd == "exception":
                K["exception_src_kind"][sk] += 1
                C["exception_to_exit"] += t == "@exit"
                if tdef:
                    C["exception_chain_internal" if body.get(s, {}).get("is_deferred")
                      else "exception_into_defer_chain"] += 1
            elif kd == "return":
                K["return_edge_src_kind"][sk] += 1
                C["return_into_defer_chain"] += tdef
        C["callables_with_defer_chain"] += any(e.get("kind") == "defer_resume" for e in cfg)

        for k, n in body.items():
            kd = n.get("kind")
            outs = succ.get(k, [])
            if kd == "return":
                chk["return_node_without_one_return_edge"] += sum(1 for x in outs if x[1] == "return") != 1
            if kd in ("branch", "loop"):
                K[kd + "_out_pattern"][",".join(sorted({x[1] for x in outs if x[1] != "exception"}))] += 1
            if kd == "branch":
                b = n["span"]["bytes"]
                C["if_err_nil_branches"] += bool(IF_ERR.match(src(sfile)[b[0]:b[0] + 400]))
            if kd == "switch":
                b = n["span"]["bytes"]
                head = src(sfile)[b[0]:b[1]]
                word = head[:6].decode("ascii", "replace")
                if word == "switch" and b".(type)" in head.split(b"{", 1)[0]:
                    word = "type-switch"
                fanout[word].append(sum(1 for x in outs if x[1] == "switch_case"))
            if kd not in ("entry", "exit") and k not in sats:
                C["cfg_nodes"] += 1
                C["cfg_nodes_with_exception_edge"] += any(x[1] == "exception" for x in outs)

        # Reachability: every non-satellite node from @entry, and @exit from every node.
        def closure_of(start, adj):
            seen_, q = {start}, deque([start])
            while q:
                for v in adj(q.popleft()):
                    if v not in seen_:
                        seen_.add(v)
                        q.append(v)
            return seen_
        nodes = keys - sats
        fw = closure_of("@entry", lambda u: (v for v, _ in succ.get(u, [])))
        bw = closure_of("@exit", lambda u: pred.get(u, []))
        C["reach_nodes_checked"] += len(nodes)
        chk["node_unreachable_from_entry"] += len(nodes - fw)
        chk["node_not_reaching_exit"] += len(nodes - bw)

        for e in cdg:
            s, t = e["src"], e["dst"]
            chk["cdg_endpoint_not_body_key"] += s not in keys or t not in keys
            chk["cdg_endpoint_is_satellite"] += s in sats or t in sats
            chk["cdg_into_exit"] += t == "@exit"
            chk["cdg_self_edge"] += s == t
            C["cdg_from_entry"] += s == "@entry"
            kinds = {x[1] for x in succ.get(s, [])}
            if s != "@entry" and not kinds & {"true", "false", "switch_case"}:
                C["cdg_from_exception_only_controller"] += 1
        C["cdg"] += len(cdg)

        fc = file_class(sfile)
        for e in ddg:
            s, t, v = e["src"], e["dst"], e.get("var", "")
            chk["ddg_endpoint_not_body_key"] += s not in keys or t not in keys
            chk["ddg_endpoint_is_satellite"] += s in sats or t in sats
            chk["ddg_prov_not_ssa"] += e.get("prov") != ["ssa"]
            K["ddg_var_class"][var_class(v)] += 1
            # A bare base (`c`, not `c.f`) matches every reaching def under that base.
            C["ddg_var_bare_base"] += not v.startswith("<") and "." not in v and "[" not in v
            # Loop-carried (approximate): the def's position is at or after the use's.
            if not s.startswith("@") and not t.startswith("@"):
                ps = tuple(map(int, s.split("/")[0].split(":")))
                pt = tuple(map(int, t.split("/")[0].split(":")))
                C["ddg_def_at_or_after_use"] += ps >= pt
            K["ddg_src_kind"][body.get(s, {}).get("kind")] += 1
        K["ddg_by_file_class"][fc] += len(ddg)
        K["body_by_file_class"][fc] += len(body)
        K["callables_by_file_class"][fc] += 1
        # Group by the callable's own name (last signature segment, parameters dropped), e.g.
        # MarshalToSizedBuffer / Unmarshal / Size for generated protobuf methods.
        nm = (c.get("signature") or "").split("(", 1)[0].rsplit(".", 1)[-1]
        K["ddg_by_name"][nm] += len(ddg)
        K["callables_by_name"][nm] += 1
        C["ddg"] += len(ddg)
        ddg_per.append(len(ddg))
        if len(ddg) > 1000:
            C["callables_ddg_gt_1000"] += 1
            C["ddg_in_callables_gt_1000"] += len(ddg)
        top.append((len(ddg), c.get("id"), c))
        if len(top) > 40:
            top.sort(key=lambda x: -x[0])
            del top[5:]

    l4 = analyze_l4(doc, st) if doc.get("max_level", 0) >= 4 else None
    ddg_per.sort()
    top.sort(key=lambda x: -x[0])
    top_detail = []
    for n, _, c in top[:5]:
        l3ddg = [e for e in c["ddg"] if e.get("prov") == ["ssa"]]
        vc = Counter(e["var"] for e in l3ddg)
        defs, uses = defaultdict(set), defaultdict(set)
        for e in l3ddg:
            defs[e["var"]].add(e["src"])
            uses[e["var"]].add(e["dst"])
        top_detail.append({
            "signature": c.get("signature"), "ddg": n, "cfg": len(c["cfg"]),
            "body": sum(1 for b in c["body"].values() if b.get("kind") not in VERTEX_KINDS),
            "cyclomatic": (c.get("metrics") or {}).get("cyclomatic"),
            "node_kinds": dict(Counter(b.get("kind") for b in c["body"].values() if b.get("kind") not in VERTEX_KINDS)),
            "distinct_vars": len(vc),
            "top_vars": [[v, k, len(defs[v]), len(uses[v])] for v, k in vc.most_common(4)]})
    k1 = max(1, len(ddg_per) // 100)
    return {
        "app": label, "bytes": os.path.getsize(path), "modules": len(st),
        "types": sum(K["types_by_kind"].values()),
        "callables": sum(K["callables_by_kind"].values()),
        "body_nodes": sum(K["node_kinds"].values()),
        "cfg": sum(K["cfg_kinds"].values()),
        **dict(C), **{k: dict(v) for k, v in K.items()
                      if k not in ("ddg_by_name", "callables_by_name")},
        "ddg_by_name_top": [[nm, n, K["callables_by_name"][nm]]
                            for nm, n in K["ddg_by_name"].most_common(10)],
        "switch_fanout": {w: {"n": len(v), "mean": round(sum(v) / len(v), 2), "max": max(v)}
                          for w, v in fanout.items()},
        "ddg_per_callable": {"median": pct(ddg_per, .5), "p90": pct(ddg_per, .9),
                             "p99": pct(ddg_per, .99), "max": ddg_per[-1] if ddg_per else 0,
                             "zero": sum(1 for x in ddg_per if x == 0),
                             "top1pct_share": round(sum(ddg_per[-k1:]) / max(1, C["ddg"]), 4)},
        "ddg_top": top_detail,
        "check_failures": {k: v for k, v in chk.items() if v},
        **({"l4": l4} if l4 is not None else {}),
    }



def of_class(of):
    for p in ("<global>:", "<capture>:", "<return>"):
        if of.startswith(p):
            return p.rstrip(":")
    return "_recv" if of == "_recv" else "param"


def analyze_l4(doc, st):
    """The L4 overlay of an -a 4 analysis.json.

    Dispatch per call site comes from param_in: a site's targets are the owners of the formals
    its actual_ins bind (`<owner-id>@formal_in:<i>`); a target other than the call node's static
    `callee` was added by VTA (an interface call's concrete methods, a func-value call's
    functions). A site whose callee is not null but binds nothing has no vertices (a pruned call
    node or an external with no formals)."""
    a = doc["application"]
    C, K, chk = Counter(), defaultdict(Counter), Counter()
    ids, calls = set(), {}
    for c, closure, mpath, sfile in callables(st):
        body = c.get("body") or {}
        sites = defaultdict(lambda: [0, 0])  # call key -> [#actual_in, #actual_out]
        for k, n in body.items():
            ids.add(n.get("id"))
            kd = n.get("kind")
            if kd in VERTEX_KINDS:
                K["vertices_by_kind"][kd] += 1
                K["vertex_of_class"][kd + " " + of_class(n.get("of", ""))] += 1
                if kd.startswith("actual"):
                    sites[n.get("parent")][kd == "actual_out"] += 1
            elif kd == "call":
                calls[n["id"]] = n.get("callee")
        per_site = Counter()
        for e in c.get("summary") or []:
            for x in (e["src"], e["dst"]):
                chk["summary_endpoint_not_vertex"] += body.get(x, {}).get("kind") not in VERTEX_KINDS
            if (body.get(e["src"], {}).get("of", "").startswith("<global>:")
                    and body.get(e["dst"], {}).get("of", "").startswith("<global>:")):
                chk["summary_global_to_global"] += 1
            per_site[body.get(e["src"], {}).get("parent")] += 1
            C["summary"] += 1
        for site, (ni, no) in sites.items():
            if per_site[site]:
                C["sites_with_summary"] += 1
                C["sites_with_summary_all_to_all"] += per_site[site] == ni * no
        for e in c.get("ddg") or []:
            prov = e.get("prov") or []
            if prov == ["reaching-defs"]:
                C["port_ddg"] += 1
            elif prov == ["points-to"]:
                C["points_to_ddg"] += 1
    C["vertices"] = sum(K["vertices_by_kind"].values())
    C["call_nodes"] = len(calls)
    C["call_nodes_with_callee"] = sum(1 for v in calls.values() if v)
    ext = a.get("external_symbols") or {}
    C["external_symbols"] = len(ext)
    for e in ext.values():
        b = e.get("body") or {}
        C["stand_ins"] += bool(b)
        C["stand_in_formals"] += len(b)
        for n in b.values():
            ids.add(n.get("id"))
    for e in a.get("call_graph") or []:
        prov = e.get("prov") or []
        C["call_graph_vta"] += "vta" in prov
        C["call_graph_vta_only"] += prov == ["vta"]
    targets = defaultdict(set)
    for lst in ("param_in", "param_out"):
        es = a.get(lst) or []
        C[lst] = len(es)
        for e in es:
            chk[lst + "_endpoint_dangling"] += e["src"] not in ids or e["dst"] not in ids
            if lst == "param_in" and "/actual_in:" in e["src"]:
                targets[e["src"].rsplit("/actual_in:", 1)[0]].add(e["dst"].rsplit("@formal_in:", 1)[0])
    for site, ts in targets.items():
        if site not in calls:
            C["capture_sites"] += 1  # a non-call capture site (a literal's position)
            continue
        C["call_sites_bound"] += 1
        extra = ts - {calls[site]}
        C["call_sites_with_vta_target"] += bool(extra)
        C["call_sites_multi_target"] += len(ts) > 1
        K["targets_per_bound_site"][min(len(ts), 10)] += 1
    deg = doc.get("degraded") or []
    return {
        **dict(C), **{k: dict(v) for k, v in K.items()},
        "pct_call_nodes_with_vta_target": round(100 * C["call_sites_with_vta_target"] / max(1, C["call_nodes"]), 2),
        "pct_bound_sites_with_vta_target": round(100 * C["call_sites_with_vta_target"] / max(1, C["call_sites_bound"]), 2),
        "degraded": {d["code"]: {"packages": len(d.get("packages") or []),
                                 "callables": len(d.get("callables") or []),
                                 "detail": d.get("detail", "")} for d in deg},
        "check_failures": {k: v for k, v in chk.items() if v},
    }


if __name__ == "__main__":
    for arg in sys.argv[1:]:
        label, path = arg.split("=", 1)
        print(json.dumps(analyze(label, path)), flush=True)
