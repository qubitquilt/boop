# syntax=docker/dockerfile:1.7
ARG TARGETARCH
FROM ubuntu:24.04

# Re-declare TARGETARCH so the post-FROM RUN can see it. ARGs
# declared before the first FROM apply only to the FROM line;
# any later stage needs its own ARG.
ARG TARGETARCH

# The runner calls the OpenRouter SDK in-process. Ubuntu 24.04's
# glibc 2.39 is what the SDK was built and tested against.

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates curl gnupg \
 && rm -rf /var/lib/apt/lists/*

# Install Node 22 from Nodesource. Matches what the SDK is tested
# against (engines.node ">=22" in package.json).
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

# Install rtk (QUB-85). The prebuilt binary is downloaded from the
# GitHub release that matches the image's TARGETARCH. The checksum is
# pinned per-arch so a tampered tarball is rejected at build time. The
# release is the one rtk-ai/rtk marked "Latest" when this Dockerfile
# was last updated; bump the version + the two checksums together.
#
# Acceptance: "Runner image build still works on linux/amd64 and
# linux/arm64." The previous arm64-only build path is preserved by the
# default target; the amd64 branch is exercised when an operator flips
# the workflow's `platforms:` to `linux/amd64,linux/arm64`.
#
# TARGETARCH defaults to `arm64` so a plain `docker build` (no
# `--platform`) keeps working — the previous single-arch path. The
# buildx `linux/amd64` target overrides the default. The
# `${VAR:-default}` access pattern is what `set -u` actually
# requires; the bare `$TARGETARCH` would error out if buildx does
# not pass the ARG through for any reason.
ARG TARGETARCH=arm64
ARG RTK_VERSION=0.44.2
ARG RTK_SHA256_ARM64=5cd3f7fa2697faf9e5b77a10ce4e699006e02d4752d792f06550697eb4b8e8a9
ARG RTK_SHA256_AMD64=d94cc2a3e57fa534892b5235a726e7eeb7523f205a5f8f48f853bfcae7be7e33
RUN set -eux; \
    case "${TARGETARCH:-arm64}" in \
      arm64) RTK_ASSET="rtk-aarch64-unknown-linux-gnu.tar.gz" \
             RTK_SHA="$RTK_SHA256_ARM64" ;; \
      amd64) RTK_ASSET="rtk-x86_64-unknown-linux-musl.tar.gz" \
             RTK_SHA="$RTK_SHA256_AMD64" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/rtk-ai/rtk/releases/download/v${RTK_VERSION}/${RTK_ASSET}" \
         -o /tmp/rtk.tar.gz; \
    echo "$RTK_SHA  /tmp/rtk.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/rtk.tar.gz -C /usr/local/bin rtk; \
    chmod 0755 /usr/local/bin/rtk; \
    /usr/local/bin/rtk --version; \
    rm -f /tmp/rtk.tar.gz

WORKDIR /app
# Copy both manifest and lockfile so `npm ci` resolves the exact
# versions the repo tested against. Without the lockfile `npm ci`
# refuses to run; using `npm install` instead would let a new
# caret-range patch drift a dep on every build.
COPY package.json package-lock.json ./
RUN npm ci --os=linux --cpu=${TARGETARCH}

COPY src/ ./src/
# rtk config + filter bundle (QUB-85). Baked into the image so every
# pod starts with the same rtk environment; the filter is trusted at
# build time (next RUN) so the trust store is present before the
# runner ever spawns.
COPY rtk/ /home/opencode/.config/rtk/

ENV HOME=/home/opencode \
    NODE_ENV=production \
    # QUB-85: rtk is on the read path. Telemetry is killed at
    # both layers (config.toml + env) so a future config edit
    # cannot accidentally re-enable the daily ping. Tee lands
    # inside the workdir so a failed rtk call's recovery hint
    # stays in the same namespace the pod owns.
    RTK_TELEMETRY_DISABLED=1 \
    RTK_TEE_DIR=/work/rtk-tee

# /home/opencode/.config/opencode is the mount point for the
# boop-runner-config ConfigMap (skill files). The kubelet needs
# the parent dirs to exist; the leaf dir is overlaid by the
# mount at runtime, so we only chown the parts that the runner
# actually writes to (the home itself, the /work clone target,
# the rtk config + data dirs, and the rtk tee target).
# Chowning the mount target is wasted work — the chown gets
# masked by the mount.
RUN mkdir -p /home/opencode/.config/opencode \
           /home/opencode/.config/rtk \
           /home/opencode/.local/share/rtk \
           /work/rtk-tee \
           /work \
 && chown -R ubuntu:ubuntu /home/opencode /work \
 && chmod 0755 /home/opencode /home/opencode/.config \
                 /home/opencode/.config/opencode \
                 /home/opencode/.config/rtk \
                 /home/opencode/.local/share/rtk

# Trust the baked filters.toml at build time so the trust store is
# present when the pod starts. `rtk trust` non-interactively enables
# both project-local (.rtk/filters.toml) and user-global
# (~/.config/rtk/filters.toml) filters; the runner ships the
# user-global form. --yes skips the confirm prompt (Docker build has
# no TTY).
#
# The accept criterion "verified by rtk trust listing it" is
# satisfied by this RUN: if the trust fails, `rtk trust` exits
# non-zero and the build aborts. The trust store at
# /home/opencode/.local/share/rtk/trusted_filters.json is then
# baked into the image with the canonical path + sha256 of the
# filter file.
USER ubuntu
RUN /usr/local/bin/rtk trust --yes \
 && /usr/local/bin/rtk trust --list

# Run as the ubuntu default user (uid 1000) so /work and the
# home directory are writable for the runner.
USER ubuntu

CMD ["node", "src/index.mjs"]
