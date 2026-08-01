#!/usr/bin/env bash
# Local equivalent of the GitHub Actions build -- convenient for testing
# before pushing. Requires Docker Buildx (bundled with modern Docker).
set -euo pipefail

OMNIROUTE_REPO="${OMNIROUTE_REPO:-https://github.com/diegosouzapw/OmniRoute.git#release/v3.8.50}"
MIMO_REPO="${MIMO_REPO:-https://github.com/hooshidev3/mimo-ai-proxy.git#main}"
METACUBEXD_REPO="${METACUBEXD_REPO:-https://github.com/MetaCubeX/metacubexd.git#main}"
GLM_REPO="${GLM_REPO:-https://github.com/borhandarabi/GLM-Free-API.git#main}"
MIHOMO_VERSION="${MIHOMO_VERSION:-v1.19.27}"
TAG="${TAG:-ai-gateway:local}"

if [ "$GLM_REPO" = "./glm-local-checkout" ] && [ ! -d "$GLM_REPO" ]; then
  echo "!! GLM_REPO points at ./glm-local-checkout but that directory doesn't exist." >&2
  echo "   Either clone the zai-api source there, or pass a real git URL:" >&2
  echo "   GLM_REPO=https://github.com/<org>/zai-api.git#main ./build.sh" >&2
  exit 1
fi

docker buildx build \
  --build-context omniroute_src="${OMNIROUTE_REPO}" \
  --build-context mimo_src="${MIMO_REPO}" \
  --build-context metacubexd_src="${METACUBEXD_REPO}" \
  --build-context glm_src="${GLM_REPO}" \
  --build-arg MIHOMO_VERSION="${MIHOMO_VERSION}" \
  -t "${TAG}" \
  --load \
  .

echo "Built ${TAG}"
