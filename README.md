# ai-gateway

Single Docker image running, as separate s6-supervised processes sharing one
network namespace:

- **OmniRoute** (Node/Next.js) — AI gateway/router, port `20128`
- **MimoApi** (Go) — Mimo/Xiaomi provider proxy, port `3000`
- **zai-api / GlmApi** (Go) — Z.ai/GLM provider proxy, port `3001`
- **metacubexd + mihomo** (Node + Go) — proxy dashboard/control (`8080`/`9090`)
  and proxy kernel: TUN + SOCKS/HTTP mixed-port (`7890`), both active
- **cloudflared** (Go, prebuilt) — optional Cloudflare Tunnel, always
  installed, idle unless `TUNNEL_TOKEN` is set

## Why one image, and the trade-offs

This was a deliberate choice (plain-Docker deployment target, not
Kubernetes/Compose-as-orchestration) over the more conventional
multi-container approach. The cost of that choice — and how each piece of
this repo addresses it — is documented inline in the Dockerfile and each
`s6-rc.d/*/run` script; the short version:

- All four apps share one network namespace, so mihomo's TUN mode captures
  **all** outbound traffic in the container unless explicitly excluded
  (see `mihomo/config.yaml`'s `PROCESS-NAME,cloudflared,DIRECT` rule).
- Only one `HEALTHCHECK` is possible per image — `healthcheck.sh` polls every
  service instead of relying on each app's own healthcheck.
- Only one PID 1 is possible — `s6-overlay` supervises all processes, with an
  explicit start order (see below) instead of Docker's own restart policy.

## Start order

```
cloudflared ─┐
             ├─▶ mihomo ─▶ mihomo-ready ─▶ omniroute
network-mode-init ────────────────────────▶ mimo
                                           ▶ zai-api
                                           ▶ metacubexd
```

- `cloudflared` starts first and is excluded from TUN routing (both by start
  order and by an explicit rule, since long-lived connections reconnect
  periodically and could otherwise get captured after TUN comes up).
- `mihomo-ready` blocks until mihomo's control API responds, so the other
  four services never race the TUN interface coming up.
- `network-mode-init` computes `BIND_ADDR` (`0.0.0.0` normally, `127.0.0.1`
  when `TUNNEL_ONLY=true` + `TUNNEL_TOKEN` is set) and every app service reads
  it at startup.

## Deploying on Railway

See `railway/README.md` — uses `railway.json` + `railway/Dockerfile` (a
git-clone-based build, since Railway's builder doesn't support Buildx named
build-contexts). Important: **Railway does not support TUN mode** (no
privileged containers / `NET_ADMIN` / `/dev/net/tun`) — mihomo runs in
mixed-port-only mode there via `DISABLE_TUN=true`.

## Quick start

```bash
cp .env.example .env
# edit .env -- at minimum set AUTH_TOKEN for zai-api

./build.sh                 # or: docker compose build
docker compose up -d
docker compose logs -f
```

CI equivalent: `.github/workflows/docker-build.yml` (multi-arch, pushes to
GHCR, same named-build-context wiring as `build.sh`).

## Open items (explicitly unresolved — do not treat as done)

1. **zai-api repo URL** — not pushed yet. `GLM_REPO` is blank/placeholder
   everywhere (`.env.example`, `docker-compose.yml`, the workflow's
   `workflow_dispatch` input / `GLM_REPO_URL` secret, `build.sh`). Until it
   exists, point `GLM_REPO` at a local checkout directory instead.
2. **zai-api's SQLite driver** — `go.mod` was pre-`go mod tidy` when reviewed,
   so it's unknown whether the driver needs CGO (`mattn/go-sqlite3`) or is
   pure Go (`modernc.org/sqlite`). The `glm-builder` stage builds with
   `CGO_ENABLED=1` + a static musl link as a safe default for either case --
   re-check once `go mod tidy` has actually run against the real source.
3. **MimoApi / metacubexd bind-address support** — `TUNNEL_ONLY` mode assumes
   every service honors a `HOST`/bind-address env var. This is confirmed for
   OmniRoute (`HOSTNAME`) and zai-api (`HOST`), but MimoApi's and
   metacubexd's actual source weren't inspected for this — if either
   hardcodes `0.0.0.0`, `TUNNEL_ONLY` won't fully close it off and it'll need
   a small upstream patch or an iptables-level fallback.
4. **metacubexd + mihomo double-spawn risk** — metacubexd's own
   `docker-entrypoint.sh` normally spawns mihomo itself as a child process.
   Here mihomo runs as its own independent s6 service instead — check
   metacubexd's server startup for an env var to point it at the
   already-running kernel rather than spawning a second `mihomo` process.
5. **mihomo control API secret** — `mihomo/config.yaml`'s `secret:` field is
   empty; set a real one before exposing port `9090` anywhere non-trusted.
