#!/usr/bin/env bash
# Aggregate HEALTHCHECK -- Docker only keeps the last HEALTHCHECK declared in
# the image, so this script checks every service instead of relying on each
# app's own individual healthcheck.
#
# NOTE: when TUNNEL_ONLY=true + TUNNEL_TOKEN set, app services bind to
# 127.0.0.1 instead of 0.0.0.0 -- that does not affect this script since it
# runs *inside* the container and always reaches them via loopback.
set -u
FAIL=0

check() {
  local name="$1" url="$2"
  if ! curl -fsS --max-time 3 "$url" >/dev/null 2>&1; then
    echo "[healthcheck] FAIL: ${name} (${url})"
    FAIL=1
  fi
}

check "omniroute"  "http://127.0.0.1:${OMNIROUTE_PORT:-20128}/api/monitoring/health"
check "mimo"       "http://127.0.0.1:${MIMO_PORT:-3000}/"
check "zai-api"    "http://127.0.0.1:${ZAI_PORT:-3001}/health"
check "kimi-api"    "http://127.0.0.1:${KIMI_PORT:-3002}/"
check "grok2api"   "http://127.0.0.1:${GROK2API_PORT:-8000}/healthz"
check "metacubexd" "http://127.0.0.1:${CONTROL_PORT:-8080}/api/control/health"
check "mihomo"     "http://127.0.0.1:${CLASH_API_PORT:-9090}/version"

# cloudflared has no HTTP endpoint to poll; only flag it as unhealthy if a
# token was actually provided (meaning the tunnel is supposed to be running).
if [ -n "${TUNNEL_TOKEN:-}" ]; then
  if ! pgrep -x cloudflared >/dev/null 2>&1; then
    echo "[healthcheck] FAIL: cloudflared (TUNNEL_TOKEN set but process not running)"
    FAIL=1
  fi
fi

exit "${FAIL}"
