#!/command/with-contenv bash
# gateway-entrypoint -- runs BEFORE /init (s6-overlay) on container boot.
#
# Purpose: decide which s6 longrun services start "up" vs "down" at boot,
# WITHOUT touching the image's service definitions. s6-overlay honours an
# empty `down` file in a service definition dir: if present when s6-rc
# compiles/starts, the longrun comes up supervised but NOT running
# (status: down, no process). The dashboard's existing Start/Stop buttons
# (/api/s6/services/{name}/start|stop -> s6-svc -u/-d) work exactly as
# before; we only choose the INITIAL state.
#
# State model (persistent across restarts/redeploys), kept in
# /data/s6-state (survives container replacement because /data is a volume):
#   <name>.up     -> force UP at boot   (user clicked Start, or ENABLE_*=true)
#   <name>.down   -> force DOWN at boot (user clicked Stop, or ENABLE_*=false)
#   (no marker)   -> default policy applies
#
# Default policy: everything DOWN except the always-on core -- singbox
# (the gateway itself + proxy kernel) is NOT in the managed list and always
# boots; cloudflared boots only when TUNNEL_TOKEN is set (the tunnel is the
# management path for many deployments).
#
# ENABLE_* env vars (evaluated once per boot, before marker logic):
#   ENABLE_FLARESOLVERR=true|false etc. force that service's initial state
#   and persist the choice via markers.
set -e

STATE_DIR="${S6_STATE_DIR:-/data/s6-state}"
SRC_DIR="${S6_SOURCE_DIR:-/etc/s6-overlay/s6-rc.d}"
mkdir -p "$STATE_DIR" 2>/dev/null || STATE_DIR="/tmp/s6-state"
mkdir -p "$STATE_DIR" 2>/dev/null || true

# Managed longruns. Must match servicedir names under $SRC_DIR. The core
# gateway service (singbox) is deliberately NOT managed here: it always
# boots, or there is no dashboard to turn anything on.
ALL_SERVICES="zenfreeapi mimo zai kimi deepseek grok2api qwen2api flaresolverr cloudflared"

is_true() {
  case "${1:-}" in
    true|TRUE|True|1|yes|YES|Yes|on|ON|On) return 0 ;;
    *) return 1 ;;
  esac
}

for svc in $ALL_SERVICES; do
  svc_dir="$SRC_DIR/$svc"
  [ -d "$svc_dir" ] || continue # unknown service in list -> skip silently
  down_file="$svc_dir/down"

  # 1) explicit env override wins for THIS boot and persists via marker
  env_val="$(printenv "ENABLE_$(echo "$svc" | tr 'a-z' 'A-Z')" || true)"
  if [ -n "$env_val" ]; then
    rm -f "$STATE_DIR/$svc.up" "$STATE_DIR/$svc.down" 2>/dev/null || true
    if is_true "$env_val"; then
      : > "$STATE_DIR/$svc.up"
      rm -f "$down_file"
    else
      : > "$STATE_DIR/$svc.down"
      : > "$down_file"
    fi
    continue
  fi

  # 2) persistent marker from a previous dashboard toggle wins
  if [ -f "$STATE_DIR/$svc.up" ]; then
    rm -f "$down_file"
    continue
  fi
  if [ -f "$STATE_DIR/$svc.down" ]; then
    : > "$down_file"
    continue
  fi

  # 3) default policy: core + tunnel up, everything else off
  keep_up=0
  case "$svc" in
    cloudflared)
      [ -n "${TUNNEL_TOKEN:-}" ] && keep_up=1
      ;;
  esac
  if [ "$keep_up" -eq 1 ]; then
    rm -f "$down_file"
  else
    : > "$down_file"
  fi
done

echo "[gateway-entrypoint] initial service states:"
for svc in $ALL_SERVICES; do
  if [ -f "$SRC_DIR/$svc/down" ]; then
    echo "[gateway-entrypoint]   $svc: down"
  else
    echo "[gateway-entrypoint]   $svc: up"
  fi
done

# Hand control to s6-overlay with the upstream argv contract. exec keeps
# PID 1 semantics (signal handling, zombie reaping) intact.
exec /init
