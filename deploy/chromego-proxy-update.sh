#!/usr/bin/env bash
#
# Refresh ChromeGo dynamic Hysteria2 nodes, probe candidates, and only switch
# the live Docker proxy container after a successful health check.
#

set -Eeuo pipefail

PRIMARY_BASE_URL="${PRIMARY_BASE_URL:-https://www.gitlabip.xyz/Alvin9999/PAC/refs/heads/master/backup/img/1/2/ipp}"
FALLBACK_BASE_URL="${FALLBACK_BASE_URL:-https://gitlab.com/free9999/ipupdate/-/raw/master/backup/img/1/2/ipp}"
SOURCE_IDS=(
  "1"
  "2"
  "3"
  "4"
)

BASE_DIR="${BASE_DIR:-/home/sub2api/sub2api-deploy/chromego-proxy}"
SOURCE_DIR="${SOURCE_DIR:-$BASE_DIR/source}"
CANDIDATE_DIR="${CANDIDATE_DIR:-$BASE_DIR/candidates}"
LIVE_DIR="${LIVE_DIR:-$BASE_DIR/live}"
RUNTIME_DIR="${RUNTIME_DIR:-$BASE_DIR/runtime}"

DOCKER_NETWORK="${DOCKER_NETWORK:-sub2api-deploy_sub2api-network}"
SING_BOX_IMAGE="${SING_BOX_IMAGE:-ghcr.io/sagernet/sing-box:latest}"
LIVE_CONTAINER_NAME="${LIVE_CONTAINER_NAME:-sub2api-chromego-proxy-live}"
LIVE_NETWORK_ALIAS="${LIVE_NETWORK_ALIAS:-sub2api-chromego-proxy}"
APP_CONTAINER_NAME="${APP_CONTAINER_NAME:-sub2api}"
SOCKS_PORT="${SOCKS_PORT:-1082}"

PROBE_URL="${PROBE_URL:-https://api.ipify.org}"
PROBE_RETRIES="${PROBE_RETRIES:-3}"
PROBE_INTERVAL_SECONDS="${PROBE_INTERVAL_SECONDS:-3}"
PROBE_STARTUP_SECONDS="${PROBE_STARTUP_SECONDS:-5}"
PROBE_CONNECT_TIMEOUT_SECONDS="${PROBE_CONNECT_TIMEOUT_SECONDS:-10}"
PROBE_MAX_TIME_SECONDS="${PROBE_MAX_TIME_SECONDS:-30}"
PROBE_PORT_BASE="${PROBE_PORT_BASE:-19080}"

LOCK_FILE="$RUNTIME_DIR/update.lock"
STATE_FILE="$LIVE_DIR/current.env"
LAST_RUN_FILE="$RUNTIME_DIR/last-run.log"
AUDIT_LOG_FILE="$RUNTIME_DIR/audit.log"

PROBE_CONTAINERS=()
DOWNLOADED_IDS=()
PREV_LIVE_ID=""
PREV_LIVE_CREATED=""
PREV_SOURCE_LABEL=""
PREV_EXIT_IP=""
NEW_LIVE_ID=""
NEW_LIVE_CREATED=""

log() {
  local level="$1"
  shift
  printf '[%s] %s\n' "$level" "$*"
}

info() {
  log INFO "$@"
}

warn() {
  log WARN "$@"
}

error() {
  log ERROR "$@" >&2
}

utc_now() {
  date -u +%FT%TZ
}

require_cmd() {
  local cmd="$1"
  command -v "$cmd" >/dev/null 2>&1 || {
    error "Missing required command: $cmd"
    exit 1
  }
}

cleanup() {
  local name
  for name in "${PROBE_CONTAINERS[@]:-}"; do
    docker rm -f "$name" >/dev/null 2>&1 || true
  done
}

load_current_state() {
  if docker inspect "$LIVE_CONTAINER_NAME" >/dev/null 2>&1; then
    PREV_LIVE_ID="$(docker inspect --format '{{.Id}}' "$LIVE_CONTAINER_NAME" 2>/dev/null || true)"
    PREV_LIVE_CREATED="$(docker inspect --format '{{.Created}}' "$LIVE_CONTAINER_NAME" 2>/dev/null || true)"
  fi

  if [ -f "$STATE_FILE" ]; then
    PREV_SOURCE_LABEL="$(sed -n 's/^SOURCE_LABEL=//p' "$STATE_FILE" | tail -n 1)"
    PREV_EXIT_IP="$(sed -n 's/^EXIT_IP=//p' "$STATE_FILE" | tail -n 1)"
  fi
}

append_audit_log() {
  local result="$1"
  local detail="$2"

  mkdir -p "$RUNTIME_DIR"

  {
    printf 'timestamp=%s\n' "$(utc_now)"
    printf 'result=%s\n' "$result"
    printf 'detail=%s\n' "$detail"
    printf 'previous_container_id=%s\n' "$PREV_LIVE_ID"
    printf 'previous_container_created=%s\n' "$PREV_LIVE_CREATED"
    printf 'previous_source_label=%s\n' "$PREV_SOURCE_LABEL"
    printf 'previous_exit_ip=%s\n' "$PREV_EXIT_IP"
    printf 'new_container_id=%s\n' "$NEW_LIVE_ID"
    printf 'new_container_created=%s\n' "$NEW_LIVE_CREATED"
    if [ -f "$STATE_FILE" ]; then
      sed -n 's/^SOURCE_LABEL=/new_source_label=/p' "$STATE_FILE" | tail -n 1
      sed -n 's/^EXIT_IP=/new_exit_ip=/p' "$STATE_FILE" | tail -n 1
      sed -n 's/^UPDATED_AT=/new_updated_at=/p' "$STATE_FILE" | tail -n 1
    else
      printf 'new_source_label=%s\n' ""
      printf 'new_exit_ip=%s\n' ""
      printf 'new_updated_at=%s\n' ""
    fi
    printf -- '---\n'
  } >> "$AUDIT_LOG_FILE"
}

download_source_file() {
  local source_id="$1"
  local output_file="$2"
  local meta_file="$3"
  local url

  for url in \
    "$PRIMARY_BASE_URL/hysteria2/$source_id/config.json" \
    "$FALLBACK_BASE_URL/hysteria2/$source_id/config.json"
  do
    if curl -fsSL --retry 2 --connect-timeout 15 --max-time 90 "$url" -o "$output_file"; then
      printf '%s\n' "$url" > "$meta_file"
      return 0
    fi
  done

  return 1
}

build_singbox_config() {
  local source_json="$1"
  local output_json="$2"

  python3 - "$source_json" "$output_json" "$SOCKS_PORT" <<'PY'
import json
import re
import sys

src_path, dst_path, socks_port = sys.argv[1], sys.argv[2], int(sys.argv[3])

with open(src_path, "r", encoding="utf-8") as fh:
    source = json.load(fh)

server = str(source.get("server", "")).strip()
if not server:
    raise SystemExit("missing server")

if server.startswith("[") and "]:" in server:
    host, port = server[1:].split("]:", 1)
else:
    host, sep, port = server.rpartition(":")
    if not sep or not host or not port:
        raise SystemExit(f"invalid server format: {server}")

password = str(source.get("auth", "")).strip()
if not password:
    raise SystemExit("missing auth")

bandwidth = source.get("bandwidth") or {}
tls = source.get("tls") or {}

def parse_mbps(value, default):
    if value is None:
        return default
    if isinstance(value, (int, float)):
        return int(value)
    match = re.search(r"(\d+)", str(value))
    return int(match.group(1)) if match else default

config = {
    "log": {
        "level": "info",
        "timestamp": True,
    },
    "inbounds": [
        {
            "type": "socks",
            "tag": "socks-in",
            "listen": "0.0.0.0",
            "listen_port": socks_port,
        }
    ],
    "outbounds": [
        {
            "type": "hysteria2",
            "tag": "proxy",
            "server": host,
            "server_port": int(port),
            "password": password,
            "up_mbps": parse_mbps(bandwidth.get("up"), 10),
            "down_mbps": parse_mbps(bandwidth.get("down"), 50),
            "tls": {
                "enabled": True,
                "server_name": str(tls.get("sni") or "www.microsoft.com"),
                "insecure": bool(tls.get("insecure", False)),
            },
        }
    ],
    "route": {
        "final": "proxy",
    },
}

with open(dst_path, "w", encoding="utf-8") as fh:
    json.dump(config, fh, ensure_ascii=False, indent=2)
    fh.write("\n")
PY
}

start_probe_container() {
  local config_file="$1"
  local container_name="$2"
  local port="$3"

  docker rm -f "$container_name" >/dev/null 2>&1 || true

  docker run -d \
    --name "$container_name" \
    --network "$DOCKER_NETWORK" \
    --label com.sub2api.chromego_proxy=probe \
    -p "127.0.0.1:${port}:${SOCKS_PORT}" \
    -v "$config_file:/etc/sing-box/config.json:ro" \
    "$SING_BOX_IMAGE" \
    run -c /etc/sing-box/config.json >/dev/null

  PROBE_CONTAINERS+=("$container_name")
}

probe_candidate() {
  local container_name="$1"
  local port="$2"
  local result
  local attempt

  sleep "$PROBE_STARTUP_SECONDS"

  if ! docker ps --format '{{.Names}}' | grep -qx "$container_name"; then
    docker logs --tail 120 "$container_name" >&2 || true
    return 1
  fi

  for ((attempt = 1; attempt <= PROBE_RETRIES; attempt++)); do
    if result="$(
      curl -fsS \
        --connect-timeout "$PROBE_CONNECT_TIMEOUT_SECONDS" \
        --max-time "$PROBE_MAX_TIME_SECONDS" \
        -x "socks5h://127.0.0.1:${port}" \
        "$PROBE_URL" 2>/dev/null
    )"; then
      printf '%s' "$result"
      return 0
    fi

    sleep "$PROBE_INTERVAL_SECONDS"
  done

  docker logs --tail 120 "$container_name" >&2 || true
  return 1
}

verify_from_app_container() {
  if ! docker ps --format '{{.Names}}' | grep -qx "$APP_CONTAINER_NAME"; then
    warn "App container $APP_CONTAINER_NAME is not running; skip in-app verification"
    return 0
  fi

  docker exec "$APP_CONTAINER_NAME" sh -lc \
    "curl -fsS --connect-timeout ${PROBE_CONNECT_TIMEOUT_SECONDS} --max-time ${PROBE_MAX_TIME_SECONDS} -x socks5h://${LIVE_NETWORK_ALIAS}:${SOCKS_PORT} ${PROBE_URL}" \
    >/dev/null
}

promote_live_container() {
  local candidate_config="$1"
  local source_label="$2"
  local exit_ip="$3"
  local next_name="${LIVE_CONTAINER_NAME}-next-$(date +%s)"
  local existing

  cp "$candidate_config" "$LIVE_DIR/config.json"
  cat > "$STATE_FILE" <<EOF
SOURCE_LABEL=$source_label
EXIT_IP=$exit_ip
UPDATED_AT=$(date -u +%FT%TZ)
EOF

  docker rm -f "$next_name" >/dev/null 2>&1 || true
  docker run -d \
    --name "$next_name" \
    --network "$DOCKER_NETWORK" \
    --network-alias "$LIVE_NETWORK_ALIAS" \
    --label com.sub2api.chromego_proxy=live \
    -v "$LIVE_DIR/config.json:/etc/sing-box/config.json:ro" \
    "$SING_BOX_IMAGE" \
    run -c /etc/sing-box/config.json >/dev/null

  sleep "$PROBE_STARTUP_SECONDS"

  if ! docker ps --format '{{.Names}}' | grep -qx "$next_name"; then
    docker logs --tail 120 "$next_name" >&2 || true
    docker rm -f "$next_name" >/dev/null 2>&1 || true
    return 1
  fi

  verify_from_app_container

  while IFS= read -r existing; do
    [ -n "$existing" ] || continue
    if [ "$existing" != "$next_name" ]; then
      docker rm -f "$existing" >/dev/null 2>&1 || true
    fi
  done < <(docker ps -a --filter label=com.sub2api.chromego_proxy=live --format '{{.Names}}')

  if [ "$next_name" != "$LIVE_CONTAINER_NAME" ]; then
    docker rename "$next_name" "$LIVE_CONTAINER_NAME"
  fi

  NEW_LIVE_ID="$(docker inspect --format '{{.Id}}' "$LIVE_CONTAINER_NAME")"
  NEW_LIVE_CREATED="$(docker inspect --format '{{.Created}}' "$LIVE_CONTAINER_NAME")"
}

main() {
  local source_id
  local source_path
  local meta_path
  local candidate_name
  local candidate_dir
  local candidate_config
  local probe_name
  local probe_port
  local source_label
  local exit_ip
  local index=0
  local promoted=0

  require_cmd docker
  require_cmd curl
  require_cmd python3
  require_cmd flock

  mkdir -p "$SOURCE_DIR" "$CANDIDATE_DIR" "$LIVE_DIR" "$RUNTIME_DIR"
  load_current_state

  exec 9>"$LOCK_FILE"
  if ! flock -n 9; then
    warn "Another proxy update is already running; skip this round"
    append_audit_log "skipped" "lock_busy"
    exit 0
  fi

  if ! docker network inspect "$DOCKER_NETWORK" >/dev/null 2>&1; then
    error "Docker network not found: $DOCKER_NETWORK"
    append_audit_log "failed" "docker_network_missing"
    exit 1
  fi

  if ! docker image inspect "$SING_BOX_IMAGE" >/dev/null 2>&1; then
    error "Docker image not found locally: $SING_BOX_IMAGE"
    error "Pull it first or set SING_BOX_IMAGE to an available image"
    append_audit_log "failed" "docker_image_missing"
    exit 1
  fi

  info "Refreshing ChromeGo Hysteria2 sources"

  for source_id in "${SOURCE_IDS[@]}"; do
    source_path="$SOURCE_DIR/hysteria2_${source_id}.json"
    meta_path="$SOURCE_DIR/hysteria2_${source_id}.json.url"
    if download_source_file "$source_id" "$source_path" "$meta_path"; then
      DOWNLOADED_IDS+=("$source_id")
      info "Downloaded hysteria2_${source_id}.json"
    else
      warn "Failed to download hysteria2_${source_id}.json"
    fi
  done

  if [ "${#DOWNLOADED_IDS[@]}" -eq 0 ]; then
    warn "No fresh candidate downloaded in this round; existing live proxy kept unchanged"
    append_audit_log "kept" "no_fresh_candidate_downloaded"
    return 1
  fi

  for source_id in "${DOWNLOADED_IDS[@]}"; do
    source_path="$SOURCE_DIR/hysteria2_${source_id}.json"
    candidate_name="hysteria2_${source_id}"
    candidate_dir="$CANDIDATE_DIR/$candidate_name"
    candidate_config="$candidate_dir/config.json"
    meta_path="$SOURCE_DIR/$candidate_name.json.url"
    probe_name="sub2api-chromego-probe-${candidate_name}"
    probe_port=$((PROBE_PORT_BASE + index))
    index=$((index + 1))

    mkdir -p "$candidate_dir"

    if ! build_singbox_config "$source_path" "$candidate_config"; then
      warn "Skip invalid candidate: $candidate_name"
      continue
    fi

    start_probe_container "$candidate_config" "$probe_name" "$probe_port"
    source_label="$(cat "$meta_path" 2>/dev/null || printf '%s' "$candidate_name")"

    if exit_ip="$(probe_candidate "$probe_name" "$probe_port")"; then
      info "Healthy candidate: $candidate_name exit_ip=$exit_ip"
      if promote_live_container "$candidate_config" "$source_label" "$exit_ip"; then
        info "Promoted live candidate: $candidate_name"
        promoted=1
        break
      fi
      warn "Candidate promotion failed: $candidate_name"
    else
      warn "Candidate unhealthy: $candidate_name"
    fi
  done

  if [ "$promoted" -eq 1 ]; then
    {
      printf 'updated_at=%s\n' "$(utc_now)"
      cat "$STATE_FILE"
    } > "$LAST_RUN_FILE"
    append_audit_log "promoted" "healthy_candidate_switched"
    info "ChromeGo proxy update completed"
    return 0
  fi

  warn "No healthy candidate promoted; existing live proxy kept unchanged"
  append_audit_log "kept" "no_healthy_candidate_promoted"
  return 1
}

trap cleanup EXIT
main "$@"
