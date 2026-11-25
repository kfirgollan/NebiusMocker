---
name: orchestrate-task
description: Delegate a task to an Opus 5.5 "leader" sub-agent working in its own git worktree. The leader plans, delegates to cheaper sub-agents (Haiku/Sonnet/Opus by complexity), validates, ships to main, and owns the result. Many tasks can run in parallel. A shared ledger tracks file claims, artifacts and dependencies, so leaders coordinate, notify each other and serialize pushes. Composes with other skills, e.g. `/orchestrate-task /add-nebius-service compute`. Use when asked to orchestrate, delegate, spin up a leader or team, or run tasks in parallel in worktrees.
---

# orchestrate-task

You, the main session, are the **dispatcher**. You don't do the task. You start a **leader** for it, keep parallel
leaders coordinated, and close the task out. The leader is accountable for delivering the task at full quality.

```text
user ──/orchestrate-task <task>──▶ dispatcher (main session)
                                     │  ledger: register, claims, deps
                                     ▼
                              leader (Opus 5.5, own worktree)  ◀──messages──▶ other leaders
                                     │  plan → delegate → verify → ship
                         ┌───────────┼───────────┐
                      haiku        sonnet       opus      (workers, chosen per sub-task)
```

Ledger (shared by every worktree, never committed): `python3 -I .claude/skills/orchestrate-task/scripts/ledger.py …`.
It is stored in `$(git rev-parse --git-common-dir)/orchestration/`. Run `ledger.py -h` for the commands.

## 1. Parse the request

`/orchestrate-task <task text>`, where the text may start with another skill invocation:
- `/orchestrate-task /add-nebius-service compute`: the **inner skill** is `add-nebius-service` with args `compute`. The
  leader must load it with the Skill tool and follow it as the task's procedure.
- `/orchestrate-task fix flaky contract test X`: a free-form task.

Choose a short task id (`svc-compute`, `fix-flaky-x`). If the inner skill names a scope, put it in the id.

## 2. Check the ledger before spawning

```bash
L=.claude/skills/orchestrate-task/scripts/ledger.py
python3 -I $L show
```

- **Duplicate:** an active task already covers this scope. Tell the user, and don't spawn.
- **Predict claims:** from the task and repo layout, list the paths it will likely edit (`services/compute/`,
  `pkg/objstore/`, …). Register the task and claims, then run `conflicts`:

  ```bash
  python3 -I $L register <id> --title "<one line>" --skill "<inner skill or ->"
  python3 -I $L claim <id> <paths…>
  python3 -I $L conflicts <id>
  ```

- **Overlap or dependency** on another active task: decide ownership now. Shared code goes into a package one task
  owns and lands early; the others consume it. Record it with `need`/`provide`, and put the agreement into **both**
  briefs. For leaders already running, send them a message.

## 3. Spawn the leader

Fill in [references/leader-brief.md](references/leader-brief.md) and call the Agent tool with:
`subagent_type: general-purpose`, **`model: opus`** (the leader is always Opus 5.5), `isolation: worktree`,
`run_in_background: true`, and a 3–5 word description.

Then record the agent: `python3 -I $L update <id> --agent <agentId> --status running`.

Send every leader that this one depends on, or that depends on it, a message with the new leader's agentId and the
agreed artifact.

## 4. While tasks run

You are re-invoked when a leader messages "main" or finishes. Don't poll.
- **Relay** information between leaders when one asks, and when a `provide` lands that another task `need`s
  (`show` lists it as READY). Send the dependent leader the commit and tell it to rebase.
- **Unblock**: answer leader questions from the user's intent. Route real decisions (scope, cost, trade-offs) to the
  user, briefly.
- **Never do a leader's work yourself**, and never take over its worktree. If a leader is stuck, send it guidance or
  respawn it, starting from its findings.
- If the user changes requirements, message the leader. Don't spawn a duplicate.

## 5. Close out (every leader report)

A leader's report is a claim. Verify it:

1. `git fetch origin` and confirm the reported commit is on `origin/main` (`git branch -r --contains <sha>`).
2. In the main checkout: `git pull --ff-only`, then run `make check`. Run the task's e2e targets if they are cheap or the
   leader reports anything flaky.
3. **Audit the report and the tests for shortcuts** (CLAUDE.md "No shortcuts"): bypassed user paths, silently ignored
   fields, test-only workarounds, skipped documented workflows. Read the "deferred" list especially closely; a shortcut
   is often filed there.
   - If the leader is still live, send it the exact gaps to fix and reopen the task in the ledger.
   - If it isn't, register and spawn a follow-up task for them.
   - Only scope deferrals may stay deferred.

   If both checks pass and there are no shortcuts, remove the worktree and branch, and mark the task done.

   A finished agent's worktree may still be locked by the harness. Unlock it only after confirming it is clean and fully
   pushed (`git -C <wt> status --porcelain` is empty and `git -C <wt> log origin/main..HEAD` shows nothing):

   ```bash
   git worktree remove <path> && git branch -D <branch>
   python3 -I $L update <id> --status done --note "shipped <sha>"
   ```

4. Tell dependent leaders to rebase onto the new `main`.
5. Report to the user: what shipped, the evidence (commit, test lines), and anything deferred.

If the leader did **not** ship (blocked or failed): keep the worktree, mark the task `failed` or `blocked` with the
reason, and tell the user what is needed. Respawn only with a changed plan or a new limit.

## Rules

- The leader is always `opus`. Workers are chosen by the leader using
  [references/model-selection.md](references/model-selection.md).
- One task, one leader, one worktree. Only leaders push to `main`, and only while holding the ledger push lock.
- Briefs carry context. Workers don't rediscover what is already known: pass paths, decisions and constraints.
- Main must stay green. A leader that cannot pass the gates does not push.
