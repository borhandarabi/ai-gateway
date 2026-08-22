# syntax=docker/dockerfile:1.7
#
# OmniRoute -- deployed as its own image/container, deliberately separate
# from ai-gateway's Dockerfile. Docker resource limits (CPU/memory) apply
# per-container, and OmniRoute's dashboard + provider traffic must not
# compete with ai-gateway's own services (several of which launch Chromium)
# for the same memory budget.
#
# This is intentionally a thin pass-through, not a rebuild from source:
# OmniRoute already publishes an official multi-arch image
# (diegosouzapw/omniroute), and this repo has no reason to rebuild it. The
# only purpose of this file is to give platforms that only support
# "build a Dockerfile from a repo" (Railway, for example) something to point
# at, pinned to a specific upstream tag for reproducible deploys instead of
# a floating `:latest`.
#
# Bump OMNIROUTE_VERSION deliberately when you want to move to a newer
# OmniRoute release -- check https://github.com/diegosouzapw/OmniRoute/releases
# first for breaking changes (JWT_SECRET/API_KEY_SECRET handling in
# particular has changed between releases).
ARG OMNIROUTE_VERSION=3.8.50
FROM diegosouzapw/omniroute:${OMNIROUTE_VERSION}
