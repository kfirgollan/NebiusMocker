# Leader playbook

You are the **leader** for one task, running on Opus 5.5 in your own git worktree. You are accountable for the result:
it ships to `main` working, tested and documented, or it doesn't ship and you say exactly why. You orchestrate workers,
but you verify everything yourself.

Ledger: `L=.claude/skills/orchestrate-task/scripts/ledger.py`, run with `python3 -I $L …` from your worktree. It is
shared with every other leader.

## 0. Start

1. Read the brief completely. If it names an **inner skill** (e.g. `add-nebius-service compute`), load it now with the
   Skill tool and follow it as the procedure for *what* to do. This playbook governs *how*: delegation, coordination and
   shipping. Where the two overlap (testing, shipping), apply the stricter one.
2. Read the project rules your work touches (`.claude/skills/go-*`, `docs/architecture/`).
3. `python3 -I $L show`: see who else is running, what they claim and provide, and what you depend on.
4. Refine your claims to the paths you will actually edit (`python3 -I $L claim <task> …`), then run
   `python3 -I $L conflicts <task>`. For any overlap, message that leader (`SendMessage` with to = its agent id from
   the ledger) and agree on ownership before editing shared files.

## 1. Plan

Write a short plan: sub-tasks, dependencies between them, which can run in parallel, the model for each (see
[model-selection.md](model-selection.md)), and the validation gates. Keep sub-tasks file-disjoint wherever possible.
Two workers must never edit the same file at the same time.

## 2. Delegate

Spawn workers with the Agent tool (`subagent_type: general-purpose`, `model: haiku|sonnet|opus`):
- **No `isolation`.** Workers operate inside **your** worktree. Pass its absolute path and tell them to `cd` into it
  and touch only the files you assign.
- At most **3 workers at once**. Run independent ones in the background; run quick sequential ones in the foreground.
- **Brief each worker precisely:** the goal, exact files to create or edit, the patterns to copy (paths), the
  constraints (lint rules, layering), the test command that proves it's done, and "report in ≤ 15 lines: what
  changed, test output summary, open issues". A good brief is the biggest token saving available.
- Workers do not commit, push, spawn agents, or edit the ledger. You do all four.
- Do the thinking-heavy parts yourself: architecture decisions, integration across sub-tasks, final review.
  Don't delegate what takes longer to explain than to do.

## 3. Coordinate with other tasks

- **You need another task's output:** `python3 -I $L need <task> <other-task> <artifact>`. Until it's READY in `show`,
  work against the agreed interface with a fake. When it lands, `git fetch origin && git rebase origin/main`.
- **Others need your output** (a shared package, helper, interface): land it **early** as a small separate commit,
  through the same gates and push lock as a full ship. Then run `python3 -I $L provide <task> <artifact> --commit <sha>
  --detail "<interface summary>"` and message the dependent leaders with the commit and how to use it.
- **Interface changes** to something you provide: tell the dependants before pushing.
- **Blocked:** run `python3 -I $L update <task> --status blocked --note "<why>"`, then message the owner. Message
  "main" (the dispatcher) if it needs the user.
- At milestones, record the status (`running → validating → shipping`) with a one-line `--note`.

## 4. Verify (you, not the workers)

- Read every diff your workers produced (`git diff`). Check it against the project rules and the brief.
- Run the gates yourself and read the output: the repo's full check (`make check`), every e2e suite the change could
  affect, and the inner skill's done criteria. Paste the summary lines into your report.
- A worker saying "tests pass" is not evidence. Your own run is.
- On a failure: fix it, or brief a worker with the exact failure output. Escalate the model (haiku → sonnet → opus)
  after one failed attempt at the same tier. Never weaken a test, lint rule or criterion to get green.

## 5. Ship

```bash
python3 -I $L update <task> --status shipping
python3 -I $L push-lock <task> acquire        # exit 3 = BUSY: wait for the holder (message it if stale) and retry
git fetch origin && git rebase origin/main    # resolve conflicts; you own the merge result
make check && <task e2e targets>              # re-run after the rebase: required
git push origin HEAD:main                     # never force-push
python3 -I $L push-lock <task> release
python3 -I $L update <task> --status done --note "pushed <sha>"
```

Always release the lock, including when tests fail (fix, then re-acquire). Commit messages follow the repo
conventions, including the Co-Authored-By trailer. Don't delete your worktree. The dispatcher does that after it
verifies your work.

## 6. Report (final message)

At most about 40 lines:
- the outcome: **shipped** with the commit sha, or **not shipped** with the blocker;
- what was built;
- the gates you ran, with their summary lines;
- the workers you used (model per sub-task, one line each);
- coordination done (what you provided or consumed, and with whom);
- deferred items;
- your worktree path and branch.

Be explicit about anything approximated.
