#!/usr/bin/env bash
# Local equivalent of the GitHub Actions build -- convenient for testing
# before pushing. Requires Docker Buildx (bundled with modern Docker).
set -euo pipefail

MIMO_REPO="${MIMO_REPO:-https://github.com/hooshidev3/mimo-ai-proxy.git#main}"
GROK2API_REPO="${GROK2API_REPO:-https://github.com/chenyme/grok2api.git#main}"
ZAI_REPO="${ZAI_REPO:-https://github.com/izaart95-jpg/GLM-Free-API.git#main}"
KIMI_REPO="${KIMI_REPO:-https://github.com/izaart95-jpg/KimiFreeAPI.git#main}"
DEEPSEEK_REPO="${DEEPSEEK_REPO:-https://github.com/izaart95-jpg/DeepSeekFreeAPI.git#main}"
FLARESOLVERR_REPO="${FLARESOLVERR_REPO:-https://github.com/Rorqualx/flaresolverr-go.git#main}"
QWEN2API_REPO="${QWEN2API_REPO:-https://github.com/XxxXTeam/Qwen2API_Go.git#main}"
ZENFREEAPI_REPO="${ZENFREEAPI_REPO:-https://github.com/izaart95-jpg/ZenFreeAPI.git#main}"
TAG="${TAG:-ai-gateway:local}"

docker buildx build \
  --build-context mimo_src="${MIMO_REPO}" \
  --build-context grok2api_src="${GROK2API_REPO}" \
  --build-context zai_src="${ZAI_REPO}" \
  --build-context kimi_src="${KIMI_REPO}" \
  --build-context deepseek_src="${DEEPSEEK_REPO}" \
  --build-context flaresolverr_src="${FLARESOLVERR_REPO}" \
  --build-context qwen2api_src="${QWEN2API_REPO}" \
  --build-context zenfreeapi_src="${ZENFREEAPI_REPO}" \
  -t "${TAG}" \
  --load \
  .

echo "Built ${TAG}"
