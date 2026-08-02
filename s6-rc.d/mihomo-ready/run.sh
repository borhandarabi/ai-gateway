#!/command/with-contenv bash
# Blocks until mihomo's control API answers, so app services never race the
# TUN interface coming up.
# NOTE: mihomo is now spawned by metacubexd (not a separate s6 service).
# This oneshot depends on metacubexd and waits for its spawned mihomo to be ready.
set -e
TRIES=0
until curl -fsS "http://127.0.0.1:${CLASH_API_PORT:-9090}/version" >/dev/null 2>&1; do
  TRIES=$((TRIES + 1))
  if [ "${TRIES}" -ge 60 ]; then
    echo "[mihomo-ready] mihomo did not become healthy in time" >&2
    exit 1
  fi
  sleep 1
done
echo "[mihomo-ready] mihomo control API is up"
