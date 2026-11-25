#!/usr/bin/env python3
"""Shared task ledger for parallel orchestrated tasks.

The ledger lives in git's common directory (shared by the main checkout and
every worktree, never committed): $(git rev-parse --git-common-dir)/orchestration/.
All writes take an exclusive file lock, so parallel leaders cannot corrupt it.

Commands (run from any checkout of the repo):
  ledger.py register <task> --title T [--agent ID] [--worktree P] [--branch B] [--skill S]
  ledger.py update   <task> [--status S] [--note N] [--agent ID] [--worktree P] [--branch B]
  ledger.py claim    <task> <path-glob>...      # files/dirs this task will edit
  ledger.py provide  <task> <name> [--commit SHA] [--detail D]   # artifact others can use
  ledger.py need     <task> <other-task> <name> # declare a dependency on another task's artifact
  ledger.py conflicts <task>                    # claims overlapping other active tasks
  ledger.py show [<task>] [--json]
  ledger.py push-lock <task> acquire|release    # serialize rebase+test+push to main
  ledger.py remove   <task>                     # drop a finished/cancelled task

Statuses: planned, running, blocked, validating, shipping, done, failed, cancelled.
Standard library only.
"""
import argparse
import contextlib
import datetime
import fcntl
import fnmatch
import json
import os
import subprocess
import sys

ACTIVE = {"planned", "running", "blocked", "validating", "shipping"}
STATUSES = ACTIVE | {"done", "failed", "cancelled"}
# A push lock older than this is considered abandoned (its holder crashed).
STALE_LOCK_SECONDS = 45 * 60


def base_dir():
    common = subprocess.run(["git", "rev-parse", "--git-common-dir"], check=True,
                            capture_output=True, text=True).stdout.strip()
    d = os.path.join(os.path.abspath(common), "orchestration")
    os.makedirs(d, exist_ok=True)
    return d


@contextlib.contextmanager
def locked():
    d = base_dir()
    with open(os.path.join(d, "ledger.lock"), "w") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        path = os.path.join(d, "ledger.json")
        data = json.load(open(path)) if os.path.exists(path) else {"tasks": {}, "push_lock": None}
        yield data
        tmp = path + ".tmp"
        with open(tmp, "w") as f:
            json.dump(data, f, indent=1, sort_keys=True)
        os.replace(tmp, path)


def now():
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def age(stamp):
    then = datetime.datetime.strptime(stamp, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc)
    return (datetime.datetime.now(datetime.timezone.utc) - then).total_seconds()


def task(data, name, create=False):
    t = data["tasks"].get(name)
    if t is None:
        if not create:
            sys.exit(f"unknown task {name!r}; register it first")
        t = data["tasks"][name] = {"status": "planned", "claims": [], "provides": {}, "needs": [], "notes": []}
    return t


def overlaps(a, b):
    a, b = a.rstrip("/"), b.rstrip("/")
    return (fnmatch.fnmatch(a, b) or fnmatch.fnmatch(b, a) or
            a.startswith(b.rstrip("*") ) or b.startswith(a.rstrip("*")))


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)
    p = sub.add_parser("register"); p.add_argument("task"); p.add_argument("--title", required=True)
    for opt in ("--agent", "--worktree", "--branch", "--skill"):
        p.add_argument(opt)
    p = sub.add_parser("update"); p.add_argument("task")
    p.add_argument("--status", choices=sorted(STATUSES)); p.add_argument("--note")
    for opt in ("--agent", "--worktree", "--branch"):
        p.add_argument(opt)
    p = sub.add_parser("claim"); p.add_argument("task"); p.add_argument("paths", nargs="+")
    p = sub.add_parser("provide"); p.add_argument("task"); p.add_argument("name")
    p.add_argument("--commit"); p.add_argument("--detail")
    p = sub.add_parser("need"); p.add_argument("task"); p.add_argument("other"); p.add_argument("name")
    p = sub.add_parser("conflicts"); p.add_argument("task")
    p = sub.add_parser("show"); p.add_argument("task", nargs="?"); p.add_argument("--json", action="store_true")
    p = sub.add_parser("push-lock"); p.add_argument("task"); p.add_argument("action", choices=["acquire", "release"])
    p = sub.add_parser("remove"); p.add_argument("task")
    a = ap.parse_args()

    with locked() as data:
        if a.cmd == "register":
            t = task(data, a.task, create=True)
            t.update({k: v for k, v in {"title": a.title, "agent": a.agent, "worktree": a.worktree,
                                         "branch": a.branch, "skill": a.skill}.items() if v})
            t["status"] = "running" if a.agent else t["status"]
            t.setdefault("created", now())
        elif a.cmd == "update":
            t = task(data, a.task)
            for k in ("status", "agent", "worktree", "branch"):
                if getattr(a, k):
                    t[k] = getattr(a, k)
            if a.note:
                t["notes"].append(f"{now()} {a.note}")
        elif a.cmd == "claim":
            t = task(data, a.task)
            t["claims"] = sorted(set(t["claims"]) | set(a.paths))
        elif a.cmd == "provide":
            t = task(data, a.task)
            t["provides"][a.name] = {"commit": a.commit, "detail": a.detail, "at": now()}
        elif a.cmd == "need":
            t = task(data, a.task)
            dep = {"task": a.other, "name": a.name}
            if dep not in t["needs"]:
                t["needs"].append(dep)
        elif a.cmd == "conflicts":
            mine = task(data, a.task)
            found = []
            for name, other in data["tasks"].items():
                if name == a.task or other.get("status") not in ACTIVE:
                    continue
                for c in mine["claims"]:
                    for oc in other["claims"]:
                        if overlaps(c, oc):
                            found.append(f"{c}  ⟷  {name}: {oc}  (agent {other.get('agent', '?')})")
            print("\n".join(found) if found else "no conflicts")
            return
        elif a.cmd == "push-lock":
            holder = data.get("push_lock")
            if a.action == "acquire":
                if holder and holder["task"] != a.task and age(holder["at"]) < STALE_LOCK_SECONDS:
                    print(f"BUSY: held by {holder['task']} since {holder['at']}")
                    sys.exit(3)
                if holder and holder["task"] != a.task:
                    print(f"taking over stale lock from {holder['task']} (held since {holder['at']})")
                data["push_lock"] = {"task": a.task, "at": now()}
                print("acquired")
            else:
                if holder and holder["task"] == a.task:
                    data["push_lock"] = None
                print("released")
            return
        elif a.cmd == "remove":
            data["tasks"].pop(a.task, None)
            if (data.get("push_lock") or {}).get("task") == a.task:
                data["push_lock"] = None
            return
        if a.cmd == "show":
            if a.json:
                print(json.dumps(data if not a.task else data["tasks"].get(a.task), indent=1))
                return
            rows = [(n, t) for n, t in sorted(data["tasks"].items()) if not a.task or n == a.task]
            for n, t in rows:
                print(f"## {n} [{t.get('status')}] {t.get('title', '')}")
                for k in ("agent", "skill", "worktree", "branch"):
                    if t.get(k):
                        print(f"- {k}: {t[k]}")
                if t["claims"]:
                    print(f"- claims: {', '.join(t['claims'])}")
                for pn, pv in t["provides"].items():
                    print(f"- provides {pn}: commit {pv.get('commit') or '(pending)'} {pv.get('detail') or ''}")
                for d in t["needs"]:
                    other = data["tasks"].get(d["task"], {})
                    got = other.get("provides", {}).get(d["name"], {})
                    print(f"- needs {d['name']} from {d['task']}: {'READY @' + got['commit'] if got.get('commit') else 'waiting'}")
                for note in t["notes"][-3:]:
                    print(f"- note: {note}")
            if data.get("push_lock"):
                print(f"\npush lock: {data['push_lock']['task']} since {data['push_lock']['at']}")
            if not rows:
                print("ledger is empty")


if __name__ == "__main__":
    main()
