#!/usr/bin/env bash
# Brings a throwaway Harbor (official goharbor images) up or down: the image
# backend of NebiusMocker's Container Registry, for local use and for
# make e2e-registry. See docs/services/registry.md.
#
#   harbor.sh up   <workdir>   generate config with goharbor/prepare, start, wait healthy,
#                              write <workdir>/env: HARBOR_URL (from the host), HARBOR_NETWORK and
#                              HARBOR_INTERNAL_URL (for containers such as the mocker),
#                              HARBOR_USERNAME, HARBOR_PASSWORD, HARBOR_VERSION
#   harbor.sh down <workdir>   stop and remove containers, networks, volumes and <workdir>
#
# Everything is scoped to a unique compose project (nmharbor-<random>): no fixed
# container names, no fixed host ports (the proxy binds 127.0.0.1:<random>),
# and Harbor's syslog container is not used for the other services' logs.
set -euo pipefail

HARBOR_VERSION="${HARBOR_VERSION:-v2.16.0}"
ADMIN_PASSWORD="${HARBOR_ADMIN_PASSWORD:-Harbor12345}"
HEALTH_TIMEOUT="${HARBOR_HEALTH_TIMEOUT:-420}"

log() { echo "harbor.sh: $*" >&2; }

compose() {
	local w="$1"
	shift
	docker compose -p "$(cat "$w/project")" -f "$w/docker-compose.yml" -f "$w/override.yml" "$@"
}

write_harbor_yml() {
	local w="$1"
	cat >"$w/input/harbor.yml" <<EOF
hostname: localhost
http:
  port: 80
harbor_admin_password: ${ADMIN_PASSWORD}
database:
  password: root123
  max_idle_conns: 50
  max_open_conns: 100
  conn_max_lifetime: 5m
  conn_max_idle_time: 0
data_volume: ${w}/data
jobservice:
  max_job_workers: 2
  max_job_duration_hours: 24
  job_loggers:
    - STD_OUTPUT
  logger_sweeper_duration: 1
notification:
  webhook_job_max_retry: 3
  webhook_job_http_client_timeout: 3
log:
  level: info
  local:
    rotate_count: 5
    rotate_size: 20M
    location: ${w}/log
_version: ${HARBOR_VERSION#v}
upload_purging:
  enabled: false
  age: 168h
  interval: 24h
  dryrun: false
cache:
  enabled: false
  expire_hours: 24
EOF
}

# write_override drops fixed container names and the syslog logging driver
# (which needs Harbor's log container on fixed host port 1514), and publishes
# the proxy on a random loopback port.
write_override() {
	local w="$1"
	{
		echo "services:"
		docker compose -p "$(cat "$w/project")" -f "$w/docker-compose.yml" config --services | while read -r svc; do
			echo "  ${svc}:"
			echo "    container_name: !reset null"
			echo "    logging: !override"
			echo "      driver: json-file"
			case "$svc" in
			log) echo "    ports: !reset []" ;;
			proxy) echo "    ports: !override [\"127.0.0.1::8080\"]" ;;
			esac
		done
	} >"$w/override.yml"
}

wait_healthy() {
	local url="$1" deadline=$((SECONDS + HEALTH_TIMEOUT)) body=""
	while ((SECONDS < deadline)); do
		body="$(curl -fsS --max-time 5 "$url/api/v2.0/health" 2>/dev/null || true)"
		if [[ "$body" == *'"status":"healthy"'* ]] && [[ "$body" != *'unhealthy'* ]]; then
			# The registry may still refuse /v2/ for a moment after core reports healthy.
			if curl -fsS --max-time 5 -u "admin:${ADMIN_PASSWORD}" "$url/v2/" >/dev/null 2>&1; then
				return 0
			fi
		fi
		sleep 3
	done
	log "Harbor at $url not healthy after ${HEALTH_TIMEOUT}s; last health: ${body:-<none>}"
	return 1
}

up() {
	local w="$1"
	mkdir -p "$w/input" "$w/data" "$w/log" "$w/common/config"
	echo "nmharbor-$(LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 8)" >"$w/project"
	write_harbor_yml "$w"
	log "generating config with goharbor/prepare:${HARBOR_VERSION} in $w"
	docker run --rm \
		-v "$w/input:/input" \
		-v "$w/data:/data" \
		-v "$w:/compose_location" \
		-v "$w/common/config:/config" \
		-v "$w:/hostfs$w" \
		--privileged \
		"goharbor/prepare:${HARBOR_VERSION}" prepare >"$w/prepare.log" 2>&1 || {
		cat "$w/prepare.log" >&2
		return 1
	}
	write_override "$w"
	log "starting project $(cat "$w/project")"
	compose "$w" up -d --quiet-pull >"$w/up.log" 2>&1 || {
		cat "$w/up.log" >&2
		return 1
	}
	local hostport
	hostport="$(compose "$w" port proxy 8080)"
	local url="http://127.0.0.1:${hostport##*:}"
	log "waiting for $url to become healthy"
	if ! wait_healthy "$url"; then
		compose "$w" ps >&2 || true
		compose "$w" logs --tail 30 >&2 || true
		return 1
	fi
	cat >"$w/env" <<EOF
export HARBOR_URL=$url
export HARBOR_NETWORK=$(cat "$w/project")_harbor
export HARBOR_INTERNAL_URL=http://proxy:8080
export HARBOR_USERNAME=admin
export HARBOR_PASSWORD=$ADMIN_PASSWORD
export HARBOR_VERSION=$HARBOR_VERSION
EOF
	log "Harbor ready at $url"
}

down() {
	local w="$1"
	[[ -d "$w" ]] || return 0
	if [[ -f "$w/project" && -f "$w/docker-compose.yml" ]]; then
		log "removing project $(cat "$w/project")"
		compose "$w" down -v --remove-orphans --timeout 10 >/dev/null 2>&1 || true
	fi
	# Harbor's containers write data as their own UIDs; delete it from a container.
	docker run --rm -v "$w:/w" --entrypoint sh "goharbor/prepare:${HARBOR_VERSION}" -c 'rm -rf /w/* /w/.[!.]*' >/dev/null 2>&1 || true
	rm -rf "$w"
}

case "${1:-}" in
up) up "${2:?workdir}" ;;
down) down "${2:?workdir}" ;;
*)
	echo "usage: $0 up|down <workdir>" >&2
	exit 2
	;;
esac
