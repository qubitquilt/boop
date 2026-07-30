# syntax=docker/dockerfile:1.7
FROM ubuntu:24.04

# The opencode-ai binary is built against newer glibc (Ubuntu 26.04
# with 2.43). Debian 12's glibc 2.36 is the minimum the binary
# advertises, but in practice the binary deadlocks on it. Ubuntu 24.04
# ships glibc 2.39 which matches the binary's runtime needs.

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    git ca-certificates curl gnupg util-linux \
 && rm -rf /var/lib/apt/lists/*

# Install Node 22 from Nodesource (matches what the opencode-ai
# npm package was tested against).
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app
# Copy both manifest and lockfile so `npm ci` can resolve the
# exact versions the repo tested against. Without the lockfile
# `npm ci` refuses to run; using `npm install` instead would let
# a new caret-range patch drift the opencode-ai precompiled
# binary on every build (H4 in the security review).
COPY package.json package-lock.json ./
RUN npm ci --os=linux --cpu=arm64

# Symlink the opencode binary into /usr/local/bin so the runner can
# find it (it shells out to `opencode run` under the hood).
RUN ln -sf /app/node_modules/.bin/opencode /usr/local/bin/opencode

COPY src/ ./src/

ENV HOME=/home/opencode \
    NODE_ENV=production

RUN mkdir -p /home/opencode/.config/opencode /work \
 && chown -R ubuntu:ubuntu /home/opencode /work

# Run as the ubuntu default user (uid 1000) so opencode-ai's postinstall
# has a writable HOME for its setup hooks.
USER ubuntu

CMD ["node", "src/index.mjs"]
