# syntax=docker/dockerfile:1.7
#
# ai-gateway — single-image bundle of:
#   - OmniRoute   (Node/Next.js)      : AI gateway / router UI + API
#                                        -- uses the official published image
#                                        (diegosouzapw/omniroute:latest-web) as
#                                        the base of the final stage instead of
#                                        building from source: it's multi-arch
#                                        (amd64+arm64 confirmed), and it removes
#                                        OmniRoute's memory-heavy Next.js/
#                                        Turbopack build from our pipeline
#                                        entirely (this was the repeated
#                                        "cannot allocate memory" failure).
#   - MimoApi     (Go)                : Mimo/Xiaomi provider proxy
#   - zai-api     (Go, aka "GlmApi")  : Z.ai/GLM provider proxy
#   - grok2api-go (Go + Node/Vite)    : Grok (xAI) provider proxy + dashboard
#   - sing-box (Go)                   : proxy kernel + Clash API
#   - cloudflared (Go, prebuilt)      : optional Cloudflare Tunnel
#
# Each application stage below pulls its source from a *named build context*
# pointing at the real upstream repo (except OmniRoute -- see above), so
# `docker build` always extracts the current entrypoint/env/build-steps from
# the source of truth instead of a locally vendored copy. Supply the
# contexts with `--build-context`, e.g.:
#
#   docker buildx build \
#     --build-context mimo_src=https://github.com/hooshidev3/mimo-ai-proxy.git#main \

#     --build-context grok2api_src=https://github.com/chenyme/grok2api.git#main \
#     --build-context glm_src=<GLM_REPO_URL>#<GLM_REF>   \
#     -t ai-gateway:latest .
#
# See build.sh / .github/workflows/docker-build.yml for the wired-up version.
#
# NOTE (open item): glm_src has no URL yet (repo not pushed). Until it exists,
# pass a local path instead, e.g. --build-context glm_src=./glm-local-checkout
# so the image still builds for local testing.

ARG OMNIROUTE_IMAGE=diegosouzapw/omniroute:3.8.49-web
# ─────────────────────────── MimoApi (Go) ───────────────────────────────────
FROM golang:1.26-alpine AS mimo-builder
WORKDIR /src
COPY --from=mimo_src . .
RUN go mod download 2>/dev/null || true
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mimoproxy main.go
# templates/ dir is required at runtime by the dashboard views

# ─────────────────────────── zai-api / GlmApi (Go) ──────────────────────────
FROM golang:1.26-alpine AS glm-builder
WORKDIR /src
COPY --from=glm_src . .
RUN apk add --no-cache git gcc musl-dev
RUN go mod init zai-api
# go.mod is pre-tidy as of writing; resolve real deps at build time.
RUN go mod tidy
# captcha.go is build-only (compiled for availability, never auto-run).
RUN go build -trimpath -gcflags="all=-l=4" -ldflags="-s -w" -o /out/token-collector captcha.go
# main.go is the actual service entry point.
RUN go build -trimpath -gcflags="all=-l=4" -ldflags="-s -w" -o /out/zai-api main.go

# ─────────────────────────── Kimi-api (Go) ──────────────────────────
FROM golang:1.26-alpine AS kimi-builder
WORKDIR /src
COPY --from=kimi_src . .
RUN apk add --no-cache git gcc musl-dev
RUN go mod init kimi-api
# go.mod is pre-tidy as of writing; resolve real deps at build time.
RUN go mod tidy
# main.go is the actual service entry point.
RUN go build -ldflags "-s -w" -trimpath -o /out/kimi-api main.go

# ─────────────────────────── DeepSeekProxy (Go) ──────────────────────────
FROM golang:1.26-alpine AS deepseek-builder
WORKDIR /src
# Source code will be provided via --build-context deepseek_src=...
COPY --from=deepseek_src . .
RUN apk add --no-cache git
# Fix invalid Go version in go.mod (1.26.5 doesn't exist, use 1.26)
RUN go mod edit -go=1.26
RUN go mod tidy
# Build the entire package (not just main.go) to include proxy.go and chat.go
RUN go build -o /out/deepseek-proxy .

# ───────────────────────── grok2api-go frontend (Vite) ──────────────────────
FROM node:22-alpine AS grok2api-frontend-builder
WORKDIR /src/frontend
RUN corepack enable
COPY --from=grok2api_src frontend/package.json frontend/pnpm-lock.yaml ./
RUN --mount=type=cache,id=grok2api-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && pnpm fetch --frozen-lockfile
COPY --from=grok2api_src frontend/ ./
RUN --mount=type=cache,id=grok2api-pnpm,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store \
    && pnpm install --offline --frozen-lockfile \
    && pnpm build
# -> /src/frontend/dist

# ───────────────────────── grok2api-go backend (Go) ─────────────────────────
FROM golang:1.26-alpine AS grok2api-backend-builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src/backend
RUN apk add --no-cache ca-certificates git
COPY --from=grok2api_src backend/go.mod backend/go.sum ./
RUN --mount=type=cache,id=grok2api-go-mod,target=/go/pkg/mod,sharing=locked \
    go mod download
COPY --from=grok2api_src backend/cmd ./cmd
COPY --from=grok2api_src backend/internal ./internal
COPY --from=grok2api_src backend/docs/docs.go ./docs/docs.go
RUN --mount=type=cache,id=grok2api-go-mod,target=/go/pkg/mod,sharing=locked \
    --mount=type=cache,id=grok2api-go-build,target=/root/.cache/go-build,sharing=locked \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/grok2api ./cmd/grok2api
# entrypoint extracted as-is from the repo: docker/entrypoint.sh (copied below)

# ───── su-exec (native build on the SAME base as the final image) ──────────
# grok2api-go's upstream image is Alpine and uses the Alpine `su-exec` package
# to drop from root to its app user inside its own entrypoint.sh. Our final
# image is Debian (node:26-trixie-slim) which has no such package, so we
# build the same tiny, well-known su-exec (ncopa/su-exec) natively here
# instead of trying to lift an Alpine/musl binary onto glibc.
FROM node:26-trixie-slim AS su-exec-builder
RUN apt-get update && apt-get install -y --no-install-recommends \
      git build-essential ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone --depth 1 https://github.com/ncopa/su-exec.git . && make

# ───────────────────────── SingBox-Manager binary ─────────────────────────────
FROM golang:1.26-alpine AS singbox-manager
WORKDIR /src
COPY . .
RUN apk add --no-cache git gcc musl-dev
RUN go mod init singbox-manager
# go.mod is pre-tidy as of writing; resolve real deps at build time.
RUN go mod tidy
# main.go is the actual service entry point.
RUN go build -ldflags "-s -w" -trimpath -o /out/singbox-manager main.go

# ───────────────────────────── cloudflared ──────────────────────────────────
FROM alpine:3.20 AS cloudflared-fetch
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates \
    && curl -fsSL "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${TARGETARCH}" \
       -o /usr/local/bin/cloudflared \
    && chmod +x /usr/local/bin/cloudflared

# ══════════════════════════ Final runtime image ═════════════════════════════
# Base = the official OmniRoute image itself (see ARG OMNIROUTE_IMAGE above).
# Its own last layers are `USER node` -- override back to root immediately so
# our own RUN/COPY steps (s6-overlay, apt packages, other services' users)
# work; s6's /init needs to run as root anyway (singbox's TUN needs root/caps,
# and each service that should run unprivileged -- grok2api, omniroute --
# drops to its own user itself via su-exec inside its own s6 run script,
# same pattern used throughout this image).
FROM ${OMNIROUTE_IMAGE} AS runtime
USER root

ARG S6_OVERLAY_VERSION=3.2.1.0
ARG TARGETARCH

RUN --mount=type=cache,id=apt-cache-rt,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,id=apt-lists-rt,target=/var/lib/apt/lists,sharing=locked \
    apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates iptables iproute2 curl bash xz-utils musl \
    && rm -rf /var/lib/apt/lists/*

ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz /tmp/s6-noarch.tar.xz
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) S6ARCH=x86_64 ;; \
      arm64) S6ARCH=aarch64 ;; \
      *) echo "unsupported arch ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-${S6ARCH}.tar.xz" -o /tmp/s6-arch.tar.xz; \
    tar -C / -Jxpf /tmp/s6-noarch.tar.xz; \
    tar -C / -Jxpf /tmp/s6-arch.tar.xz; \
    rm -f /tmp/s6-noarch.tar.xz /tmp/s6-arch.tar.xz

LABEL org.opencontainers.image.title="ai-gateway" \
      org.opencontainers.image.description="OmniRoute + MimoApi + zai-api + singbox-manager + cloudflared, single image"

# --- application artifacts ---
# OmniRoute needs nothing here -- it's already fully baked into this base
# image at /app (standalone Next.js build, better-sqlite3, healthcheck.mjs,
# check-permissions.sh entrypoint), exactly as diegosouzapw published it.

COPY --from=mimo-builder /out/mimoproxy /opt/mimo/mimoproxy
COPY --from=mimo-builder /src/templates /opt/mimo/templates

COPY --from=glm-builder /out/zai-api /opt/glm/zai-api
COPY --from=glm-builder /out/token-collector /opt/glm/token-collector

COPY --from=kimi-builder /out/kimi-api /opt/kimi/kimi-api
COPY --from=deepseek-builder /out/deepseek-proxy /opt/deepseek/deepseek-proxy

COPY --from=grok2api-backend-builder --chmod=0755 /out/grok2api /opt/grok2api/grok2api
COPY --from=grok2api-frontend-builder /src/frontend/dist /opt/grok2api/frontend/dist
COPY --from=grok2api_src VERSION /opt/grok2api/VERSION
# Real entrypoint from the repo, unmodified -- it drops from root to the
# grok2api user via su-exec before exec'ing the CMD it's given.
COPY --from=grok2api_src --chmod=0755 docker/entrypoint.sh /usr/local/bin/grok2api-entrypoint
COPY --from=su-exec-builder /src/su-exec /usr/local/bin/su-exec

COPY --from=singbox-manager /out/singbox-manager /usr/local/bin/singbox-manager
COPY --from=cloudflared-fetch /usr/local/bin/cloudflared /usr/local/bin/cloudflared

# --- healthcheck + s6 service tree ---
COPY healthcheck.sh /usr/local/bin/healthcheck.sh
RUN chmod +x /usr/local/bin/healthcheck.sh
COPY s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN chmod -R +x /etc/s6-overlay/s6-rc.d/*/run /etc/s6-overlay/s6-rc.d/*/up /etc/s6-overlay/s6-rc.d/*/run.sh 2>/dev/null || true

# --- data dirs (namespaced per service to avoid collisions) ---
# grok2api's own upstream image runs its process as a UID-10001 non-root
# user (dropped to by its entrypoint via su-exec) -- replicate that user here
# so the entrypoint's su-exec step has a real target to drop into.
RUN groupadd -g 10001 grok2api \
    && useradd -u 10001 -g grok2api -M -s /usr/sbin/nologin grok2api

RUN mkdir -p /data/omniroute /data/mimo /data/glm /data/grok2api /data/sing-box \
    && chown -R node:node /data/omniroute \
    && chown -R grok2api:grok2api /data/grok2api /opt/grok2api

# default (overridable) network-bind mode: 0.0.0.0 unless TUNNEL_ONLY kicks in at runtime
RUN mkdir -p /run/s6/container_environment \
    && printf '0.0.0.0' > /etc/s6-overlay/s6-rc.d/network-mode-init/BIND_ADDR_DEFAULT

ENV OMNIROUTE_PORT=20128 \
    MIMO_PORT=3003 \
    GLM_PORT=3001 \
    KIMI_PORT=3002 \
    KIMI_ACCESS_TOKEN= \
    KIMI_TOKEN=Waguri \
    GROK2API_PORT=3004 \
    CLASH_API_PORT=9090 \
    MIXED_PORT=7890 \
    ZAI_TIMEOUT=300000 \
    ZAI_AUTH_TOKEN=Waguri \
    ZAI_AGENT_MODE=true \
    ZAI_LOG_LEVEL=info \
    ZAI_LOG_FORMAT=text

EXPOSE 20128 3000 3001 3002 8000 9090 7890

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/healthcheck.sh"]

ENTRYPOINT ["/init"]
