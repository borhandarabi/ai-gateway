# AGENTS.md — ai-gateway knowledge base for AI agents

> هدف این فایل: هر ایجنت/سشنی بعدی بدون کلون مجدد مخازن آپ‌استریم، دانش لازم را داشته باشد.
> Purpose: persistent knowledge so future agent sessions never need to re-clone upstream
> repos or rediscover the architecture. Last verified from upstream sources: **2026-08-24**.
> If a build pulls newer upstream code, spot-check before trusting a specific default.

## 1. What this repo is

One Docker image ("ai-gateway") bundling ~10 services under **s6-overlay v3**, managed by a
Go program that IS `main.go` (built as `singbox-manager`). The same binary serves:

- the management dashboard (embedded HTML/JS/CSS strings inside main.go)
- reverse proxy `/` → per-service routes (`/<name>/...`)
- sing-box lifecycle (template.json + nodes.json + subscriptions → config.json)
- a REST API under `/api/*` (routes registered near the bottom of main.go, ~line 12200+;
  all guarded by `requireAuth` keyed on `CLASH_SECRET`, header `X-Admin-Token`)

Deploy targets: local docker-compose AND **Railway** (limit: 1 GB RAM / 2 vCPU per image,
volume mounted at `/data`). OmniRoute runs as a SEPARATE container/image.

## 2. Architecture map (symbol → where in main.go; line numbers drift, search by symbol)

| Symbol | What |
|---|---|
| `ServiceDef` (~4112) | `{Name, ListenPort, ProxyPort, Env}` — one row of "Active services"; persisted in `state.json` → `.services[]` |
| `serviceDefaultEnvs` (~4117) | map[service]map[key]default — upstream-verified env seeds (see §4) |
| `applyServiceDefaultEnvs` | fills ONLY missing keys; never overwrites user values |
| `readStateOrDefault` (~4236) | loads state.json + legacy migrations + omni/zenfree ensure + env seeding |
| `bootstrapFreshInstall` (~4751) | fresh install: WARP accounts, default services (+seeded envs), template/nodes |
| `readSystemMetrics` (~6610) | cgroup-aware RAM/CPU; falls back to host /proc |
| `readCgroupMemory` / `readCgroupCPUPercent` | cgroup v2 → v1 → fallback; CPU diffs usage_usec between polls |
| `getConfigsHandler` (~6560) | GET /api/get_configs → `{template, nodes, services}` (dashboard table source) |
| `updateServiceEnvHandler` (~7630) | POST /api/update_service_env — writes env into run script + state, restarts svc |
| `applyServiceEnvToRun` (~10906) | renders `# BEGIN/END AI-GATEWAY SERVICE ENV` export block into BOTH source (`$S6_SOURCE_DIR`) and live (`$S6_SERVICE_DIR`) copies of the run script |
| s6 layer (~10520+) | `listS6Services`, `controlS6ServiceHandler` (start/stop/restart via `/command/s6-svc -u/-d/-r`), SSE live logs; `s6WriteBootMarker` persists toggles to `/data/s6-state/<name>.up\|.down` |
| `omniRouteSuggestedKeyEnv` (~7158) | which env holds each service's client key for "Add to OmniRoute" |

Files outside main.go:
- `s6-rc.d/<svc>/run` — longrun start scripts (bash, `#!/command/with-contenv`)
- `s6-rc.d/gateway-entrypoint.sh` — ENTRYPOINT before `/init`; seeds initial up/down state
- `healthcheck.sh` — aggregate check; SKIPS services whose `$S6_SERVICE_DIR/<name>/down` exists
- `Dockerfile` — multi-stage; every app stage pulls LIVE upstream source via named build-context
- `build.sh` — wires the build-context repo URLs (source of truth for upstream refs)
- `railway.template.json` — Railway deploy template incl. variable catalog (ENABLE_* etc.)
- `main_seed_test.go` — tests for env seeding (idempotency, coverage)

Longruns: `singbox`(core, always up) zenfreeapi mimo zai kimi deepseek grok2api qwen2api flaresolverr cloudflared.
Oneshots: network-mode-init, singbox-ready (+*-log companions, *-pipeline).

## 3. Boot policy & service control (v3, 2026-08-24)

- Default: everything boots DOWN except `singbox` (never manageable) and `cloudflared`
  (only when `TUNNEL_TOKEN` set). `ENABLE_<NAME>=true|false` env forces a service's initial
  state and persists it via markers in `/data/s6-state`. Dashboard Start/Stop writes the same
  markers (`s6WriteBootMarker`).
- **CRITICAL s6 fact (cost us a bug):** writing an empty `down` file into a service
  DEFINITION dir does NOT work — `s6-rc-compile` deliberately ignores/does-not-replicate
  ./down files from definition dirs (skarnet docs). The only supported runtime mechanism is
  `/command/s6-svc -d /run/service/<name>` after the bundle is up. Implementation:
  `gateway-entrypoint.sh` computes the boot-stop list -> `$S6_BOOTDOWN_FILE`
  (**default /tmp/ai-gateway-bootstop — NEVER under /run/s6, preinit `s6-rmrf`s that tree**)
  and the `service-bootdown` oneshot (depends on all managed longruns + loggers + pipelines,
  registered in user/contents.d) runs `s6-svc -d` against each entry once everything is up.
  The oneshot also stops matching `*-log` services so pipelines don't linger half-up.
- **NEVER use `exec sleep infinity` idle stubs** — they hide real status from
  `s6-svstat`/dashboard. Status truth = `/command/s6-svstat -o up,wantedup,pid,updownfor`.
  healthcheck.sh asks s6-svstat directly; do not reintroduce marker-file checks there.
- UI button gating (main.go renderS6Services/actionBtn): Start enabled ONLY when status is
  stopped/unhealthy-wanted-down; Stop/Restart only when up or unhealthy. `wantedup=false`
  means "deliberately down".
- `ZAI_AUTO_COLLECT=true` gates zai's first-boot token collection (`./zai-api collect ...`)
  which launches REAL headless Chromium via Playwright (~400MB+) -- off by default because of
  the 1GB budget. Without tokens.sqlite the server still boots; requests fail until tokens exist.

## 4. Upstream sources & complete env inventory (verified 2026-08-24)

Legend: ⭐ = seeded into `serviceDefaultEnvs` (editable from dashboard Active services → Env).
"not seeded" = real upstream key we deliberately don't manage (derived/internal/rare).
Mapping column shows upstream-name ← bundle-name when they differ.

### 4.1 zenfreeapi (Rust)
Repo: https://github.com/izaart95-jpg/ZenFreeAPI.git#main (reads `env::var` in src/*.rs)
| Upstream key | Default | Notes |
|---|---|---|
| ⭐ OPENCODE_ZEN_BASE | https://opencode.ai/zen/v1 | |
| ⭐ OPENCODE_API_KEY | public | |
| ⭐ OPENCODE_CLIENT | cli | |
| OPENCODE_USER_AGENT | opencode/0.0.0 | not seeded |
| OPENCODE_PROJECT_ID / OPENCODE_SESSION_ID | — | per-request context |
| ZEN_LISTEN / ZEN_STATE_DIR | — | run script derives (ZENFREEAPI_PORT / /data/zenfreeapi) |

### 4.2 mimo (Go)
Repo: https://github.com/hooshidev3/mimo-ai-proxy.git#main (os.Getenv)
| Upstream key | Default | Notes |
|---|---|---|
| ⭐ CORS_ORIGIN (bundle MIMO_CORS_ORIGIN) | *(unset)* | main.go:70 |
| ⭐ API_KEY (bundle MIMO_API_KEY) | "" | client bearer; enforced ONLY when non-empty (internal/middleware/auth.go) |
| ⭐ SERVICE_TOKENS (fallback SERVICE_TOKEN) | "" | csv |
| ⭐ USER_IDS (fallback USER_ID) | "" | csv |
| ⭐ XIAOMI_CHATBOT_PHS (fallback …PH) | "" | csv |
| PORT | 3000 | derived from MIMO_PORT by run script |
| DB_PATH | — | bundle leaves default (data dir /data/mimo) |
| AGENT_MODEL | — | not seeded |

### 4.3 zai / GLM-Free-API (Go)
Repo: https://github.com/borhandarabi/GLM-Free-API.git#main (loadConfig in main.go ~line 95)
| Upstream key | Default | Notes |
|---|---|---|
| ⭐ AUTH_TOKEN (bundle ZAI_AUTH_TOKEN) | Waguri | client auth |
| ⭐ TIMEOUT (bundle ZAI_TIMEOUT) | 300000 | ms |
| ⭐ AGENT_MODE (bundle ZAI_AGENT_MODE) | false upstream / **true** in image | |
| ⭐ LOG_LEVEL (bundle ZAI_LOG_LEVEL) | debug upstream / **info** in image | |
| ⭐ LOG_FORMAT (bundle ZAI_LOG_FORMAT) | text | |
| ⭐ ZAI_AUTO_COLLECT | false | BUNDLE-only gate, not upstream |
| PORT / HOST | — | derived |
| ZAI_TOKEN | "" | upstream server token slot, not seeded |
| PROXY_SERVER_URL / PROXY_COLLECTOR_URL / PROXY | — | run script derives from ZAI_PROXY_PORT(2001)/ZAI_COLLECTOR_PROXY_PORT(2007) |
Important: `playwright.go` here is a STUB (no browser at server runtime). Only the `collect`
subcommand (`captcha.go`, builds as token-collector, flags --tokens --batch --parallel
--no-tui --headed) drives real Chromium. README slogan: "Pure HTTP".

### 4.4 kimi (Go)
Repo: https://github.com/izaart95-jpg/KimiFreeAPI.git#main (envOrDefault in main.go:47)
| Upstream key | Default | Notes |
|---|---|---|
| ⭐ KIMI_ACCESS_TOKEN | "" | empty ⇒ nothing to serve; boot policy keeps it down anyway |
| ⭐ AUTH_KEY (bundle KIMI_AUTH_KEY) | Waguri | "Bearer "+value |
| ⭐ KIMI_DEBUG | "" | any non-empty enables debug |
| PORT | 3000 | derived from KIMI_PORT |

### 4.5 deepseek (Go)
Repo: https://github.com/izaart95-jpg/DeepSeekFreeAPI.git#main (envOr in main.go:25)
| Upstream key | Default | Notes |
|---|---|---|
| ⭐ PROXY_API_KEY | **Waguri-san** | client key of the proxy |
| ⭐ DEEPSEEK_TOKEN | "" | required upstream DS token |
| PORT | 3000 | derived from DEEPSEEK_PORT; runs `deepseek-proxy proxy` |

### 4.6 grok2api (Go backend + React dist)
Repo (build context): https://github.com/chenyme/grok2api.git#main
Config is a YAML HEREDOC generated once by `s6-rc.d/grok2api/run` into
`/data/grok2api/config.yaml`. Env is consumed ONLY at first generation:
| Key | Default | Notes |
|---|---|---|
| ⭐ GROK2API_KEY | random(base64) | → secrets.credentialEncryptionKey |
| ⭐ GROK2API_SECRET | random(base64) | → secrets.jwtSecret (admin session JWT ONLY — NOT a client API key; client keys are made in grok2api's own admin UI, default admin/admin123456) |
To change KEY/SECRET afterwards you must delete /data/grok2api/config.yaml (regenerates).
There is NO GROK2API_SSO env anywhere upstream — don't invent one (was wrongly added once, removed).

### 4.7 qwen2api (Go)
Repo: https://github.com/XxxXTeam/Qwen2API_Go.git#main (internal/config/config.go Load())
| Upstream key | Default | Notes |
|---|---|---|
| ⭐ API_KEY (bundle QWEN2API_KEY) | Waguri | csv; FIRST entry becomes AdminKey |
| ⭐ DATA_SAVE_MODE | none | |
| ⭐ SIMPLE_MODEL_MAP | false | |
| ⭐ OUTPUT_THINK | false | |
| ⭐ AUTO_REFRESH | true | |
| ⭐ AUTO_REFRESH_INTERVAL | 21600 (6h, seconds) | |
| ⭐ CACHE_MODE | default | |
| ⭐ LOG_LEVEL | INFO | plain upstream name (NOT "QWEN2API_LOG_LEVEL") |
| ⭐ BROWSER_HEADLESS | true | qwen2api itself can drive a browser |
| ⭐ BROWSER_TIMEOUT_SECONDS | 45 | |
| BATCH_LOGIN_CONCURRENCY | 5 | not seeded |
| SEARCH_INFO_MODE / DEBUG_MODE / ENABLE_FILE_LOG / LOG_DIR / MAX_LOG_FILE_SIZE / MAX_LOG_FILES | various | not seeded |
| QWEN_CHAT_PROXY_URL | https://chat.qwen.ai | not seeded |
| LISTEN_ADDRESS / SERVICE_PORT | 0.0.0.0 / 3000 | derived (HOST←BIND_ADDR, PORT←QWEN2API_PORT) |
| PROXY_URL | — | run script: socks5h://127.0.0.1:${QWEN2API_PROXY_PORT:-2006} |
| BROWSER_AUTH_ENABLED(true)/BROWSER_EXECUTABLE_PATH/CHAT_CLEANUP_MODE(0)/PROMPT_OVERRIDES_JSON/REDIS_URL | — | not seeded |

### 4.8 flaresolverr-go
Repo: https://github.com/Rorqualx/flaresolverr-go.git#main (internal/config/config.go getEnv*)
Its run script reads FLARESOLVERR_*-prefixed container env and exports the BARE names;
starts Xvfb :99 when HEADLESS=true (soffware GL: LIBGL_ALWAYS_SOFTWARE=1, LP_NUM_THREADS=4);
drops to user flaresolverr; needs /usr/bin/chromium.
| Managed (prefixed) key | Seeded default | Upstream default if different |
|---|---|---|
| ⭐ FLARESOLVERR_LOG_LEVEL | info | info |
| ⭐ FLARESOLVERR_LOG_HTML | false | false |
| ⭐ FLARESOLVERR_HEADLESS | true | true |
| ⭐ FLARESOLVERR_BROWSER_POOL_SIZE | **1** | 3 |
| ⭐ FLARESOLVERR_BROWSER_POOL_TIMEOUT | 30s | 30s |
| ⭐ FLARESOLVERR_MAX_MEMORY_MB | **1024** | 2048 |
| ⭐ FLARESOLVERR_SESSION_TTL / _CLEANUP_INTERVAL | 30m / 1m | same |
| ⭐ FLARESOLVERR_MAX_SESSIONS | 100 | 100 |
| ⭐ FLARESOLVERR_DEFAULT_TIMEOUT / _MAX_TIMEOUT | 60s / 300s | same |
| ⭐ FLARESOLVERR_RATE_LIMIT_ENABLED / _RPM | true / 60 | same |
Not seeded (upstream): CLEARANCE_CACHE_ENABLED=true, CLEARANCE_TTL=25m, PROXY_URL/_USERNAME/_PASSWORD, PROXY_LIST, PROXY_STRATEGY=sticky-domain, TZ, LANG, TEST_URL, DISABLE_MEDIA, PPROF_*, TRUST_PROXY=false, IGNORE_CERT_ERRORS=false, CORS_ALLOWED_ORIGINS, ALLOW_LOCAL_PROXIES=false, DNS_REBINDING_PROTECTION=true, API_KEY_ENABLED=false, API_KEY, CAPTCHA_NATIVE_ATTEMPTS=3, CAPTCHA_FALLBACK_ENABLED, TWOCAPTCHA_API_KEY/CAPSOLVER_API_KEY/ANTICAPTCHA_API_KEY/NINEKW_API_KEY, CAPTCHA_PRIMARY_PROVIDER=2captcha, CAPTCHA_SOLVER_TIMEOUT=120s, SELECTORS_* .
NOTE: "FLARESOLVERR_CAPTCHA_SOLVER" belongs to the OLD Python port — the Go port does NOT read it.

## 5. Pitfalls & rules (learned the hard way)

1. **Injected env beats run-script defaults** because scripts use `${VAR:-default}` — but the
   injected block is placed BEFORE `exec 2>&1` and must keep using the same variable name the
   script references. Never seed `PORT`, `HOST`, `HTTP(S)_PROXY`, `ALL_PROXY`, `NO_PROXY`,
   `PROXY_URL`, `PROXY_SERVER_URL`… — those are DERIVED by run scripts from `<SVC>_PORT`,
   `BIND_ADDR`, `<SVC>_PROXY_PORT`.
2. **Metrics**: inside a container `/proc/meminfo` & `/proc/stat` describe the HOST. Railway
   showed ~70% while the container was OOMing at its 1GB cap. Always read cgroup first
   (v2 `/sys/fs/cgroup/memory.{max,current}` + `memory.stat:inactive_file`, `cpu.stat
   usage_usec` + quota from `cpu.max`; v1 fallback `memory/memory.limit_in_bytes`,
   `cpuacct/cpuacct.usage` for CPU — v1 has NO cpu.stat). Sentinel gotchas:
   v1 no-limit memory = **9223372036854771712** (~LLONG_MAX page-rounded), NOT MaxUint64;
   compare against `1<<62`. `memory.current`/`usage_in_bytes` include reclaimable page
   cache — subtract inactive_file like `docker stats` does or numbers look inflated.
   Normalize CPU% by the effective quota (quota/period) so 100% == whole allowance.
3. **s6**: status truth = `s6-svstat -o up,wantedup,pid,updownfor /run/service/<name>`.
   Stop = `s6-svc -d` (runtime only). Boot-time down CANNOT come from a `down` file in the
   source definition dir (s6-rc-compile ignores it) — use the service-bootdown oneshot
   mechanism (see §3). Persistence markers live in `/data/s6-state` (volume ⇒ survives
   redeploy). `gateway-entrypoint.sh` precedence: ENABLE_* env > marker > default policy.
4. **healthcheck** must ask `s6-svstat` (wantedup=false ⇒ deliberately stopped) and SKIP
   those services, or a deliberately-stopped service marks the whole container unhealthy.
5. **grok2api config.yaml is generate-once** — env changes need the file deleted.
6. **Railway template**: add new operator-facing vars to `railway.template.json` variables AND
   `.env.example`; Railway caps each image at 1GB/2vCPU — Chromium services (flaresolverr,
   zai-collect, qwen2api browser mode) are the memory bombs; keep them opt-in.
7. **Windows dev host quirks**: terminal is git-bash (MSYS); native tools need `C:/...` paths;
   `patch` tool can mismatch CRLF — for Dockerfile-style edits a small python byte-replace or
   full `write_file` is safer. Blocked inline commands land in
   `%LOCALAPPDATA%/hermes/cache/blocked-scripts/`.
8. Verify with: `go build ./...`-equivalent (`go build -o <tmp> main.go`), `go vet .`,
   `go test .` (seed tests), `bash -n` on shell scripts. Full image: `./build.sh`.
9. Upstream drift: Dockerfile/build.sh clone `#main` AT BUILD TIME. This doc reflects the
   sources as of 2026-08-24; if a service behaves differently than documented, re-check its
   repo (list in §4 headers) before changing the bundle.

## 6. Repo URLs (for one-line re-clone if ever needed)

```bash
cd "$LOCALAPPDATA/Temp/upstream"
for r in hooshidev3/mimo-ai-proxy borhandarabi/GLM-Free-API izaart95-jpg/KimiFreeAPI \
         izaart95-jpg/DeepSeekFreeAPI XxxXTeam/Qwen2API_Go Rorqualx/flaresolverr-go \
         izaart95-jpg/ZenFreeAPI; do git clone -q --depth 1 "https://github.com/$r.git"; done
# env extraction shortcut (Go services):
grep -rhoE 'os\.(Getenv|LookupEnv)\("[A-Z0-9_]+"\)' <dir> --include='*.go' | sort -u
```
