IMAGE ?= ghcr.io/michaelruelas/boop-runner:latest

# `make test` runs under Bun. The runner's local test loop is the
# hot path for the QUB-87 staged-workflow stack; Bun's test runner
# is materially faster than node --test on this suite. The CI
# image and the production pod still run on node 22; the runner
# code itself is node-compatible. `make test-node` is the escape
# hatch for the rare case Bun disagrees with node on a test.
.PHONY: install build docker-build docker-push clean test test-node

install:
	npm ci

build:
	node --check src/index.mjs

test:
	bun test src/*.test.mjs src/lib/*.test.mjs

test-node:
	node --test src/*.test.mjs src/lib/*.test.mjs

docker-build:
	docker buildx build --platform linux/arm64 -t $(IMAGE) .

docker-push: docker-build
	docker push $(IMAGE)

clean:
	rm -rf node_modules
