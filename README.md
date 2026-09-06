# ai-gateway

ZenFreeAPI is included as a Rust OpenAI-compatible OpenCode Zen proxy on port
`8080`, with sing-box proxy inbound `2008`. It is built from
`https://github.com/izaart95-jpg/ZenFreeAPI.git` and supervised by s6.

Single Docker image running, as separate s6-supervised processes sharing one
network namespace:

- **MimoApi** (Go) — Mimo/Xiaomi provider proxy, port `3000`
- **zai-api / GlmApi** (Go) — Z.ai/GLM provider proxy, port `3001`
- **kimi-api** (Go) — kimi.com proxy, port `3002`
- **grok2api-go** (Go + Node/Vite) — Grok (xAI) provider proxy + dashboard, port `8000`
- **sing-box** (Go) — proxy kernel: TUN + SOCKS/HTTP mixed-port (`7890`) +
  Clash API (`9090`)
- **cloudflared** (Go, prebuilt) — optional Cloudflare Tunnel, always
  installed, idle unless `TUNNEL_TOKEN` is set


## Why one image, and the trade-offs

This was a deliberate choice (plain-Docker deployment target, not
Kubernetes/Compose-as-orchestration) over the more conventional
multi-container approach. The cost of that choice — and how each piece of
this repo addresses it — is documented inline in the Dockerfile and each
`s6-rc.d/*/run` script; the short version:

- All four apps share one network namespace, so sing-box's TUN mode captures
  **all** outbound traffic in the container unless explicitly excluded
  (see the sing-box configuration's `PROCESS-NAME,cloudflared,DIRECT` rule).
- Only one `HEALTHCHECK` is possible per image — `healthcheck.sh` polls every
  service instead of relying on each app's own healthcheck.
- Only one PID 1 is possible — `s6-overlay` supervises all processes, with an
  explicit start order (see below) instead of Docker's own restart policy.

## Start order

```
cloudflared ─┐
             ├─▶ singbox ─▶ singbox-ready
network-mode-init ────────────────────────▶ mimo
                                           ▶ zai-api
                                           ▶ kimi-api
                                           ▶ grok2api
                                           ▶ metacubexd
```

- `cloudflared` starts first and is excluded from TUN routing (both by start
  order and by an explicit rule, since long-lived connections reconnect
  periodically and could otherwise get captured after TUN comes up).
- `singbox-ready` blocks until sing-box's control API responds, so the other
  services never race the TUN interface coming up.
- `network-mode-init` computes `BIND_ADDR` (`0.0.0.0` normally, `127.0.0.1`
  when `TUNNEL_ONLY=true` + `TUNNEL_TOKEN` is set) and every app service reads
  it at startup.

## s6-rc oneshot scripts (important!)

The `network-mode-init` and `singbox-ready` services are **oneshot** type in
s6-rc. Their `up` files must be valid **execline** scripts (not bash with
shebangs), because `s6-rc-compile` strips the shebang and parses the body as
execline. The actual bash logic lives in a separate `run.sh` file, and the
`up` file is a thin execline wrapper:

```
s6-rc.d/network-mode-init/up       → execline: with-contenv ./run.sh
s6-rc.d/network-mode-init/run.sh   → bash: the actual logic
s6-rc.d/singbox-ready/up            → execline: with-contenv ./run.sh
s6-rc.d/singbox-ready/run.sh        → bash: the actual logic
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

### Production with the prebuilt image

For production, use the Compose file that pulls the multi-architecture image
published by GitHub Actions. It does not build `ai-gateway` locally:

```bash
cp .env.example .env
# set production secrets in .env
docker compose -f docker-compose.production.yml up -d
```

The default image is `ghcr.io/borhandarabi/ai-gateway:latest`. Pin a release
tag or digest when reproducible rollbacks are required:

```bash
AI_GATEWAY_IMAGE=ghcr.io/borhandarabi/ai-gateway:<tag> \
  docker compose -f docker-compose.production.yml up -d
```

The GHCR package must be public for Railway's free/public-image flow. If it is
private, configure GHCR registry credentials in Railway (a Pro plan is
required for private registry images).

### Railway (prebuilt image, no Railway build)

Railway must create this as a **Docker Image** service, using
`ghcr.io/borhandarabi/ai-gateway:latest`; connecting this repository as a
GitHub service would find `Dockerfile` and build it. The two-service
image/variable/network/volume settings to enter in a Railway Template are documented in
[`railway.template.json`](railway.template.json). After publishing that
template, replace `ai-gateway` in the button URL below with the template's
actual Railway template code:

[![Deploy on Railway](https://railway.com/button.svg)](https://railway.com/new/template/ai-gateway)

The template contains both `ai-gateway` and `omniroute`. The gateway volume is
mounted at `/data` (including `/data/sing-box` and all other service state
directories), while OmniRoute's volume is mounted at `/app/data`. Set provider
tokens and other production values in the service variables.

CI equivalent: `.github/workflows/docker-build.yml` -- builds `linux/amd64`
and `linux/arm64` as **separate jobs** (native runners, no QEMU sharing a
runner with the other arch's build), then merges them into one multi-arch
manifest. This was originally needed to avoid a "cannot allocate memory"
failure caused by OmniRoute's Next.js/Turbopack build running inside this
pipeline. OmniRoute has since been removed from this image entirely (see
"Resolved issues" below), so that specific OOM cause no longer exists, but
the per-arch job split and swap-space step are kept as cheap insurance for
the remaining Go/Node builds.

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `TUNNEL_TOKEN` | (empty) | Cloudflare Tunnel token. Leave empty to disable tunnel. |
| `TUNNEL_ONLY` | `false` | When `true` + `TUNNEL_TOKEN` set, bind services to 127.0.0.1. |
| `OMNIROUTE_PROXY_PORT` | `20129` | sing-box mixed-proxy inbound ai-gateway reserves for OmniRoute's own outbound traffic (optional; unrelated to OmniRoute's own listen port -- see "OmniRoute" section below). |
| `ZENFREEAPI_PORT` | `3008` | ZenFreeAPI HTTP listen port (`ZEN_LISTEN` is derived from it). |
| `ZENFREEAPI_PROXY_PORT` | `2008` | sing-box mixed proxy inbound used by ZenFreeAPI. |
| `OPENCODE_ZEN_BASE` | `https://opencode.ai/zen/v1` | Zen upstream base URL. |
| `OPENCODE_API_KEY` | `public` | Zen API key; public permits free-tier models only. |
| `OPENCODE_CLIENT` | `cli` | Value sent in `x-opencode-client`. |
| `OPENCODE_USER_AGENT` | `opencode/0.0.0` | Upstream User-Agent. |
| `ZEN_STATE_DIR` | `/data/zenfreeapi` | Persistent Zen conversation state directory. |
| `MIMO_PORT` | `3003` | MimoApi port. |
| `KIMI_PORT` | `3002` | kimi-api port. |
| `KIMI_ACCESS_TOKEN` | (empty) | **Required** for kimi-api. Get from kimi.com. Falls back to `KIMI_AUTH_KEY`. |
| `KIMI_AUTH_KEY` | `Waguri` | Legacy fallback for Kimi token. |
| `ZAI_PORT` | `3001` | zai-api port. |
| `ZAI_AUTH_TOKEN` | `Waguri` | Auth token for zai-api. Change in production! |
| `ZAI_AGENT_MODE` | `true` | Enable agent mode for zai-api. |
| `GROK2API_PORT` | `3004` | Grok (xAI) proxy and web dashboard. |
| `PROXY_PORT` | `80` | General reverse proxy (serves `/mimo/`, `/zai/`, etc., and the UI on `/`). |
| `CLASH_API_PORT` | `9090` | sing-box control API port. |
| `MIXED_PORT` | `7890` | sing-box SOCKS/HTTP mixed port. |
| `FLARESOLVERR_PORT` | `8191`   | FlareSolverr sidecar port (host-mapped).                                   |
| `FLARESOLVERR_PROXY_PORT` | `8190`   | FlareSolverr sidecar port (host-mapped).                                   |
| `FLARESOLVERR_CAPTCHA_SOLVER` | `none` | FlareSolverr captcha-solver adapter name (see upstream docs).       |
| `QWEN2API_PORT` | `3006` | Qwen2API sidecar port (host-mapped). |

## OmniRoute (separate service)

[OmniRoute](https://github.com/diegosouzapw/OmniRoute) runs as its **own**
image/container -- see `docker-compose.yml`'s `omniroute` service and
`omniroute/Dockerfile` / `omniroute/railway.json`. It is deliberately kept
out of the `ai-gateway` image: Docker/Railway resource limits apply
per-container, and OmniRoute must not compete with `ai-gateway`'s own
services (several of which launch Chromium) for the same CPU/memory.

```bash
docker compose up -d ai-gateway omniroute
```

For Railway, see `omniroute/README.md` (two options: the maintainer's own
one-click template, or deploying this repo's `omniroute/` directory as its
own Railway service).

### Adding the seven ai-gateway services as providers

This is a **manual, per-service step from ai-gateway's own dashboard** (not
automatic, and not something you have to do from OmniRoute's dashboard
directly): open ai-gateway's **Active services** table, click **"Add to
OmniRoute"** next to a service, review the prefilled form, and save.
Editing and removing later reuse the same button on that row.

The button only appears for the seven services this image actually knows
are AI providers -- MimoApi, zai-api, kimi-api, DeepSeekApi, grok2api-go,
Qwen2API, ZenFreeAPI -- driven by the same `knownServices` catalog described
in "Adding a service from a preset" below (`is_ai_provider` in
`GET /api/known_services`), not a fixed list duplicated in the frontend.
Anything else in the table (a custom service you added yourself,
`flaresolverr`, `zai-collect`) shows a plain `—` in that column instead.

#### What actually happens when you click Save

OmniRoute models a custom provider as **two separate objects**, and the
dashboard button performs both calls in sequence against OmniRoute's own
management API (verified against `diegosouzapw/OmniRoute`'s source, not
guessed):

1. `POST /api/provider-nodes` -- creates the connection *type* itself:
   name, a routing prefix, the Base URL, and `type` (`openai-compatible` or
   `anthropic-compatible`). Returns a node id.
2. `POST /api/providers` -- creates the actual *connection* against that
   node id: display name + API key. This is what shows up as "the
   provider" in OmniRoute's own dashboard.

ai-gateway stores the two ids it gets back (not the API key itself) in its
own state file, keyed by service name, so a later **Edit** knows which
node/connection to `PUT`, and **Remove** knows which node to
`DELETE /api/provider-nodes/{id}` (which cascades and deletes its
connection too).

A node's `type` is **immutable** on OmniRoute's side once created -- there
is no field for it in OmniRoute's own update schema. So if you edit a
service and change its type, the dashboard doesn't try to `PUT` the
existing node; it deletes the old node/connection pair and creates a fresh
one instead. That means a new API key is issued downstream -- any client
that had hardcoded the old key needs updating.

Authentication for these calls is a **management-scoped API key**
(`Authorization: Bearer ...`), not a session cookie, since the call is
container-to-container and not from your browser. Create one from
OmniRoute's own dashboard: **API Keys -> Create -> enable the "manage"
scope**.

#### One-time setup (Settings, in ai-gateway's own dashboard)

| Variable | What it's for |
|---|---|
| `OMNIROUTE_BASE_URL` | Where ai-gateway sends the two calls above. Defaults to `http://omniroute:20128`, matching the compose service name. |
| `OMNIROUTE_MANAGEMENT_API_KEY` | The management-scoped key described above. Required -- the button fails with a clear error until this is set. |
| `AI_GATEWAY_INTERNAL_BASE_URL` | Where **OmniRoute** should reach **this** container to call a service back. Used only to prefill the form's Base URL field (`<this>:<service port>/v1`) -- editable per-service before saving, never hardcoded in code. Defaults to `http://ai-gateway`, matching the compose service name in `docker-compose.yml`; change it if your deploy topology differs (see Railway note below). |

These three all live in the exact same Settings mechanism as every other
env var in this project (`managedEnvKeys`/`EnvOverrides`) -- nothing
provider-specific about how they're stored.

#### Where the API key value comes from

The form's API key field is **prefilled automatically for five of the
seven services**, read live from that service's own already-configured auth
setting (the same value visible on the Settings page) -- pulled from the
exact env var each service's own s6 run script reads as its **client-facing**
key, not whatever upstream credential it also happens to need:

| Service | Prefilled from | Notes |
|---|---|---|
| zai-api / GlmApi | `ZAI_AUTH_TOKEN` | |
| kimi-api | `KIMI_AUTH_KEY` | **Not** `KIMI_ACCESS_TOKEN` -- that one is kimi.com's own upstream session token the service needs to even start, unrelated to what a client presents to kimi-api's own `/v1/chat/completions`. |
| DeepSeekApi | `PROXY_API_KEY` | **Not** `DEEPSEEK_TOKEN` -- same distinction as kimi above (upstream vs. client-facing). Its effective default is `Waguri-san`, not empty -- DeepSeekFreeAPI's own `envOr("PROXY_API_KEY", "Waguri-san")` falls back to that placeholder for *any* empty value, and the run script always sets it (even to `""`) so the fallback always fires unless you set a real value. |
| Qwen2API | `QWEN2API_KEY` | |
| MimoApi | `MIMO_API_KEY` | mimo-ai-proxy's own `internal/middleware/auth.go` reads `API_KEY` from the environment and only enforces it when non-empty (genuinely open if left blank, unlike DeepSeek above -- confirmed from source, not assumed). `s6-rc.d/mimo/run` now passes this through -- it didn't before, so this service was always open regardless of upstream support until this was wired up. |
| ZenFreeAPI | `OPENCODE_API_KEY` | Not a local access gate -- ZenFreeAPI forwards whatever `Authorization` header a client sends straight upstream to OpenCode Zen, falling back to this value if the client sends none. Suggesting it here just means OmniRoute presents the same key ZenFreeAPI would use anyway. |
| grok2api-go | *(nothing)* | **Not** `GROK2API_SECRET` (`jwtSecret` in its config) -- that key only signs admin-dashboard session JWTs (`adminauth.NewService` in its `application.go`), a completely separate system from the client key that actually guards `/v1/chat/completions` (`ClientAuth` -> `clientkeyapp.Service.Authenticate` in its `middleware/auth.go`). That client key can only be minted from grok2api's own admin panel (`admin`/`admin123456` default login) today, not from a static env var -- generate one there and paste it in manually. |

If a value is prefilled, you can still overwrite it before saving -- it's a
suggestion, not a lock. If nothing is prefilled, type the key in by hand.

#### Which type to add each service as

The form also asks for a **type** -- OpenAI-compatible or
Anthropic-compatible (plus a stricter "Claude Code compatible" variant
meant for reverse proxies that mimic the actual Claude Code client, not
relevant here). This only controls which wire format OmniRoute uses when
*calling* the service's Base URL -- it does **not** limit how OmniRoute
exposes that provider to its own clients afterward: any registered
provider, regardless of which type it was added as, is served back out
through both `/v1/chat/completions` (OpenAI) and `/v1/messages`
(Anthropic), since OmniRoute translates bidirectionally at the gateway
level. So the only thing that matters when picking a type is which format
the service you're adding actually speaks on its own listen port:

| Service | Native OpenAI `/v1/chat/completions` | Native Anthropic `/v1/messages` | Add as |
|---|---|---|---|
| MimoApi | ✅ | ❌ | OpenAI-compatible |
| zai-api / GlmApi | ✅ | ✅ (native, added upstream) | OpenAI-compatible |
| kimi-api | ✅ | ❌ | OpenAI-compatible |
| DeepSeekApi | ✅ | ❌ | OpenAI-compatible |
| grok2api-go | ✅ | ✅ (native) | OpenAI-compatible |
| Qwen2API | ✅ | ✅ (native) | OpenAI-compatible |
| ZenFreeAPI | ✅ | ❌ | OpenAI-compatible |

zai-api, grok2api-go, and Qwen2API do serve a native `/v1/messages` endpoint
of their own (verified in their upstream source, not assumed), so those
three *can* be added via the Anthropic-compatible path instead if you
specifically want to exercise their native implementation -- functionally
it makes no difference to OmniRoute's own clients either way, so
**OpenAI-compatible is the recommended, more-tested path for all seven**,
kept uniform on purpose (and it's what the form defaults to).

On Railway, set `AI_GATEWAY_INTERNAL_BASE_URL` to
`http://ai-gateway.railway.internal` if `ai-gateway` is deployed as a
separate Railway service in the same project -- the form's Base URL prefill
picks that up automatically instead of the docker-compose default.

### Adding a service from a preset

"Add a service" (above the Active services table) has a **Preset** dropdown
sourced from `GET /api/known_services` -- the same catalog that drives the
"Add to OmniRoute" button visibility above and the built-in defaults every
fresh install starts with (see below). Picking one of the nine services
this image actually ships (mimo, zai, kimi, deepseek, grok2api, qwen2api,
zenfreeapi, flaresolverr, zai-collect) instead of leaving it on "Custom...":

- Locks the **Name** field to that service's real name (required for the
  next two points to work -- it has to match the actual `s6-rc.d/<name>`
  directory).
- Prefills **Listen port** / **Proxy port** from the service's own
  currently-configured values (its real env var, e.g. `MIMO_PORT`, not a
  guess).
- For the seven AI providers, shows an **OmniRoute type** selector
  (OpenAI-compatible / Anthropic-compatible) -- this is only a preference
  saved for later; it prefills the "Add to OmniRoute" form's type field
  once you get there, it does not call OmniRoute itself at this point.
- Renders that service's own **configuration fields**, sourced from the
  same `serviceDefaultEnvs` map that already auto-fills every default
  service's Env on every state read (see "Resolved issues" below) -- so
  this form and the per-row Env editor never show two different sets of
  defaults for the same service. Saving applies whatever you typed to the
  real service the same way the Env button does: rewrites
  `s6-rc.d/<name>/run`'s defaults and restarts that service.

"Custom..." keeps the previous free-form behavior for anything not in the
catalog (an arbitrary named routing target, matching `DEFAULT_SERVICES`'s
`name:port` format).

#### What a fresh install starts with

A brand-new install (no existing `template.json`/`nodes.json`) seeds
**Active services with all nine catalog entries automatically** -- the
seven AI providers plus `flaresolverr` and `zai-collect` -- using their real
current port env vars, not hardcoded numbers. `DEFAULT_SERVICES` (still
`name:port`, comma-separated) is *additional* to this: anything you list
there is layered on top of the nine built-in ones, for services outside
this image entirely (matching the original `telegram:2083`-style use case).

This replaced two bugs found while building the preset feature:
`bootstrapFreshInstall` previously read env vars like `MIMO_LISTEN_PORT`
that nothing in this image ever set (the real var is `MIMO_PORT`), so
overriding those ports never actually worked; and a separate migration path
in `readStateOrDefault` force-re-added an `omniroute` row into Active
services on every single request -- a leftover from before OmniRoute was
pulled out into its own image/container, which meant deleting that stale
row by hand never stuck. Both are gone now.



1. **zai-api repo URL** — not pushed yet. `ZAI_REPO` is blank/placeholder
   everywhere (`.env.example`, `docker-compose.yml`, the workflow's
   `workflow_dispatch` input / `ZAI_REPO_URL` secret, `build.sh`). Until it
   exists, point `ZAI_REPO` at a local checkout directory instead.
3. **sing-box control API secret** — the sing-box configuration's `secret:` field is
   empty; set a real one before exposing port `9090` anywhere non-trusted.
4. **kimi-api requires valid token** — kimi-api will fail to start if
   `KIMI_ACCESS_TOKEN` is not set to a valid kimi.com token. The placeholder
   `Waguri` does not work. Set this in your environment before deploying.

## Resolved issues

- **Stale `omniroute` row kept reappearing in Active services** —
  `readStateOrDefault` had a migration block that force-re-added an
  `omniroute` `ServiceDef` on every single request, left over from before
  OmniRoute was pulled out into its own container. Deleting that row from
  the dashboard never stuck. Removed entirely.
- **`bootstrapFreshInstall` read env vars nothing ever set** — its
  hardcoded default-service list used names like `MIMO_LISTEN_PORT`,
  `KIMI_LISTEN_PORT`, `DEEPSEEK_LISTEN_PORT`, `GROK2API_LISTEN_PORT` that no
  run script (or anything else) ever reads or sets -- the real vars are
  `MIMO_PORT`/`KIMI_PORT`/`DEEPSEEK_PORT`/`GROK2API_PORT`. Overriding those
  ports for a fresh install silently had no effect. Fixed by building the
  default list from the same `knownServices` catalog the "Add a service"
  presets and OmniRoute button now use, instead of a separate hand-written
  copy.
- **OmniRoute removed** — the final image used to be built `FROM` OmniRoute's
  own prebuilt Node/Next.js image, which made OmniRoute "free" to include but
  meant a full Node process ran alongside every other service at all times.
  On constrained hosts (e.g. 2 vCPU / 1 GB RAM) that left too little memory
  for zai-api's Playwright-launched Chromium, which crashed as a result.
  OmniRoute has been removed entirely and the final stage now builds
  `FROM debian:bookworm-slim` directly -- the leanest base still officially
  supported for Playwright's (glibc-linked) Chromium build.
- **System Services dashboard only showed one entry** — the s6 status API
  only listed whatever currently had a live `supervise/control` under
  `/run/service`, so any service s6-rc hadn't finished starting yet (which,
  under the memory pressure above, could be most of them) was silently
  omitted instead of shown as down. Fixed by enumerating the installed
  service list from `/etc/s6-overlay/s6-rc.d/user/contents.d` first, then
  overlaying live status where available.
- **s6-rc oneshot format** — `network-mode-init` and `singbox-ready` `up` files
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
