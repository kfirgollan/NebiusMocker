#!/usr/bin/env bash
# Removes the k3s containers, volumes and networks of mk8s k3s backends whose
# mocker is gone (killed before it could clean up). Every object carries
# nebius-mocker.owner=<webhook URL>, whose port is the owning mocker's edge
# port; objects whose owner port no longer accepts connections are removed.
# A running mocker reaps its own leftovers at startup; this is for owners
# that will not come back (e.g. test runs on random ports).
set -euo pipefail

owners=$(docker ps -a --filter label=nebius-mocker.backend=k3s --format '{{.Label "nebius-mocker.owner"}}' | sort -u)
owners+=$'\n'$(docker volume ls --filter label=nebius-mocker.backend=k3s --format '{{.Label "nebius-mocker.owner"}}' | sort -u)
owners+=$'\n'$(docker network ls --filter label=nebius-mocker.backend=k3s --format '{{.Label "nebius-mocker.owner"}}' | sort -u)

for owner in $(printf '%s\n' "$owners" | sort -u); do
	[ -n "$owner" ] || continue
	port=${owner##*:}
	if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
		continue # the owning mocker is alive
	fi
	f="label=nebius-mocker.owner=$owner"
	docker ps -aq --filter label=nebius-mocker.backend=k3s --filter "$f" | xargs docker rm -f -v >/dev/null 2>&1 || true
	docker volume ls -q --filter label=nebius-mocker.backend=k3s --filter "$f" | xargs docker volume rm -f >/dev/null 2>&1 || true
	docker network ls -q --filter label=nebius-mocker.backend=k3s --filter "$f" | xargs docker network rm >/dev/null 2>&1 || true
	echo "removed k3s leftovers of $owner"
done
