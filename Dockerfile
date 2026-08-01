# syntax=docker/dockerfile:1.7
#
# ai-gateway — single-image bundle of:
#   - OmniRoute   (Node/Next.js)      : AI gateway / router UI + API
#   - MimoApi     (Go)                : Mimo/Xiaomi provider proxy
#   - zai-api     (Go, aka "GlmApi")  : Z.ai/GLM provider proxy
#   - mihomo + metacubexd (Go + Node) : proxy kernel + dashboard
#   - cloudflared (Go, prebuilt)      : optional Cloudflare Tunnel
#
# Each application stage below pulls its source from a *named build context*
# pointing at the real upstream repo, so `docker build` always extracts the
# current entrypoint/env/build-steps from the source of truth instead of a
# locally vendored copy. Supply the contexts with `--build-context`, e.g.:
#
#   docker buildx build \
#     --build-context omniroute_src=https://github.com/diegosouzapw/OmniRoute.git#release/v3.8.50 \
#     --build-context mimo_src=https://github.com/hooshidev3/mimo-ai-proxy.git#main \
#     --build-context metacubexd_src=https://github.com/MetaCubeX/metacubexd.git#main \
#     --build-context glm_src=<GLM_REPO_URL>#<GLM_REF>   \
#     -t ai-gateway:latest .
#
# See build.sh / .github/workflows/docker-build.yml for the wired-up version.
#
# NOTE (open item): glm_src has no URL yet (repo not pushed). Until it exists,
# pass a local path instead, e.g. --build-context glm_src=./glm-local-checkout
# so the image still builds for local testing.

ARG MIHOMO_VERSION=v1.19.27

# ─────────────────────────── OmniRoute (Node) ───────────────────────────────
FROM node:26-trixie-slim AS omniroute-builder
WORKDIR /app
COPY --from=omniroute_src . .
RUN --mount=type=cache,id=apt-cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,id=apt-lists,target=/var/lib/apt/lists,sharing=locked \
    apt-get update \
    && apt-get install -y --no-install-recommends python3 make g++ ca-certificates \
    && rm -rf /var/lib/apt/lists/*
ENV NPM_CONFIG_LEGACY_PEER_DEPS=true \
    OMNIROUTE_MITM_STUB=1 \
    OMNIROUTE_USE_TURBOPACK=1
ARG OMNIROUTE_BUILD_MEMORY_MB=4096
ENV NODE_OPTIONS="--max-old-space-size=${OMNIROUTE_BUILD_MEMORY_MB}"
RUN --mount=type=cache,id=npm-cache,target=/root/.npm \
    npm ci --no-audit --no-fund --legacy-peer-deps --ignore-scripts \
    && (cd node_modules/better-sqlite3 \
        && node /usr/local/lib/node_modules/npm/node_modules/node-gyp/bin/node-gyp.js rebuild) \
    && node -e "require('better-sqlite3')(':memory:').close()" \
    && (node node_modules/tls-client-node/scripts/postinstall.js || true)
RUN --mount=type=cache,id=next-cache,target=/app/.build/next/cache \
    mkdir -p /app/data && npm run build
# Entry point extracted from the repo itself: dev/run-standalone.mjs

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
# go.mod is pre-tidy as of writing; resolve real deps at build time.
RUN go mod tidy
# captcha.go is build-only (compiled for availability, never auto-run).
RUN go build -trimpath -gcflags="all=-l=4" -ldflags="-s -w" -o /out/token-collector captcha.go
# main.go is the actual service entry point.
RUN go build -trimpath -gcflags="all=-l=4" -ldflags="-s -w" -o /out/zai-api main.go

# ───────────────────────── metacubexd UI (static) ───────────────────────────
FROM node:22-alpine AS metacubexd-ui
ENV PNPM_HOME=/pnpm PATH=/pnpm:$PATH HUSKY=0
WORKDIR /repo
RUN corepack enable
COPY --from=metacubexd_src . .
RUN pnpm install --frozen-lockfile
RUN pnpm --filter @metacubexd/ui generate
# -> /repo/packages/ui/.output/public

# ─────────────────────── metacubexd server (Nitro) ──────────────────────────
FROM node:22-alpine AS metacubexd-server
ENV PNPM_HOME=/pnpm PATH=/pnpm:$PATH HUSKY=0
WORKDIR /repo
RUN corepack enable
COPY --from=metacubexd_src . .
RUN pnpm install --frozen-lockfile
RUN pnpm --filter @metacubexd/server... build
# -> /repo/apps/server/.output  (entry: server/index.mjs)

# ───────────────────────── mihomo kernel binary ─────────────────────────────
FROM alpine:3.20 AS mihomo-kernel
ARG TARGETARCH
ARG MIHOMO_VERSION
RUN apk add --no-cache curl ca-certificates gzip
RUN set -eux; \
    if [ "$TARGETARCH" = "amd64" ]; then ASSET="mihomo-linux-amd64-compatible-${MIHOMO_VERSION}.gz"; \
    elif [ "$TARGETARCH" = "arm64" ]; then ASSET="mihomo-linux-arm64-${MIHOMO_VERSION}.gz"; \
    else echo "unsupported arch $TARGETARCH" >&2; exit 1; fi; \
    curl -fsSL "https://github.com/MetaCubeX/mihomo/releases/download/${MIHOMO_VERSION}/${ASSET}" -o /tmp/k.gz; \
    gunzip -c /tmp/k.gz > /usr/local/bin/mihomo; \
    chmod +x /usr/local/bin/mihomo; \
    /usr/local/bin/mihomo -v

# ───────────────────────────── cloudflared ──────────────────────────────────
FROM alpine:3.20 AS cloudflared-fetch
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates \
    && curl -fsSL "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${TARGETARCH}" \
       -o /usr/local/bin/cloudflared \
    && chmod +x /usr/local/bin/cloudflared

# ══════════════════════════ Final runtime image ═════════════════════════════
FROM node:26-trixie-slim AS runtime

ARG S6_OVERLAY_VERSION=3.2.1.0
ARG TARGETARCH
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

RUN --mount=type=cache,id=apt-cache-rt,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,id=apt-lists-rt,target=/var/lib/apt/lists,sharing=locked \
    apt-get update \
    && apt-get install -y --no-install-recommends \
       ca-certificates iptables iproute2 curl bash \
    && rm -rf /var/lib/apt/lists/*

LABEL org.opencontainers.image.title="ai-gateway" \
      org.opencontainers.image.description="OmniRoute + MimoApi + zai-api + mihomo/metacubexd + cloudflared, single image"

# --- application artifacts ---
COPY --from=omniroute-builder /app/.build/next/standalone /opt/omniroute
COPY --from=omniroute-builder /app/node_modules/better-sqlite3 /opt/omniroute/node_modules/better-sqlite3
COPY --from=omniroute-builder /app/scripts/dev/healthcheck.mjs /opt/omniroute/healthcheck.mjs

COPY --from=mimo-builder /out/mimoproxy /opt/mimo/mimoproxy
COPY --from=mimo-builder /src/templates /opt/mimo/templates

COPY --from=glm-builder /out/zai-api /opt/glm/zai-api
COPY --from=glm-builder /out/token-collector /opt/glm/token-collector

COPY --from=metacubexd-server /repo/apps/server/.output /opt/metacubexd
COPY --from=metacubexd-ui /repo/packages/ui/.output/public /opt/metacubexd/ui-dist

COPY --from=mihomo-kernel /usr/local/bin/mihomo /usr/local/bin/mihomo
COPY --from=cloudflared-fetch /usr/local/bin/cloudflared /usr/local/bin/cloudflared

# --- default mihomo configs (override by mounting /data/mihomo/config.yaml) ---
# config.yaml: TUN + mixed-port, for plain Docker hosts with NET_ADMIN/TUN access.
# config.no-tun.yaml: mixed-port only, for platforms that don't allow privileged
# containers (e.g. Railway) -- selected automatically when DISABLE_TUN=true.
COPY mihomo/config.yaml /opt/mihomo-default-config.yaml
COPY mihomo/config.no-tun.yaml /opt/mihomo-default-config.no-tun.yaml

# --- healthcheck + s6 service tree ---
COPY healthcheck.sh /usr/local/bin/healthcheck.sh
RUN chmod +x /usr/local/bin/healthcheck.sh
COPY s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN chmod -R +x /etc/s6-overlay/s6-rc.d/*/run /etc/s6-overlay/s6-rc.d/*/up 2>/dev/null || true

# --- data dirs (namespaced per service to avoid collisions) ---
RUN mkdir -p /data/omniroute /data/mimo /data/glm /data/metacubexd /data/mihomo \
    && cp /opt/mihomo-default-config.yaml /data/mihomo/config.yaml.default \
    && cp /opt/mihomo-default-config.no-tun.yaml /data/mihomo/config.no-tun.yaml.default

# default (overridable) network-bind mode: 0.0.0.0 unless TUNNEL_ONLY kicks in at runtime
RUN mkdir -p /run/s6/container_environment \
    && printf '0.0.0.0' > /etc/s6-overlay/s6-rc.d/network-mode-init/BIND_ADDR_DEFAULT

ENV OMNIROUTE_PORT=20128 \
    MIMO_PORT=3000 \
    ZAI_PORT=3001 \
    CONTROL_PORT=8080 \
    CLASH_API_PORT=9090 \
    MIXED_PORT=7890 \
    ZAI_TIMEOUT=300000 \
    ZAI_AUTH_TOKEN=Waguri \
    ZAI_AGENT_MODE=true \
    ZAI_LOG_LEVEL=info \
    ZAI_LOG_FORMAT=text \
    DISABLE_TUN=false

EXPOSE 20128 3000 3001 8080 9090 7890

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/healthcheck.sh"]

ENTRYPOINT ["/init"]
