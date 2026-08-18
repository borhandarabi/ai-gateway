#!/usr/bin/env bash
# Local equivalent of the GitHub Actions build -- convenient for testing
# before pushing. Requires Docker Buildx (bundled with modern Docker).
set -euo pipefail

MIMO_REPO="${MIMO_REPO:-https://github.com/hooshidev3/mimo-ai-proxy.git#main}"
GROK2API_REPO="${GROK2API_REPO:-https://github.com/chenyme/grok2api.git#main}"
ZAI_REPO="${ZAI_REPO:-https://github.com/borhandarabi/GLM-Free-API.git#main}"
KIMI_REPO="${KIMI_REPO:-https://github.com/izaart95-jpg/KimiFreeAPI.git#main}"
DEEPSEEK_REPO="${DEEPSEEK_REPO:-https://github.com/izaart95-jpg/DeepSeekFreeAPI.git#main}"
FLARESOLVERR_REPO="${FLARESOLVERR_REPO:-https://github.com/Rorqualx/flaresolverr-go.git#main}"
QWEN2API_REPO="${QWEN2API_REPO:-https://github.com/Rorqualx/flaresolverr-go.git#main}"
# OmniRoute is pulled as a prebuilt base image, not built from source 
OMNIROUTE_IMAGE="${OMNIROUTE_IMAGE:-diegosouzapw/omniroute:3.8.49-web}"
TAG="${TAG:-ai-gateway:local}"

docker buildx build \
  --build-context mimo_src="${MIMO_REPO}" \
  --build-context grok2api_src="${GROK2API_REPO}" \
  --build-context zai_src="${ZAI_REPO}" \
  --build-context kimi_src="${KIMI_REPO}" \
  --build-context deepseek_src="${DEEPSEEK_REPO}" \
  --build-context flaresolverr_src="${FLARESOLVERR_REPO}" \
  --build-context qwen2api_src="${QWEN2API_REPO}" \
  --build-arg OMNIROUTE_IMAGE="${OMNIROUTE_IMAGE}" \
  -t "${TAG}" \
  --load \
  .

echo "Built ${TAG}"
