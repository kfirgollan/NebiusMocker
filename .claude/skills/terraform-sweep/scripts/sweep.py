#!/usr/bin/env python3
"""Terraform sweep: compare NebiusMocker with the newest nebius/nebius provider.

For every resource, data source and ephemeral resource the provider documents,
find the gRPC calls it makes (from the provider's generated code), check
whether the mock registers that exact service version and implements each
call, and whether a test under test/ uses the object in HCL. Prints a Markdown
report (or JSON), and with --write updates the status cells, summary table and
version line of docs/terraform.md.

Usage (from the repo root):
  python3 -I .claude/skills/terraform-sweep/scripts/sweep.py [--provider-ref REF] [--json] [--write]

Standard library only. Network: git clone/fetch of the public nebius/api and
terraform-provider-nebius repositories into the cache
(default ~/.cache/nebius-mocker/upstream).
"""
import argparse
import datetime
import glob
import json
import os
import re
import subprocess
import sys

REPOS = {
    "api": "https://github.com/nebius/api.git",
    "provider": "https://github.com/nebius/terraform-provider-nebius.git",
}
KINDS = ("resources", "data-sources", "ephemeral-resources")
HCL_KEYWORD = {"resources": "resource", "data-sources": "data", "ephemeral-resources": "ephemeral"}
SECTION = {"resources": "Resources", "data-sources": "Data sources", "ephemeral-resources": "Ephemeral resources"}
LABEL = {
    "ok": "✅",
    "untested": "⚠️ No Terraform test",
    "missing": "❌ Missing",
    "partial": "❌ Partial",
    "local": "➖ Provider-local",
}
# Struct methods the provider calls per kind (generated code, provider/service).
METHODS = {
    "resources": {"Read", "GetByName", "Create", "Update", "Delete"},
    "data-sources": {"Read", "GetByName", "GetAdditionalGetters"},
    "ephemeral-resources": {"Read"},
}
SCHEMA_METHOD = {
    "ResourceSchema": "resources",
    "DataSourceSchema": "data-sources",
    "EphemeralResourceSchema": "ephemeral-resources",
}


def run(args, cwd=None):
    return subprocess.run(args, cwd=cwd, check=True, capture_output=True, text=True).stdout.strip()


def sync(cache, name, ref=None):
    path = os.path.join(cache, name)
    if not os.path.isdir(os.path.join(path, ".git")):
        run(["git", "clone", "--quiet", "--filter=blob:none", REPOS[name], path])
    else:
        run(["git", "fetch", "--quiet", "--tags", "origin"], cwd=path)
    target = ref or "origin/HEAD"
    if name == "provider" and not ref:
        tags = [t for t in run(["git", "tag", "--list", "v*", "--sort=-v:refname"], cwd=path).splitlines() if t]
        target = tags[0] if tags else target
    run(["git", "checkout", "--quiet", "--detach", target], cwd=path)
    return path, target, run(["git", "rev-parse", "--short", "HEAD"], cwd=path)


def go_imports(src):
    """Map import alias -> path for a Go file (explicit aliases and last path element)."""
    out = {}
    block = re.search(r"^import\s*\((.*?)^\)", src, re.S | re.M)
    lines = block.group(1).splitlines() if block else re.findall(r"^import\s+(.*)$", src, re.M)
    for line in lines:
        m = re.match(r'\s*(\w+)?\s*"([^"]+)"', line)
        if m:
            out[m.group(1) or m.group(2).rsplit("/", 1)[-1]] = m.group(2)
    return out


def go_funcs(src):
    """Yield (receiver type, method name, body) for every method in a Go file."""
    heads = list(re.finditer(r"^func \(\w+ \*?(\w+)\) (\w+)\(", src, re.M))
    for i, h in enumerate(heads):
        end = heads[i + 1].start() if i + 1 < len(heads) else len(src)
        yield h.group(1), h.group(2), src[h.end():end]


def provider_objects(provider_root):
    """{(kind, tf_type)} documented by the provider."""
    objs = set()
    for kind in KINDS:
        for f in glob.glob(os.path.join(provider_root, "docs", kind, "*.md")):
            objs.add((kind, "nebius_" + os.path.basename(f)[:-3]))
    return objs


def provider_calls(provider_root):
    """{(kind, tf_type): {(full service, rpc)}} from the generated code."""
    calls = {}
    for f in glob.glob(os.path.join(provider_root, "generated", "**", "*.go"), recursive=True):
        src = open(f).read()
        imports = go_imports(src)
        structs = {}
        for recv, name, body in go_funcs(src):
            s = structs.setdefault(recv, {"name": None, "kinds": set(), "methods": {}})
            s["methods"][name] = body
            if name == "GetName":
                m = re.search(r'return "(\w+)"', body)
                s["name"] = m and "nebius_" + m.group(1)
            if name in SCHEMA_METHOD:
                s["kinds"].add(SCHEMA_METHOD[name])
        for s in structs.values():
            if not s["name"]:
                continue
            for kind in s["kinds"]:
                found = set()
                for method, body in s["methods"].items():
                    if method not in METHODS[kind]:
                        continue
                    svc = None
                    for alias, svcname in re.findall(r"\b(\w+)\.New(\w+Service)\(", body):
                        path = imports.get(alias, "")
                        if "/gosdk/services/" in path:
                            svc = path.split("/gosdk/services/", 1)[1].replace("/", ".") + "." + svcname
                    for rpc in re.findall(r"\bservice\.(\w+)\(ctx", body):
                        if svc:
                            found.add((svc, rpc))
                calls.setdefault((kind, s["name"]), set()).update(found)
    return calls


def proto_services(api_root):
    """{full service: {rpc: (request full type, response type)}}."""
    out = {}
    for f in glob.glob(os.path.join(api_root, "nebius", "**", "*_service.proto"), recursive=True):
        src = re.sub(r"//[^\n]*", "", open(f).read())
        pkg = re.search(r"^package\s+([\w.]+);", src, re.M).group(1)
        for sm in re.finditer(r"service\s+(\w+)\s*\{", src):
            i, depth = sm.end(), 1
            while depth:
                depth += (src[i] == "{") - (src[i] == "}")
                i += 1
            rpcs = {}
            for name, req, resp in re.findall(
                    r"rpc\s+(\w+)\s*\(\s*(?:stream\s+)?([\w.]+)\s*\)\s*returns\s*\(\s*(?:stream\s+)?([\w.]+)\s*\)",
                    src[sm.end():i - 1]):
                full_req = req if req.startswith("nebius.") else (
                    "nebius." + req if req.count(".") >= 2 else pkg + "." + req)
                rpcs[name] = (full_req, resp)
            out[f"{pkg}.{sm.group(1)}"] = rpcs
    return out


def mock_surface(repo):
    """(registered services, implemented (proto package, rpc, request type)) from services/ and pkg/."""
    registered, handlers = set(), set()
    files = glob.glob(os.path.join(repo, "services", "**", "*.go"), recursive=True)
    files += glob.glob(os.path.join(repo, "pkg", "**", "*.go"), recursive=True)
    for f in files:
        if f.endswith("_test.go"):
            continue
        src = open(f).read()
        imports = go_imports(src)
        protos = {a: p.split("/gosdk/proto/", 1)[1].replace("/", ".")
                  for a, p in imports.items() if "/gosdk/proto/nebius/" in p}
        for alias, svc in re.findall(r"\b(\w+)\.Register(\w+)Server\(", src):
            if alias in protos:
                registered.add(protos[alias] + "." + svc)
        for rpc, alias, req in re.findall(
                r"^func \(\w+ \*?\w+\) (\w+)\(\w+ context\.Context, \w+ \*(\w+)\.(\w+)\)", src, re.M):
            if alias in protos:
                handlers.add((protos[alias], rpc, protos[alias] + "." + req))
    return registered, handlers


def rpc_implemented(svc, rpc, protos, registered, handlers):
    if svc not in registered:
        return False
    req = protos.get(svc, {}).get(rpc, (None, None))[0]
    pkg = svc.rsplit(".", 1)[0]
    if req is None:
        return False
    return (pkg, rpc, req) in handlers or any(h[1] == rpc and h[2] == req for h in handlers)


def test_usage(repo, objects):
    """{(kind, tf_type): [test files]} from HCL in Go sources under test/."""
    names = {}
    for kind, name in objects:
        names.setdefault(HCL_KEYWORD[kind], set()).add((kind, name))
    found = {}
    for f in glob.glob(os.path.join(repo, "test", "**", "*.go"), recursive=True):
        src = open(f).read()
        rel = os.path.relpath(f, repo)
        for kw, name in re.findall(r'\b(resource|data|ephemeral)\s+\\?"(nebius_\w+)\\?"', src):
            for obj in names.get(kw, ()):
                if obj[1] == name:
                    found.setdefault(obj, set()).add(rel)
        # Templated HCL, e.g. data "nebius_vpc_v1_%[1]s" with the suffixes listed in the same file.
        quoted = set(re.findall(r'"(\w+)"', src))
        for kw, prefix in re.findall(r'\b(resource|data|ephemeral)\s+\\?"(nebius_\w*)%(?:\[\d+\])?s', src):
            for obj in names.get(kw, ()):
                if obj[1].startswith(prefix) and obj[1][len(prefix):] in quoted:
                    found.setdefault(obj, set()).add(rel)
    return {k: sorted(v) for k, v in found.items()}


def coverage_doc(repo):
    """{(kind, tf_type): (service, [rpcs])} from docs/services/terraform-coverage.md (hand-written objects)."""
    path = os.path.join(repo, "docs", "services", "terraform-coverage.md")
    out, kind = {}, None
    if not os.path.exists(path):
        return out
    rev = {v: k for k, v in SECTION.items()}
    for line in open(path):
        m = re.match(r"## (Resources|Data sources|Ephemeral resources)", line)
        if m:
            kind = rev[m.group(1)]
            continue
        m = re.match(r"\| `(nebius_\w+)` \| `([^`]+)` \| ([^|]+) \|", line)
        if m and kind:
            rpcs = [r.strip().split()[0] for r in re.split(r"[,+]", m.group(3).split("(")[0])
                    if r.strip() and "OperationService" not in r]
            out[(kind, m.group(1))] = (m.group(2), rpcs)
    return out


def sweep(repo, cache, provider_ref):
    api_root, api_ref, api_sha = sync(cache, "api")
    prov_root, prov_ref, prov_sha = sync(cache, "provider", provider_ref)
    objects = provider_objects(prov_root)
    calls = provider_calls(prov_root)
    protos = proto_services(api_root)
    registered, handlers = mock_surface(repo)
    tests = test_usage(repo, objects)
    fallback = coverage_doc(repo)
    # gosdk Go names can differ in case from the proto names (K8SReleaseService vs K8sReleaseService).
    canon = {k.lower(): k for k in protos}
    calls = {obj: {(canon.get(svc.lower(), svc), rpc) for svc, rpc in found} for obj, found in calls.items()}
    pinned = re.search(r'ProviderVersion\s*=\s*"([^"]+)"',
                       open(os.path.join(repo, "test", "internal", "tfharness", "tfharness.go")).read())
    rows = []
    for kind, name in sorted(objects):
        rpcs, source = sorted(calls.get((kind, name), set())), "generated"
        if not rpcs:
            svc, names = fallback.get((kind, name), ("", []))
            source = "terraform-coverage.md" if svc else "unknown"
            rpcs = [] if svc.startswith("—") or not svc else [(svc, r) for r in names]
            if svc.startswith("—"):
                source = "provider-local"
        # Mutations on v1alpha1 services return common.v1alpha1.Operation, polled through its own service.
        needs = set()
        for svc, rpc in rpcs:
            resp = protos.get(svc, {}).get(rpc, (None, ""))[1]
            if resp.endswith("v1alpha1.Operation"):
                needs.add(("nebius.common.v1alpha1.OperationService", "Get"))
            elif resp.endswith("common.v1.Operation") or resp == "Operation":
                needs.add(("nebius.common.v1.OperationService", "Get"))
        missing = [f"{s}/{r}" for s, r in sorted(set(rpcs) | needs)
                   if not rpc_implemented(s, r, protos, registered, handlers)]
        own_missing = [m for m in missing if m.rsplit("/", 1)[0] not in
                       ("nebius.common.v1.OperationService", "nebius.common.v1alpha1.OperationService")]
        tested = tests.get((kind, name), [])
        if source == "provider-local":
            status = "local"
        elif not rpcs:
            status = "missing"
        elif missing:
            status = "missing" if len(own_missing) == len(set(rpcs)) else "partial"
        else:
            status = "ok" if tested else "untested"
        rows.append({"kind": kind, "type": name, "status": status, "rpcs": [f"{s}/{r}" for s, r in rpcs],
                     "rpc_source": source, "missing_rpcs": missing, "tests": tested})
    drift = []
    for row in rows:
        doc = fallback.get((row["kind"], row["type"]))
        if row["rpc_source"] != "generated":
            continue
        if not doc:
            drift.append(f"{row['type']} ({row['kind']}): not in terraform-coverage.md")
            continue
        doc_rpcs = {r for r in doc[1]}
        gen_rpcs = {r.rsplit("/", 1)[1] for r in row["rpcs"]}
        gen_svc = {r.rsplit("/", 1)[0] for r in row["rpcs"]}
        if not gen_rpcs <= doc_rpcs or gen_svc != {doc[0]}:
            drift.append(f"{row['type']} ({row['kind']}): provider calls {sorted(gen_svc)} {sorted(gen_rpcs)}, "
                         f"doc says {doc[0]} {sorted(doc_rpcs)}")
    known = {(r["kind"], r["type"]) for r in rows}
    drift += [f"{t} ({k}): in terraform-coverage.md but no longer in the provider"
              for k, t in sorted(set(fallback) - known)]
    return {
        "upstream": {"provider": f"{prov_ref}@{prov_sha}", "api": f"{api_ref}@{api_sha}",
                     "provider_pinned_in_tests": pinned.group(1) if pinned else None},
        "objects": rows,
        "coverage_doc_drift": drift,
    }


def summary(rows):
    out = {}
    for kind in KINDS:
        c = {s: 0 for s in LABEL}
        for r in rows:
            if r["kind"] == kind:
                c[r["status"]] += 1
        c["total"] = sum(c.values())
        out[kind] = c
    return out


def markdown(result):
    rows, up = result["objects"], result["upstream"]
    lines = [f"# Terraform sweep", "",
             f"Provider {up['provider']} (tests pin {up['provider_pinned_in_tests']}), nebius/api {up['api']}.", "",
             "| Kind | Total | ✅ tested | ⚠️ no TF test | ➖ local | ❌ partial | ❌ missing |",
             "|---|---|---|---|---|---|---|"]
    for kind, c in summary(rows).items():
        lines.append(f"| {SECTION[kind]} | {c['total']} | {c['ok']} | {c['untested']} | {c['local']} | "
                     f"{c['partial']} | {c['missing']} |")
    lines += ["", "## Objects", "", "| Kind | Type | Status | Missing RPCs | Tests |", "|---|---|---|---|---|"]
    for r in rows:
        tests = ", ".join(r["tests"][:3]) + (" …" if len(r["tests"]) > 3 else "")
        lines.append(f"| {r['kind']} | `{r['type']}` | {LABEL[r['status']]} | {', '.join(r['missing_rpcs'])} | "
                     f"{tests} |")
    lines += ["", "## terraform-coverage.md drift", ""]
    lines += [f"- {d}" for d in result["coverage_doc_drift"]] or ["- none"]
    return "\n".join(lines)


def write_doc(repo, result):
    """Update docs/terraform.md in place: status cells, new/removed rows, summary table, version line."""
    path = os.path.join(repo, "docs", "terraform.md")
    text = open(path).read()
    rows, changes = result["objects"], []
    by_kind = {k: {r["type"]: r for r in rows if r["kind"] == k} for k in KINDS}
    for kind in KINDS:
        head = f"\n## {SECTION[kind]}\n"
        start = text.find(head)
        if start < 0:
            changes.append(f"section '## {SECTION[kind]}' not found; left unchanged")
            continue
        end = text.find("\n## ", start + len(head))
        end = len(text) if end < 0 else end
        body = text[start:end].split("\n")
        table = [i for i, l in enumerate(body) if re.match(r"\| `nebius_\w+` \|", l)]
        if not table:
            continue
        existing, first, last = {}, table[0], table[-1]
        for i in table:
            cells = body[i].split("|")
            existing[cells[1].strip().strip("`")] = cells
        new_rows = []
        for name in sorted(set(existing) | set(by_kind[kind])):
            if name not in by_kind[kind]:
                changes.append(f"removed {name} ({kind}): no longer in the provider")
                continue
            label = LABEL[by_kind[kind][name]["status"]]
            cells = existing.get(name)
            if cells is None:
                changes.append(f"added {name} ({kind}) as {label}")
                cells = ["", f" `{name}` ", "", " ", ""]
            elif cells[2].strip() != label:
                changes.append(f"{name} ({kind}): {cells[2].strip()} -> {label}")
            cells[2] = f" {label} "
            new_rows.append("|".join(cells))
        body[first:last + 1] = new_rows
        text = text[:start] + "\n".join(body) + text[end:]
    c = summary(rows)
    tot = {s: sum(c[k][s] for k in KINDS) for s in list(LABEL) + ["total"]}

    def srow(name, d, local_names=(), bold=False):
        cell = (lambda v: f"**{v}**") if bold else str
        local = cell(d["local"]) + (" (" + ", ".join(f"`{n}`" for n in local_names) + ")" if local_names else "")
        return (f"| {name} | {cell(d['total'])} | {cell(d['ok'])} | {cell(d['untested'])} | {local} | "
                f"{cell(d['missing'] + d['partial'])} |")

    locals_by_kind = {k: [r["type"] for r in rows if r["kind"] == k and r["status"] == "local"] for k in KINDS}
    new_summary = [srow(SECTION[k], c[k], locals_by_kind[k]) for k in KINDS] + [srow("**Total**", tot, bold=True)]
    m = re.search(r"(\| Kind \| In the official provider[^\n]*\n\|[-| ]+\|\n)((?:\|[^\n]*\n){4})", text)
    if m:
        text = text[:m.start(2)] + "\n".join(new_summary) + "\n" + text[m.end(2):]
    else:
        changes.append("summary table not found; left unchanged")
    version = result["upstream"]["provider"].split("@")[0].lstrip("v")
    text = re.sub(r"Checked against provider \*\*[\w.]+\*\*", f"Checked against provider **{version}**", text, count=1)
    sha = run(["git", "rev-parse", "--short", "HEAD"], cwd=repo)
    today = datetime.date.today().isoformat()  # date of the sweep, written into the doc
    text = re.sub(r"Status as of [\d-]+\s*\n?\(`main` at `\w+`\)", f"Status as of {today}\n(`main` at `{sha}`)",
                  text, count=1)
    open(path, "w").write(text)
    return changes


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--cache", default=os.path.expanduser("~/.cache/nebius-mocker/upstream"))
    ap.add_argument("--provider-ref", help="provider tag/commit (default: newest v* tag)")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    ap.add_argument("--write", action="store_true", help="update docs/terraform.md in place")
    args = ap.parse_args()
    repo = run(["git", "rev-parse", "--show-toplevel"])
    os.makedirs(args.cache, exist_ok=True)
    result = sweep(repo, args.cache, args.provider_ref)
    if args.write:
        result["doc_changes"] = write_doc(repo, result)
    if args.json:
        json.dump(result, sys.stdout, indent=1)
        print()
        return
    print(markdown(result))
    if args.write:
        print("\n## docs/terraform.md changes\n")
        print("\n".join(f"- {c}" for c in result["doc_changes"]) or "- none")


if __name__ == "__main__":
    main()
