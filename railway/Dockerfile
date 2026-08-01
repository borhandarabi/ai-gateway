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

# OmniRoute is pulled as a prebuilt base image (see final stage below), not
# built from source -- removes the memory-heavy Next.js build from this
# pipeline entirely. Pin to a specific tag for reproducible builds.
ARG OMNIROUTE_IMAGE=diegosouzapw/omniroute:latest-web
ARG MIMO_REPO_URL=https://github.com/hooshidev3/mimo-ai-proxy.git
ARG MIMO_REF=main
ARG METACUBEXD_REPO_URL=https://github.com/MetaCubeX/metacubexd.git
ARG METACUBEXD_REF=main
ARG GROK2API_REPO_URL=https://github.com/i-panel/grok2api-go.git
ARG GROK2API_REF=main
ARG GLM_REPO_URL=https://github.com/borhandarabi/GLM-Free-API.git
ARG GLM_REF=main
ARG KIMI_REPO_URL=https://github.com/izaart95-jpg/KimiFreeAPI.git
ARG KIMI_REF=main

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

# ─────────────────────────── Kimi-api (Go) ──────────────────────────
FROM golang:1.26-alpine AS kimi-builder
ARG KIMI_REPO_URL
ARG KIMI_REF
RUN apk add --no-cache git gcc musl-dev
RUN go mod init kimi-api
# go.mod is pre-tidy as of writing; resolve real deps at build time.
RUN go mod tidy
# main.go is the actual service entry point.
RUN go build -ldflags "-s -w" -trimpath -o /out/kimi-api main.go

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

# ───────────────────────── grok2api-go frontend (Vite) ──────────────────────
FROM node:22-alpine AS grok2api-frontend-builder
ARG GROK2API_REPO_URL
ARG GROK2API_REF
RUN apk add --no-cache git
WORKDIR /src/frontend-repo
RUN git clone --depth 1 --branch "${GROK2API_REF}" "${GROK2API_REPO_URL}" .
WORKDIR /src/frontend-repo/frontend
RUN corepack enable && pnpm install --frozen-lockfile && pnpm build

# ───────────────────────── grok2api-go backend (Go) ─────────────────────────
FROM golang:1.26-alpine AS grok2api-backend-builder
ARG GROK2API_REPO_URL
ARG GROK2API_REF
ARG TARGETOS
ARG TARGETARCH
RUN apk add --no-cache ca-certificates git
WORKDIR /src
RUN git clone --depth 1 --branch "${GROK2API_REF}" "${GROK2API_REPO_URL}" .
WORKDIR /src/backend
RUN go mod download
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -buildvcs=false -trimpath -ldflags="-s -w" -o /out/grok2api ./cmd/grok2api

FROM node:26-trixie-slim AS su-exec-builder
RUN apt-get update && apt-get install -y --no-install-recommends \
      git build-essential ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /src
RUN git clone --depth 1 https://github.com/ncopa/su-exec.git . && make

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
# Base = the official OmniRoute image (see ARG OMNIROUTE_IMAGE above). Its
# last USER layer is `node` -- override back to root for our own setup; s6's
# /init needs root, each service that should run unprivileged drops to its
# own user itself via su-exec inside its own s6 run script.
FROM ${OMNIROUTE_IMAGE} AS runtime
USER root

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

# OmniRoute needs nothing here -- already baked into the base image at /app.

COPY --from=mimo-builder /out/mimoproxy /opt/mimo/mimoproxy
COPY --from=mimo-builder /src/templates /opt/mimo/templates

COPY --from=glm-builder /out/zai-api /opt/glm/zai-api
COPY --from=glm-builder /out/token-collector /opt/glm/token-collector
COPY glm/tokens.sqlite /data/glm/tokens.sqlite

COPY --from=kimi-builder /out/kimi-api /opt/kimi/kimi-api

COPY --from=metacubexd-server /repo/apps/server/.output /opt/metacubexd
COPY --from=metacubexd-ui /repo/packages/ui/.output/public /opt/metacubexd/ui-dist

COPY --from=grok2api-backend-builder --chmod=0755 /out/grok2api /opt/grok2api/grok2api
COPY --from=grok2api-frontend-builder /src/frontend-repo/frontend/dist /opt/grok2api/frontend/dist
COPY --from=grok2api-backend-builder /src/VERSION /opt/grok2api/VERSION
COPY --from=grok2api-backend-builder --chmod=0755 /src/docker/entrypoint.sh /usr/local/bin/grok2api-entrypoint
COPY --from=su-exec-builder /src/su-exec /usr/local/bin/su-exec

COPY --from=mihomo-kernel /usr/local/bin/mihomo /usr/local/bin/mihomo
COPY --from=cloudflared-fetch /usr/local/bin/cloudflared /usr/local/bin/cloudflared

COPY mihomo/config.yaml /opt/mihomo-default-config.yaml
COPY mihomo/config.no-tun.yaml /opt/mihomo-default-config.no-tun.yaml

COPY healthcheck.sh /usr/local/bin/healthcheck.sh
RUN chmod +x /usr/local/bin/healthcheck.sh
COPY s6-rc.d/ /etc/s6-overlay/s6-rc.d/
RUN chmod -R +x /etc/s6-overlay/s6-rc.d/*/run /etc/s6-overlay/s6-rc.d/*/up 2>/dev/null || true

RUN groupadd -g 10001 grok2api \
    && useradd -u 10001 -g grok2api -M -s /usr/sbin/nologin grok2api

RUN mkdir -p /data/omniroute /data/mimo /data/glm /data/grok2api /data/metacubexd /data/mihomo \
    && chown -R node:node /data/omniroute \
    && chown -R grok2api:grok2api /data/grok2api /opt/grok2api \
    && cp /opt/mihomo-default-config.yaml /data/mihomo/config.yaml.default \
    && cp /opt/mihomo-default-config.no-tun.yaml /data/mihomo/config.no-tun.yaml.default

ENV OMNIROUTE_PORT=20128 \
    MIMO_PORT=3000 \
    GLM_PORT=3001 \
    KIMI_PORT=3002 \
    KIMI_ACCESS_TOKEN= \
    KIMI_TOKEN=Waguri \
    GROK2API_PORT=8000 \
    CONTROL_PORT=8080 \
    CLASH_API_PORT=9090 \
    MIXED_PORT=7890 \
    ZAI_TIMEOUT=300000 \
    ZAI_AUTH_TOKEN=Waguri \
    ZAI_AGENT_MODE=true \
    ZAI_LOG_LEVEL=info \
    ZAI_LOG_FORMAT=text \
    DISABLE_TUN=true \
    TZ=Asia/Shanghai

EXPOSE 20128 3000 3001 3002 8000 8080 9090 7890

HEALTHCHECK --interval=30s --timeout=10s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/healthcheck.sh"]

ENTRYPOINT ["/init"]
