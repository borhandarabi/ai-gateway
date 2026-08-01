# Deploying ai-gateway on Railway

Railway-specific files:
- `railway.json` (repo root) — config-as-code: build + deploy settings
- `railway/Dockerfile` — same stack, but sources are pulled via `git clone`
  instead of Buildx `--build-context`, since Railway's Dockerfile builder
  doesn't expose that flag
- `mihomo/config.no-tun.yaml` — used automatically (see below)

## 0. OmniRoute is a prebuilt image, not built from source

Unlike the other services, OmniRoute comes from the official published
image (`diegosouzapw/omniroute:latest-web`, confirmed multi-arch:
amd64 + arm64) used as this Dockerfile's base, instead of `git clone` +
`npm run build`. This removes OmniRoute's memory-heavy Next.js/Turbopack
build from the pipeline entirely -- no `OMNIROUTE_REPO_URL`/`OMNIROUTE_REF`
Variables needed for it, just `OMNIROUTE_IMAGE` if you want to pin a
specific version tag instead of the floating `latest-web`.

## 1. TUN does not work on Railway — read this first

Railway does not grant containers `CAP_NET_ADMIN`/`CAP_NET_RAW` or
`/dev/net/tun` access, and does not support privileged containers (confirmed
by Railway staff on their support forum). **mihomo's TUN mode cannot run
here.** `railway/Dockerfile` sets `DISABLE_TUN=true` by default, which makes
`s6-rc.d/mihomo/run` seed `mihomo/config.no-tun.yaml` instead of the TUN
config — you get SOCKS/HTTP mixed-port mode only (port `7890`), not
system-wide transparent routing. If TUN is a hard requirement, Railway isn't
a viable target for that part of the stack; use a plain VM/VPS instead (the
root `Dockerfile` + `docker-compose.yml` already does this).

## 2. Required Variables (build-time ARGs)

Railway auto-fills any Dockerfile `ARG` whose name matches a Variable you set
on the service — there's no separate "build args" field in `railway.json`.
Set these under the service's **Variables** tab:

| Variable | Purpose | Default in Dockerfile if unset |
|---|---|---|
| `OMNIROUTE_IMAGE` | prebuilt OmniRoute image (not built from source -- see below) | `diegosouzapw/omniroute:3.8.49-web` |
| `MIMO_REPO_URL` | MimoApi git URL | `https://github.com/hooshidev3/mimo-ai-proxy.git` |
| `MIMO_REF` | branch/tag | `main` |
| `METACUBEXD_REPO_URL` | metacubexd git URL | `https://github.com/MetaCubeX/metacubexd.git` |
| `METACUBEXD_REF` | branch/tag | `main` |
| `GROK2API_REPO_URL` | grok2api-go git URL | `https://github.com/i-panel/grok2api-go.git` |
| `GROK2API_REF` | branch/tag | `main` |
| `GLM_REPO_URL` | zai-api git URL | **blank — build fails until set** (repo not pushed yet) |
| `GLM_REF` | branch/tag | `main` |
| `MIHOMO_VERSION` | mihomo release tag | `v1.19.27` |

## 3. Runtime Variables

Same set as `.env.example` at the repo root (`TUNNEL_TOKEN`, `TUNNEL_ONLY`,
`AUTH_TOKEN`, `ZAI_TOKEN`, etc.) — set them the same way, just as Railway
service Variables instead of a `.env` file.

## 4. Volume

Railway allows **exactly one volume per service**. That's fine here because
every service's data dir is already namespaced under `/data/*`
(`/data/omniroute`, `/data/mimo`, `/data/glm`, `/data/metacubexd`,
`/data/mihomo`) — mount the single Railway volume at **`/data`** and all five
persist together. Attach it via the dashboard (or CLI / the newer
`railway.ts` IaC, which is beta and can't be mixed with `railway.json` on the
same service).

## 5. Domains / public access

- `EXPOSE`d ports (`20128, 3000, 3001, 8080, 9090, 7890`) show up as
  selectable **Target Ports** when you generate a domain in the dashboard —
  this isn't controlled from `railway.json`. Pick `20128` (OmniRoute) for the
  main public domain; `healthcheckPath` in `railway.json` assumes that's the
  target port.
- Only add domains for ports you actually want end users hitting directly
  (e.g. `8080` for the metacubexd dashboard, if you want it public). Ports
  that are only consumed internally by other services in this same
  container (`3000`, `3001`, `9090`) don't need a domain — they're already
  reachable over localhost since everything runs in one container regardless
  of platform.
- `7890` (mihomo mixed-port, SOCKS/HTTP) isn't an HTTP page — if you need it
  reachable from outside the container, use Railway's **TCP Proxy** feature
  instead of a domain (Settings → Networking → TCP Proxy, port `7890`).
- If `TUNNEL_TOKEN` + `TUNNEL_ONLY=true` are set, none of this matters for
  ingress — the Cloudflare Tunnel becomes the only entry point regardless of
  what Railway's own networking is configured to do. This is arguably the
  simplest option on Railway, since it sidesteps Target Ports/TCP Proxy
  configuration entirely.
