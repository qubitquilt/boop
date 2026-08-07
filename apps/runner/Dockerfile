# syntax=docker/dockerfile:1.7
# QUB-114: 4-stage build (rtk, node, build, runtime). Each stage has
# its own cache layer; the runtime stage only carries the binaries it
# actually needs (node, rtk, git, ca-certificates).

ARG TARGETARCH=arm64
ARG RTK_VERSION=0.44.2
ARG RTK_SHA256_ARM64=5cd3f7fa2697faf9e5b77a10ce4e699006e02d4752d792f06550697eb4b8e8a9
ARG RTK_SHA256_AMD64=d94cc2a3e57fa534892b5235a726e7eeb7523f205a5f8f48f853bfcae7be7e33

# Stage 1: download + verify the rtk binary. Independent cache layer
# so a Node or app-source change does not re-download the release.
FROM ubuntu:24.04 AS rtk
ARG TARGETARCH
ARG RTK_VERSION
ARG RTK_SHA256_ARM64
ARG RTK_SHA256_AMD64
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
        curl ca-certificates \
 && rm -rf /var/lib/apt/lists/*
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
    mkdir -p /out; \
    tar -xzf /tmp/rtk.tar.gz -C /out rtk; \
    chmod 0755 /out/rtk; \
    /out/rtk --version; \
    rm -f /tmp/rtk.tar.gz

# Stage 2: install Node 22 from the official nodejs.org tarball.
# Replaces the NodeSource curl-pipe-bash + gnupg path. The tarball
# ships unstripped binaries; strip here so the runtime image does not
# carry debug symbols. binutils is only needed in this stage, so it
# does not bloat the runtime.
FROM ubuntu:24.04 AS node
ARG TARGETARCH
ARG NODE_VERSION=22.21.1
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl xz-utils binutils \
 && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    case "${TARGETARCH:-arm64}" in \
      arm64) NODE_ARCH="arm64" ;; \
      amd64) NODE_ARCH="amd64" ;; \
      *) echo "unsupported TARGETARCH: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSLo /tmp/node.tar.xz "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${NODE_ARCH}.tar.xz"; \
    mkdir -p /opt/node; \
    tar -xJf /tmp/node.tar.xz -C /opt/node --strip-components=1; \
    strip /opt/node/bin/node 2>/dev/null || true; \
    # Drop docs/man/examples/include to shrink the node payload.
    rm -rf /opt/node/share \
           /opt/node/include \
           /opt/node/lib/node_modules/npm/man \
           /opt/node/lib/node_modules/npm/docs \
           /tmp/node.tar.xz; \
    ln -sf /opt/node/bin/node /usr/local/bin/node; \
    ln -sf /opt/node/bin/npm  /usr/local/bin/npm; \
    ln -sf /opt/node/bin/npx  /usr/local/bin/npx; \
    node --version; npm --version

# Stage 3: npm ci + copy source + trust the rtk filter bundle.
# HOME=/home/opencode so the trust store lands at the path the k8s
# pod expects. Runs as `ubuntu` (uid 1000) so the trust store is
# owned by the same user the runner uses at runtime.
FROM node AS build
ARG TARGETARCH
ENV HOME=/home/opencode
COPY --from=rtk /out/rtk /usr/local/bin/rtk
WORKDIR /app
# Copy both manifest and lockfile so `npm ci` resolves the exact
# versions the repo tested against. Without the lockfile `npm ci`
# refuses to run; using `npm install` instead would let a new
# caret-range patch drift a dep on every build.
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm,sharing=locked \
    npm ci --os=linux --cpu=${TARGETARCH}
COPY src/ ./src/
# rtk config + filter bundle (QUB-85). Baked into the image so every
# pod starts with the same rtk environment; the filter is trusted at
# build time (next RUN) so the trust store is present before the
# runner ever spawns. Pre-create /home/opencode owned by ubuntu so
# the trust step (USER ubuntu) can write into ~/.local.
RUN mkdir -p /home/opencode/.config/rtk \
 && chown -R ubuntu:ubuntu /home/opencode
COPY --chown=ubuntu:ubuntu rtk/ /home/opencode/.config/rtk/
USER ubuntu
RUN mkdir -p /home/opencode/.local/share/rtk \
 && /usr/local/bin/rtk trust --yes \
 && /usr/local/bin/rtk trust --list

# Stage 4: minimal runtime. ubuntu base + git + ca-certificates + the
# stripped node tarball + the rtk binary + the app + the rtk trust
# store + the rtk config dir.
FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates git \
 && rm -rf /var/lib/apt/lists/*
COPY --from=node  /opt/node /opt/node
COPY --from=rtk   /out/rtk  /usr/local/bin/rtk
COPY --from=build /app      /app
COPY --from=build --chown=ubuntu:ubuntu /home/opencode/.config/rtk /home/opencode/.config/rtk
COPY --from=build --chown=ubuntu:ubuntu /home/opencode/.local/share/rtk /home/opencode/.local/share/rtk

# /home/opencode/.config/opencode is the mount point for the
# boop-runner-config ConfigMap (skill files). The kubelet needs
# the parent dirs to exist; the leaf dir is overlaid by the
# mount at runtime, so we only chown the parts that the runner
# actually writes to (the home itself, the /work clone target,
# the rtk tee target). The trust store + rtk config dirs are
# already chowned via the COPY --chown above.
RUN mkdir -p /home/opencode/.config/opencode /work/rtk-tee /work \
 && chown -R ubuntu:ubuntu /home/opencode /work \
 && chmod 0755 /home/opencode /home/opencode/.config /home/opencode/.config/opencode

ENV HOME=/home/opencode \
    NODE_ENV=production \
    PATH="/opt/node/bin:$PATH" \
    # QUB-85: rtk is on the read path. Telemetry is killed at
    # both layers (config.toml + env) so a future config edit
    # cannot accidentally re-enable the daily ping. Tee lands
    # inside the workdir so a failed rtk call's recovery hint
    # stays in the same namespace the pod owns.
    RTK_TELEMETRY_DISABLED=1 \
    RTK_TEE_DIR=/work/rtk-tee

# Run as the ubuntu default user (uid 1000) so /work and the
# home directory are writable for the runner.
USER ubuntu
WORKDIR /app

CMD ["node", "src/index.mjs"]
