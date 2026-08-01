#!/usr/bin/env bash
# Local equivalent of the GitHub Actions build -- convenient for testing
# before pushing. Requires Docker Buildx (bundled with modern Docker).
set -euo pipefail

MIMO_REPO="${MIMO_REPO:-https://github.com/hooshidev3/mimo-ai-proxy.git#main}"
METACUBEXD_REPO="${METACUBEXD_REPO:-https://github.com/MetaCubeX/metacubexd.git#main}"
GROK2API_REPO="${GROK2API_REPO:-https://github.com/i-panel/grok2api-go.git#main}"
GLM_REPO="${GLM_REPO:-https://github.com/borhandarabi/GLM-Free-API.git#main}"
KIMI_REPO="${KIMI_REPO:-https://github.com/izaart95-jpg/KimiFreeAPI.git#main}"
MIHOMO_VERSION="${MIHOMO_VERSION:-v1.19.27}"
# OmniRoute is pulled as a prebuilt base image, not built from source 
OMNIROUTE_IMAGE="${OMNIROUTE_IMAGE:-diegosouzapw/omniroute:3.8.49-web}"
TAG="${TAG:-ai-gateway:local}"

docker buildx build \
  --build-context mimo_src="${MIMO_REPO}" \
  --build-context metacubexd_src="${METACUBEXD_REPO}" \
  --build-context grok2api_src="${GROK2API_REPO}" \
  --build-context glm_src="${GLM_REPO}" \
  --build-context kimi_src="${KIMI_REPO}" \
  --build-arg MIHOMO_VERSION="${MIHOMO_VERSION}" \
  --build-arg OMNIROUTE_IMAGE="${OMNIROUTE_IMAGE}" \
  -t "${TAG}" \
  --load \
  .

echo "Built ${TAG}"
