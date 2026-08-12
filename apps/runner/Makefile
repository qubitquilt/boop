IMAGE ?= ghcr.io/michaelruelas/boop-runner:latest

# QUB-136: the local dev loop runs under bun. The QUB-87 staged-workflow
# stack landed on bun because the test runner is materially faster
# than node --test on this suite; the migration to TypeScript kept
# bun for the test + dev + local-run path, and tsc (the only CPU-bound
# tool we ship) runs through `bun x tsc` for the same reason
# (~2x faster cold start than `npx tsc`). Production goes through
# `tsc → dist/index.js`; the local loop bypasses the build for
# iteration speed. The CI image and the production pod still run on
# node 22; the runner code itself is node-compatible, and `make
# test-node` runs the same suite under `node --test
# --experimental-strip-types` so a divergence between the two
# runtimes surfaces in CI before it surfaces in production.
.PHONY: install typecheck build local-run docker-build docker-push clean test test-node

install:
	bun install

typecheck:
	bun x tsc --noEmit

build:
	bun x tsc

# local-run: the macOS dev loop. Stage the skill mount + repo per
# apps/runner/.env.local.example, fill in OPENROUTER_API_KEY +
# BOOP_GITHUB_APP_PRIVATE_KEY_PATH, then `make local-run`. The
# wrapper verifies the mount + repo head SHA before invoking the
# runner so a missing piece aborts fast with a clear message
# instead of dying in the sniff stage. bun runs the .ts source
# directly — no tsx, no `npm run` indirection.
local-run:
	bun run local-run

test:
	bun test src/*.test.ts src/lib/*.test.ts src/lib/**/*.test.ts

test-node:
	node --test --experimental-strip-types --no-warnings src/*.test.ts src/lib/*.test.ts src/lib/**/*.test.ts

docker-build:
	docker buildx build --platform linux/arm64 -t $(IMAGE) .

docker-push: docker-build
	docker push $(IMAGE)

clean:
	rm -rf node_modules dist
