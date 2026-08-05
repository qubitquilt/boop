# syntax=docker/dockerfile:1.7
FROM ubuntu:24.04

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

WORKDIR /app
# Copy both manifest and lockfile so `npm ci` resolves the exact
# versions the repo tested against. Without the lockfile `npm ci`
# refuses to run; using `npm install` instead would let a new
# caret-range patch drift a dep on every build.
COPY package.json package-lock.json ./
RUN npm ci --os=linux --cpu=arm64

COPY src/ ./src/

ENV HOME=/home/opencode \
    NODE_ENV=production

# /home/opencode/.config/opencode is the mount point for the
# boop-runner-config ConfigMap (skill files). The kubelet needs
# the parent dirs to exist; the leaf dir is overlaid by the
# mount at runtime, so we only chown the parts that the runner
# actually writes to (the home itself, the /work clone target).
# Chowning the mount target is wasted work — the chown gets
# masked by the mount.
RUN mkdir -p /home/opencode/.config/opencode /work \
 && chown ubuntu:ubuntu /home/opencode /work

# Run as the ubuntu default user (uid 1000) so /work and the
# home directory are writable for the runner.
USER ubuntu

CMD ["node", "src/index.mjs"]
