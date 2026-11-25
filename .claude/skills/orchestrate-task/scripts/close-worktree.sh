#!/bin/sh
# Remove a finished leader's worktree and branch, but ONLY if nothing can be lost:
# no uncommitted changes and no commits missing from origin/main. Before removing
# it, it stops any processes still running in the worktree (left-over background
# shells, servers, wait loops) and lists what it stopped.
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

# Stop processes a leader or its workers left running in the worktree (wait
# loops, mock servers, probes). Background shells outlive their agent, and once
# the worktree is gone their git commands fail forever. Matched by working
# directory or command line; claude processes and this script are never touched.
abs=$(cd "$wt" && pwd -P)
procs=$(
  {
    lsof -a -d cwd -Fpn 2>/dev/null | awk -v d="$abs" '
      /^p/ { pid = substr($0, 2) }
      /^n/ { n = substr($0, 2); if (n == d || index(n, d "/") == 1) print pid }'
    ps -eo pid=,command= | awk -v d="$abs" 'index($0, d) { print $1 }'
  } | sort -un | while read -r pid; do
    [ "$pid" = "$$" ] && continue
    [ "$pid" = "$PPID" ] && continue
    case $(ps -o comm= -p "$pid" 2>/dev/null) in
      ("" | claude | */claude) continue ;;
    esac
    echo "$pid"
  done
)
if [ -n "$procs" ]; then
  echo "stopping processes left in $wt:"
  for pid in $procs; do ps -o pid=,etime=,command= -p "$pid" 2>/dev/null | cut -c1-160; done
  for pid in $procs; do pkill -TERM -P "$pid" 2>/dev/null || true; kill -TERM "$pid" 2>/dev/null || true; done
  sleep 2
  for pid in $procs; do
    if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid" 2>/dev/null || true; fi
  done
fi

branch=$(git -C "$wt" rev-parse --abbrev-ref HEAD)
git worktree unlock "$wt" 2>/dev/null || true # the harness locks agent worktrees
git worktree remove "$wt"
if [ "$branch" != "HEAD" ]; then
  git branch -D "$branch"
fi
echo "removed $wt${branch:+ (branch $branch)}"
