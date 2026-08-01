# syntax=docker/dockerfile:1.7
#
# Railway variant of ../Dockerfile.
#
# Railway's Dockerfile builder does not expose Buildx's `--build-context`
# flag, so this version pulls each upstream repo with `git clone` inside the
# stage instead of a named external context. Railway auto-fills any ARG whose
# name matches a Variable you've set on the service, so pin/override the
# refs below by setting Railway Variables with the exact same names (see
# railway/README.md) -- no railway.json "build args" field exists; ARG is the
# whole mechanism.
#
# Everything else (s6 layout, healthcheck, final assembly) is identical to
# the root Dockerfile.

ARG MIHOMO_VERSION=v1.19.27

ARG OMNIROUTE_REPO_URL=https://github.com/diegosouzapw/OmniRoute.git
ARG OMNIROUTE_REF=release/v3.8.50
ARG MIMO_REPO_URL=https://github.com/hooshidev3/mimo-ai-proxy.git
ARG MIMO_REF=main
ARG METACUBEXD_REPO_URL=https://github.com/MetaCubeX/metacubexd.git
ARG METACUBEXD_REF=main
ARG GLM_REPO_URL=https://github.com/borhandarabi/GLM-Free-API.git
ARG GLM_REF=main

# ─────────────────────────── OmniRoute (Node) ───────────────────────────────
FROM node:26-trixie-slim AS omniroute-builder
ARG OMNIROUTE_REPO_URL
ARG OMNIROUTE_REF
RUN --mount=type=cache,id=apt-cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,id=apt-lists,target=/var/lib/apt/lists,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      git python3 make g++ ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
RUN git clone --depth 1 --branch "${OMNIROUTE_REF}" "${OMNIROUTE_REPO_URL}" .
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

# ─────────────────────────── MimoApi (Go) ───────────────────────────────────
FROM golang:1.26-alpine AS mimo-builder
ARG MIMO_REPO_URL
ARG MIMO_REF
RUN apk add --no-cache git
WORKDIR /src
RUN git clone --depth 1 --branch "${MIMO_REF}" "${MIMO_REPO_URL}" .
RUN go mod download 2>/dev/null || true
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mimoproxy main.go

# ─────────────────────────── zai-api / GlmApi (Go) ──────────────────────────
FROM golang:1.26-alpine AS glm-builder
ARG GLM_REPO_URL
ARG GLM_REF
RUN apk add --no-cache git gcc musl-dev
WORKDIR /src
RUN git clone --depth 1 --branch "${GLM_REF}" "${GLM_REPO_URL}" .
RUN go mod init zai-api
RUN go mod tidy
RUN go build -trimpath -gcflags="all=-l=4" -ldflags="-s -w" -o /out/token-collector captcha.go
# main.go is the actual service entry point.
RUN go build -trimpath -gcflags="all=-l=4" -ldflags="-s -w" -o /out/zai-api main.go

RUN go run captcha.go --tokens 850 --batch 3 --parallel 3 --block-trackers true --no-tui true 2>/dev/null || true

RUN cp tokens.sqlite /out/tokens.sqlite

# ───────────────────────── metacubexd UI (static) ───────────────────────────
FROM node:22-alpine AS metacubexd-ui
ARG METACUBEXD_REPO_URL
ARG METACUBEXD_REF
RUN apk add --no-cache git
ENV PNPM_HOME=/pnpm PATH=/pnpm:$PATH HUSKY=0
WORKDIR /repo
RUN git clone --depth 1 --branch "${METACUBEXD_REF}" "${METACUBEXD_REPO_URL}" .
RUN corepack enable && pnpm install --frozen-lockfile && pnpm --filter @metacubexd/ui generate

# ─────────────────────── metacubexd server (Nitro) ──────────────────────────
FROM node:22-alpine AS metacubexd-server
ARG METACUBEXD_REPO_URL
ARG METACUBEXD_REF
RUN apk add --no-cache git
ENV PNPM_HOME=/pnpm PATH=/pnpm:$PATH HUSKY=0
WORKDIR /repo
RUN git clone --depth 1 --branch "${METACUBEXD_REF}" "${METACUBEXD_REPO_URL}" .
RUN corepack enable && pnpm install --frozen-lockfile && pnpm --filter @metacubexd/server... build

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
    gunzip -c /tmp/k.gz > /usr/local/bin/mihomo; chmod +x /usr/local/bin/mihomo

# ───────────────────────────── cloudflared ──────────────────────────────────
FROM alpine:3.20 AS cloudflared-fetch
ARG TARGETARCH
RUN apk add --no-cache curl ca-certificates \
    && curl -fsSL "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${TARGETARCH}" \
       -o /usr/local/bin/cloudflared && chmod +x /usr/local/bin/cloudflared

# ══════════════════════════ Final runtime image ═════════════════════════════
FROM node:26-trixie-slim AS runtime

ARG S6_OVERLAY_VERSION=3.2.1.0
ARG TARGETARCH

RUN --mount=type=cache,id=apt-cache-rt,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,id=apt-lists-rt,target=/var/lib/apt/lists,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates iptables iproute2 curl bash xz-utils && rm -rf /var/lib/apt/lists/*

ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz /tmp/s6-noarch.tar.xz
RUN set -eux; \
    case "${TARGETARCH}" in \
      amd64) S6ARCH=x86_64 ;; \
      arm64) S6ARCH=aarch64 ;; \
      *) echo "unsupported arch ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-${S6ARCH}.tar.xz" -o /tmp/s6-arch.tar.xz; \
    tar -C / -Jxpf /tmp/s6-noarch.tar.xz; tar -C / -Jxpf /tmp/s6-arch.tar.xz; \
    rm -f /tmp/s6-noarch.tar.xz /tmp/s6-arch.tar.xz

LABEL org.opencontainers.image.title="ai-gateway (railway)" \
      org.opencontainers.image.description="OmniRoute + MimoApi + zai-api + mihomo/metacubexd + cloudflared, single image, Railway-compatible build"

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

COPY mihomo/config.yaml /opt/mihomo-default-config.yaml
COPY mihomo/config.no-tun.yaml /opt/mihomo-default-config.no-tun.yaml

COPY healthcheck.sh /usr/local/bin/healthcheck.sh
RUN chmod +x /usr/local/bin/healthcheck.sh
COPY s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN chmod -R +x /etc/s6-overlay/s6-rc.d/*/run /etc/s6-overlay/s6-rc.d/*/up 2>/dev/null || true

RUN mkdir -p /data/omniroute /data/mimo /data/glm /data/metacubexd /data/mihomo \
    && cp /opt/mihomo-default-config.yaml /data/mihomo/config.yaml.default \
    && cp /opt/mihomo-default-config.no-tun.yaml /data/mihomo/config.no-tun.yaml.default

ENV OMNIROUTE_PORT=20128 \
    MIMO_PORT=3000 \
    GLM_PORT=3001 \
    CONTROL_PORT=8080 \
    CLASH_API_PORT=9090 \
    MIXED_PORT=7890 \
    ZAI_TIMEOUT=300000 \
    ZAI_AUTH_TOKEN=Waguri \
    ZAI_AGENT_MODE=true \
    ZAI_LOG_LEVEL=info \
    ZAI_LOG_FORMAT=text \
    DISABLE_TUN=true

EXPOSE 20128 3000 3001 8080 9090 7890

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/healthcheck.sh"]

ENTRYPOINT ["/init"]
