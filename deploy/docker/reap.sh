#!/usr/bin/env bash
# Removes the Docker containers, volumes and networks carrying any of the
# given labels (e.g. nebius-mocker.backend=msp-postgresql) whose owner is
# gone. Every object a mocker backend or a test creates carries
# nebius-mocker.owner=<edge address or URL>; its port is the owning mocker's
# edge port. Objects whose owner port still accepts connections belong to a
# live mocker (possibly a concurrent test run) and are left alone; objects of
# dead owners, and objects without an owner label, are removed.
#
# A running mocker reaps its own leftovers at startup; this is for owners
# that will not come back (e.g. test runs on random ports that were killed).
#
# Usage: deploy/docker/reap.sh <label=value>...
set -euo pipefail

alive() {
	local port=${1##*:}
	[[ $port =~ ^[0-9]+$ ]] || return 1
	(exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null
}

owner_label='{{.Label "nebius-mocker.owner"}}'

for kind in container volume network; do
	case $kind in
	container) list=(docker ps -a) rm=(docker rm -f -v) format="{{.ID}}	$owner_label" ;;
	volume) list=(docker volume ls) rm=(docker volume rm -f) format="{{.Name}}	$owner_label" ;;
	network) list=(docker network ls) rm=(docker network rm) format="{{.Name}}	$owner_label" ;;
	esac
	for selector in "$@"; do
		while IFS=$'\t' read -r id owner; do
			[ -n "$id" ] || continue
			if [ -n "$owner" ] && alive "$owner"; then
				continue # the owning mocker is alive
			fi
			"${rm[@]}" "$id" >/dev/null 2>&1 || true
			echo "removed $kind $id ($selector, owner ${owner:-none})"
		done < <("${list[@]}" --filter "label=$selector" --format "$format" 2>/dev/null || true)
	done
done
