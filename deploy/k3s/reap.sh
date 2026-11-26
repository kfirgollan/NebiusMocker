#!/usr/bin/env bash
# Removes the k3s containers, volumes and networks of mk8s k3s backends whose
# mocker is gone (killed before it could clean up). Every object carries
# nebius-mocker.owner=<webhook URL>, whose port is the owning mocker's edge
# port; objects whose owner port no longer accepts connections are removed.
# A running mocker reaps its own leftovers at startup; this is for owners
# that will not come back (e.g. test runs on random ports).
set -euo pipefail
exec "$(dirname "$0")/../docker/reap.sh" nebius-mocker.backend=k3s
