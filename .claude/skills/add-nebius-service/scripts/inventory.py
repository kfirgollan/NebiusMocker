#!/usr/bin/env python3
"""Gap report for one NebiusMocker service area.

Compares upstream Nebius sources (protos, Terraform provider, Go SDK) with what
the repository implements and tests, and prints a Markdown report.

Usage (from the repo root):
  python3 -I .claude/skills/add-nebius-service/scripts/inventory.py <area> [--cache DIR] [--provider-ref REF]

<area> is a NebiusMocker area name: iam, vpc, compute, registry, storage,
mk8s, msp-postgresql, msp-mlflow, ... (see docs/services/README.md).

Standard library only. Network: git clone/fetch of public GitHub repos into
the cache directory (default: ~/.cache/nebius-mocker/upstream).
"""
import argparse
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


def proto_dir(area):
    return "nebius/" + area.replace("-", "/")


def parse_services(api_root, area):
    """Return {full_service: {"file", "rpcs": [(name, req, resp)], "deprecated"}}."""
    out = {}
    for f in sorted(glob.glob(os.path.join(api_root, proto_dir(area), "**", "*_service.proto"), recursive=True)):
        src = re.sub(r"//[^\n]*", "", open(f).read())
        pkg = re.search(r"^package\s+([\w.]+);", src, re.M).group(1)
        for sm in re.finditer(r"service\s+(\w+)\s*\{", src):
            i, depth = sm.end(), 1
            while depth:
                depth += (src[i] == "{") - (src[i] == "}")
                i += 1
            body = src[sm.end():i - 1]
            rpcs = re.findall(r"rpc\s+(\w+)\s*\(\s*(?:stream\s+)?([\w.]+)\s*\)\s*returns\s*\(\s*(?:stream\s+)?([\w.]+)\s*\)", body)
            out[f"{pkg}.{sm.group(1)}"] = {
                "file": os.path.relpath(f, api_root),
                "rpcs": rpcs,
                "deprecated": "service_deprecation_details" in body,
            }
    return out


def tf_objects(provider_root, area):
    """Return [(kind, tf_type)] for provider objects of the area."""
    prefix = area.replace("-", "_") + "_"
    objs = []
    for kind in ("resources", "data-sources", "ephemeral-resources"):
        for f in sorted(glob.glob(os.path.join(provider_root, "docs", kind, prefix + "*.md"))):
            objs.append((kind, "nebius_" + os.path.basename(f)[:-3]))
    return objs


def tf_calls(provider_root, area):
    """Return {service_file_key: set(methods)} from generated provider code."""
    calls = {}
    gen = os.path.join(provider_root, "generated", proto_dir(area))
    for f in glob.glob(os.path.join(gen, "**", "*_service.tfgen.go"), recursive=True):
        key = os.path.relpath(f, gen)[: -len("_service.tfgen.go")]
        calls[key] = set(re.findall(r"\bservice\.([A-Z]\w+)\(", open(f).read())) - {
            "NewResource", "NewDataSource", "NewDefaultParent", "NewSimpleDataSource", "NewSimpleEphemeralResource"}
    return calls


def norm(s):
    return re.sub(r"[^a-z0-9]", "", s.lower())


def repo_text(repo, pattern):
    text = {}
    for f in glob.glob(os.path.join(repo, pattern), recursive=True):
        text[os.path.relpath(f, repo)] = open(f, errors="replace").read()
    return text


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("area")
    ap.add_argument("--cache", default=os.path.expanduser("~/.cache/nebius-mocker/upstream"))
    ap.add_argument("--provider-ref", help="provider tag/commit (default: newest v* tag)")
    ap.add_argument("--json", action="store_true", help="machine-readable output")
    a = ap.parse_args()
    repo = os.getcwd()
    if not os.path.exists(os.path.join(repo, "go.mod")):
        sys.exit("run from the repository root")
    os.makedirs(a.cache, exist_ok=True)

    api_root, api_ref, api_sha = sync(a.cache, "api")
    prov_root, prov_ref, prov_sha = sync(a.cache, "provider", a.provider_ref)
    services = parse_services(api_root, a.area)
    if not services:
        sys.exit(f"no services found under {proto_dir(a.area)} in nebius/api")
    objects = tf_objects(prov_root, a.area)
    calls = tf_calls(prov_root, a.area)

    area_dir = "services/" + a.area.split("-")[0]
    api_go = repo_text(repo, area_dir + "/**/api/*.go")
    api_go.update(repo_text(repo, area_dir + "/api/*.go"))
    all_go = "\n".join(repo_text(repo, "**/*.go").values())
    tests = repo_text(repo, "test/**/*_test.go")
    tests.update(repo_text(repo, area_dir + "/**/*_test.go"))
    test_blob = "\n".join(tests.values())
    gomod = open(os.path.join(repo, "go.mod")).read()
    gosdk = (re.search(r"github.com/nebius/gosdk\s+(\S+)", gomod) or [None, "?"])[1]
    pinned = sorted(set(re.findall(r'providerVersion\s*=\s*"([^"]+)"', test_blob)))

    report = {"area": a.area, "upstream": {"api": f"{api_ref}@{api_sha}", "provider": f"{prov_ref}@{prov_sha}",
                                            "gosdk_in_go_mod": gosdk, "provider_pinned_in_tests": pinned},
              "services": [], "terraform": []}
    for svc, info in services.items():
        short = svc.rsplit(".", 1)[1]
        registered = f"Register{short}Server" in all_go and svc.split(".")[2] in all_go
        key = None
        for k in calls:
            if norm(k.replace("/", "")) == norm(".".join(svc.split(".")[2:-1]) + short[: -len("Service")]):
                key = k
        tf_used = calls.get(key, set())
        rows = []
        for name, req, _resp in info["rpcs"]:
            req_type = req.rsplit(".", 1)[-1]
            implemented = any(re.search(rf"func \(\w+ \*\w+\) {name}\(ctx context\.Context, req \*\w+\.{req_type}\)", t)
                              for t in api_go.values())
            authz = (f'"/{svc}/{name}"' in all_go or f"/{svc}/" in all_go or
                     re.search(rf'CRUD\(\w*[Ss]ervice\w*', all_go) is not None and svc in all_go)
            tested = req_type in test_blob
            rows.append({"rpc": name, "request": req_type, "implemented": implemented,
                         "authz_rule": bool(authz) and implemented, "tested": tested, "terraform_uses": name in tf_used})
        report["services"].append({"service": svc, "proto": info["file"], "deprecated": info["deprecated"],
                                   "registered": registered, "rpcs": rows})
    hcl_keyword = {"resources": "resource", "data-sources": "data", "ephemeral-resources": "ephemeral"}
    for kind, typ in objects:
        tested = re.search(rf'{hcl_keyword[kind]}\s+\\?"{typ}\\?"', test_blob) is not None
        report["terraform"].append({"kind": kind, "type": typ, "e2e_tested": tested})

    if a.json:
        print(json.dumps(report, indent=1))
        return
    u = report["upstream"]
    print(f"# Gap report: {a.area}\n")
    print(f"- nebius/api: `{u['api']}`\n- terraform-provider-nebius: `{u['provider']}` (tests pin: {', '.join(pinned) or 'none'})")
    print(f"- gosdk in go.mod: `{gosdk}` (compare with the newest tag: `go list -m -versions github.com/nebius/gosdk`)\n")
    print("## RPCs\n\n| Service | RPC | Implemented | Authz rule | Tested | Used by Terraform |\n|---|---|---|---|---|---|")
    mark = lambda b: "✅" if b else "❌"
    todo = 0
    for s in report["services"]:
        dep = " *(deprecated)*" if s["deprecated"] else ""
        for r in s["rpcs"]:
            if not (r["implemented"] and r["tested"]):
                todo += 1
            print(f"| `{s['service'].split('.', 1)[1]}`{dep} | `{r['rpc']}` | {mark(r['implemented'])} | "
                  f"{mark(r['authz_rule'])} | {mark(r['tested'])} | {'✅' if r['terraform_uses'] else ''} |")
    print("\n## Terraform objects\n\n| Kind | Type | E2E tested |\n|---|---|---|")
    for o in report["terraform"]:
        if not o["e2e_tested"]:
            todo += 1
        print(f"| {o['kind']} | `{o['type']}` | {mark(o['e2e_tested'])} |")
    print(f"\n**Open items: {todo}.** Prioritise ❌ rows that Terraform uses, then the remaining RPCs.")
    print("Heuristic detection: confirm each row by reading the code before relying on it.")


if __name__ == "__main__":
    main()
