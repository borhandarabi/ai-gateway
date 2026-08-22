# OmniRoute (standalone service)

This directory deploys [OmniRoute](https://github.com/diegosouzapw/OmniRoute)
as its **own** image/container, deliberately separate from the main
`ai-gateway` image at the repo root. Docker/Railway resource limits apply
per-container, and OmniRoute must not compete with `ai-gateway`'s own
services (several of which launch Chromium) for the same CPU/memory budget.

`Dockerfile` here is a thin pass-through pinned to a specific upstream tag
(`diegosouzapw/omniroute:<version>`) -- OmniRoute is not rebuilt from source.

## docker-compose (same stack as ai-gateway)

Already wired up in the repo-root `docker-compose.yml` as a separate
`omniroute` service on the same compose network as `ai-gateway`. Just set
the required secrets in `.env` (see `.env.example`) and run:

```bash
docker compose up -d ai-gateway omniroute
```

From inside the `omniroute` container, the six ai-gateway services are
reachable at `http://ai-gateway:<port>/v1` -- see the root `README.md`
"OmniRoute" section for the exact base URL / auth / OpenAI-vs-Anthropic
add-path for each one.

## Railway

Two ways to deploy OmniRoute on Railway:

1. **One-click, unmanaged by this repo** -- use the maintainer's own
   Railway template: <https://railway.com/deploy/omniroute--omni-route>.
   Fastest path, but not tied to this repo's version pin or your other
   Railway services' private networking by default.
2. **From this repo** (what `Dockerfile` + `railway.json` in this directory
   are for) -- in the Railway dashboard, add a new service from this GitHub
   repo, then set its **Root Directory** to `omniroute`. Railway will pick up
   `railway.json` automatically (Dockerfile builder, `/api/monitoring/health`
   healthcheck, restart-on-failure).

Either way, set these variables on the OmniRoute service:

| Variable | Notes |
| --- | --- |
| `JWT_SECRET` | **Fixed, random value.** Changing it after first deploy invalidates every session/API key. |
| `API_KEY_SECRET` | **Fixed, random value.** Same as above -- generated keys stop working if this changes. |
| `INITIAL_PASSWORD` | First-login dashboard password. Change it under Configuration -> Security after logging in once. |
| `REQUIRE_API_KEY` | `true` recommended for anything internet-reachable. |
| `RAILWAY_RUN_UID` | Set to `0` **on Railway specifically**. Without it, the container can't write to the mounted volume, OmniRoute silently falls back to an in-memory DB while still returning HTTP 200, and all provider config is lost on the next restart. Not needed for the docker-compose path. |

Attach a volume at `/app/data` (persists `storage.sqlite` -- providers, keys,
usage, everything). If you deploy from this repo's Root Directory, add the
volume the same way you would for any other Railway service.

To reach the six ai-gateway services from OmniRoute on Railway, deploy
`ai-gateway` as a Railway service too and use its private networking
hostname (e.g. `http://ai-gateway.railway.internal:<port>/v1`) as the base
URL when adding each provider from the dashboard, instead of the
docker-compose `http://ai-gateway:<port>/v1` form.
