#!/usr/bin/env bash
# Aggregate HEALTHCHECK -- Docker only keeps the last HEALTHCHECK declared in
# the image, so this script checks every service instead of relying on each
# app's own individual healthcheck.
#
# NOTE: when TUNNEL_ONLY=true + TUNNEL_TOKEN set, app services bind to
# 127.0.0.1 instead of 0.0.0.0 -- that does not affect this script since it
# runs *inside* the container and always reaches them via loopback.
#
# Services the admin stopped from the dashboard carry an s6 "down" file in
# their live servicedir ($S6_SERVICE_DIR/<name>/down); those are SKIPPED --
# a deliberately stopped service must never mark the whole container
# unhealthy.
set -u
FAIL=0

SVC_DIR="${S6_SERVICE_DIR:-/run/service}"

check() {
  local name="$1" url="$2"
  if ! curl -fsS --max-time 3 "$url" >/dev/null 2>&1; then
    echo "[healthcheck] FAIL: ${name} (${url})"
    FAIL=1
  fi
}

check_if_up() {
  local name="$1" url="$2"
  if [ -f "$SVC_DIR/$name/down" ]; then
    echo "[healthcheck] SKIP: $name (stopped via dashboard)"
    return 0
  fi
  if [ ! -d "$SVC_DIR/$name/supervise" ]; then
    echo "[healthcheck] SKIP: $name (not supervised yet)"
    return 0
  fi
  check "$name" "$url"
}

# core: always checked
check singbox "http://127.0.0.1:${SINGBOX_API_PORT:-9090}/version"

# toggleable services: only checked when actually running
check_if_up zenfreeapi   "http://127.0.0.1:${ZENFREEAPI_PORT:-3008}/health"
check_if_up mimo         "http://127.0.0.1:${MIMO_PORT:-3003}/"
check_if_up kimi         "http://127.0.0.1:${KIMI_PORT:-3002}/"
check_if_up zai          "http://127.0.0.1:${ZAI_PORT:-3001}/health"
check_if_up grok2api     "http://127.0.0.1:${GROK2API_PORT:-3004}/healthz"
check_if_up qwen2api     "http://127.0.0.1:${QWEN2API_PORT:-3006}"
check_if_up flaresolverr "http://127.0.0.1:${FLARESOLVERR_PORT:-8191}/health"

# cloudflared has no HTTP endpoint to poll; only flag it as unhealthy if a
# token was actually provided (tunnel supposed to run) AND the admin has not
# stopped it from the dashboard.
if [ -n "${TUNNEL_TOKEN:-}" ] && [ ! -f "$SVC_DIR/cloudflared/down" ]; then
  if ! pgrep -x cloudflared >/dev/null 2>&1; then
    echo "[healthcheck] FAIL: cloudflared (TUNNEL_TOKEN set but process not running)"
    FAIL=1
  fi
fi

exit "${FAIL}"
