#!/bin/sh
# Remove a finished leader's worktree and branch, but ONLY if nothing can be lost:
# no uncommitted changes and no commits missing from origin/main.
#
# Usage (from the main checkout): close-worktree.sh <worktree-path>
# Exit codes: 0 removed; 2 refused (dirty or unpushed work, listed); 1 usage/error.
set -eu

wt=${1:?usage: close-worktree.sh <worktree-path>}
[ -d "$wt" ] || { echo "no such worktree: $wt" >&2; exit 1; }

git fetch -q origin
dirty=$(git -C "$wt" status --porcelain)
unpushed=$(git -C "$wt" log --oneline origin/main..HEAD)

if [ -n "$dirty" ] || [ -n "$unpushed" ]; then
  echo "REFUSED: $wt still holds work that is not on origin/main" >&2
  [ -n "$dirty" ] && printf 'uncommitted:\n%s\n' "$dirty" >&2
  [ -n "$unpushed" ] && printf 'unpushed commits:\n%s\n' "$unpushed" >&2
  exit 2
fi

branch=$(git -C "$wt" rev-parse --abbrev-ref HEAD)
git worktree unlock "$wt" 2>/dev/null || true # the harness locks agent worktrees
git worktree remove "$wt"
if [ "$branch" != "HEAD" ]; then
  git branch -D "$branch"
fi
echo "removed $wt${branch:+ (branch $branch)}"
