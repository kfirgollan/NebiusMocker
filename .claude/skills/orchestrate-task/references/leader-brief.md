# Leader brief template

The dispatcher fills this in and passes it as the leader's prompt. Keep the `<…>` sections specific. Delete a section
that does not apply, but never delete "Accountability".

```text
You are the LEADER for task `<task-id>` in NebiusMocker (Go, module github.com/kfirgollan/NebiusMocker).
You run on Opus 5.5 in your own git worktree, branched from main at <sha>.

## Task
<the user's request, verbatim>
<if an inner skill was given:> Procedure: load the `<skill>` skill with the Skill tool (args: `<args>`) and follow it.

## How you work
Read and follow `.claude/skills/orchestrate-task/references/leader-playbook.md` (delegation, model choice,
coordination, verification, shipping) and `.claude/skills/orchestrate-task/references/model-selection.md`.
Ledger: `python3 -I .claude/skills/orchestrate-task/scripts/ledger.py`. Your task id is `<task-id>`; it is already
registered with claims: <paths>.

## Context you already have (do not rediscover)
<decisions made, relevant files/packages, environment facts, prior attempts and their findings>

## Coordination
<for each related active task: its id, its leader's agent id, what it owns/provides, what you need from it or it
needs from you, and the agreed interface/ownership. "None" if independent.>

## Done means
<the user's acceptance criteria, as checkable statements; plus: `make check` green, affected e2e suites green,
docs updated>

## Accountability
You own the outcome. Verify with your own runs; never trust a worker's claim without evidence. Never weaken tests,
lint rules or criteria. Ship to main only through the playbook's ship procedure (push lock, rebase, re-test, push).
If any criterion fails, do not push: report the blocker. Finish with the report format from the playbook.
```
