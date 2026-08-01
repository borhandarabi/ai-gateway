#!/command/with-contenv bash
# Decides whether app services bind to 0.0.0.0 (normal) or 127.0.0.1
# (TUNNEL_ONLY mode: only cloudflared can reach them locally; every other
# published port becomes unreachable since nothing listens on the
# container-facing interface).
set -e
BIND_ADDR="0.0.0.0"
if [ -n "${TUNNEL_TOKEN:-}" ] && [ "${TUNNEL_ONLY:-false}" = "true" ]; then
  BIND_ADDR="127.0.0.1"
  echo "[network-mode-init] TUNNEL_ONLY=true and TUNNEL_TOKEN set -> binding to 127.0.0.1"
else
  echo "[network-mode-init] normal mode -> binding to 0.0.0.0"
fi
mkdir -p /run/s6/container_environment
printf '%s' "${BIND_ADDR}" > /run/s6/container_environment/BIND_ADDR
