# ai-gateway

Single Docker image running, as separate s6-supervised processes sharing one
network namespace:

- **OmniRoute** (Node/Next.js) — AI gateway/router, port `20128` -- pulled as
  the official prebuilt image (`diegosouzapw/omniroute:latest-web`, confirmed
  multi-arch amd64+arm64) and used as this image's base, rather than built
  from source (removes its Next.js/Turbopack build from this pipeline
  entirely -- see the OOM history below)
- **MimoApi** (Go) — Mimo/Xiaomi provider proxy, port `3000`
- **zai-api / GlmApi** (Go) — Z.ai/GLM provider proxy, port `3001`
- **kimi-api** (Go) — kimi.com proxy, port `3002`
- **grok2api-go** (Go + Node/Vite) — Grok (xAI) provider proxy + dashboard, port `8000`
- **mihomo** (Go) — proxy kernel: TUN + SOCKS/HTTP mixed-port (`7890`) +
  Clash API (`9090`)
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
                                           ▶ kimi-api
                                           ▶ grok2api
                                           ▶ metacubexd
```

- `cloudflared` starts first and is excluded from TUN routing (both by start
  order and by an explicit rule, since long-lived connections reconnect
  periodically and could otherwise get captured after TUN comes up).
- `mihomo-ready` blocks until mihomo's control API responds, so the other
  services never race the TUN interface coming up.
- `network-mode-init` computes `BIND_ADDR` (`0.0.0.0` normally, `127.0.0.1`
  when `TUNNEL_ONLY=true` + `TUNNEL_TOKEN` is set) and every app service reads
  it at startup.

## s6-rc oneshot scripts (important!)

The `network-mode-init` and `mihomo-ready` services are **oneshot** type in
s6-rc. Their `up` files must be valid **execline** scripts (not bash with
shebangs), because `s6-rc-compile` strips the shebang and parses the body as
execline. The actual bash logic lives in a separate `run.sh` file, and the
`up` file is a thin execline wrapper:

```
s6-rc.d/network-mode-init/up       → execline: with-contenv ./run.sh
s6-rc.d/network-mode-init/run.sh   → bash: the actual logic
s6-rc.d/mihomo-ready/up            → execline: with-contenv ./run.sh
s6-rc.d/mihomo-ready/run.sh        → bash: the actual logic
```

**Do not** put `#!/command/with-contenv bash` in oneshot `up` files —
`s6-rc-compile` will silently drop the service from the compiled database,
causing all dependent services to never start.

## Quick start

```bash
cp .env.example .env
# edit .env -- at minimum set ZAI_AUTH_TOKEN for zai-api
# and KIMI_ACCESS_TOKEN for kimi-api (if using kimi-api)

# Note: You must provide build contexts for external repositories
./build.sh                 # or see docker build command below
docker compose up -d
docker compose logs -f

# Manual docker build with required build-contexts:
# docker buildx build \
#   --build-context deepseek_src=https://github.com/izaart95-jpg/DeepSeekFreeAPI.git \
#   --build-context zai_src=https://github.com/izaart95-jpg/GLM-Free-API.git \
#   -t ghcr.io/borhandarabi/ai-gateway:latest \
#   --push \
#   .
```

CI equivalent: `.github/workflows/docker-build.yml` -- builds `linux/amd64`
and `linux/arm64` as **separate jobs** (native runners, no QEMU sharing a
runner with the other arch's build), then merges them into one multi-arch
manifest. This was originally needed to avoid a "cannot allocate memory"
failure from a combined build, but since OmniRoute is now pulled as a
prebuilt image (see above) instead of built from source, the actual cause of
that OOM -- its Next.js/Turbopack build -- no longer runs in this pipeline
at all. The per-arch job split and swap-space step are kept as cheap
insurance for the remaining Go/Node builds.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TUNNEL_TOKEN` | (empty) | Cloudflare Tunnel token. Leave empty to disable tunnel. |
| `TUNNEL_ONLY` | `false` | When `true` + `TUNNEL_TOKEN` set, bind services to 127.0.0.1. |
| `OMNIROUTE_PORT` | `20128` | OmniRoute port. |
| `MIMO_PORT` | `3003` | MimoApi port. |
| `KIMI_PORT` | `3002` | kimi-api port. |
| `KIMI_ACCESS_TOKEN` | (empty) | **Required** for kimi-api. Get from kimi.com. Falls back to `KIMI_TOKEN`. |
| `KIMI_TOKEN` | `Waguri` | Legacy fallback for Kimi token. |
| `ZAI_PORT` | `3001` | zai-api port. |
| `ZAI_AUTH_TOKEN` | `Waguri` | Auth token for zai-api. Change in production! |
| `ZAI_AGENT_MODE` | `true` | Enable agent mode for zai-api. |
| `GROK2API_PORT` | `3004` | grok2api port. |
| `CLASH_API_PORT` | `9090` | mihomo Clash API port. |
| `MIXED_PORT` | `7890` | mihomo SOCKS/HTTP mixed port. |

## Open items (explicitly unresolved — do not treat as done)

1. **zai-api repo URL** — not pushed yet. `ZAI_REPO` is blank/placeholder
   everywhere (`.env.example`, `docker-compose.yml`, the workflow's
   `workflow_dispatch` input / `ZAI_REPO_URL` secret, `build.sh`). Until it
   exists, point `ZAI_REPO` at a local checkout directory instead.
3. **mihomo control API secret** — `mihomo/config.yaml`'s `secret:` field is
   empty; set a real one before exposing port `9090` anywhere non-trusted.
4. **kimi-api requires valid token** — kimi-api will fail to start if
   `KIMI_ACCESS_TOKEN` is not set to a valid kimi.com token. The placeholder
   `Waguri` does not work. Set this in your environment before deploying.

## Resolved issues

- **s6-rc oneshot format** — `network-mode-init` and `mihomo-ready` `up` files
  were written as bash scripts with shebangs. `s6-rc-compile` treats `up` files
  as execline, silently dropping services from the compiled database. Fixed by
  converting to execline wrappers calling separate `run.sh` bash scripts.
- **musl libc missing** — zai-api and kimi-api are built with `golang:alpine`
  (musl-linked), but the runtime image is Debian (glibc). Fixed by installing
  `musl` package in the runtime image.
- **grok2api entrypoint conflict** — upstream `docker/entrypoint.sh` copies
  config to `/app/config.yaml`, conflicting with OmniRoute's `/app` directory.
  Fixed by bypassing the entrypoint and running grok2api directly via su-exec
  with a wrapper that generates config and fixes permissions.
- **grok2api config format** — upstream requires specific yaml structure
  (`server.listen`, `secrets.jwtSecret`, `secrets.credentialEncryptionKey`,
  `bootstrapAdmin`). Fixed with auto-generated default config.
- **zai-api token-generator** — run script referenced `token-generator` but
  the actual binary is `token-collector`. Also, `exec` before `cp` meant the
  token copy never ran. Fixed.
