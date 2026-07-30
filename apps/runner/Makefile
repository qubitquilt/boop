IMAGE ?= ghcr.io/michaelruelas/boop-runner:latest

.PHONY: install build docker-build docker-push clean

install:
	npm ci

build:
	node --check src/index.mjs

docker-build:
	docker buildx build --platform linux/arm64 -t $(IMAGE) .

docker-push: docker-build
	docker push $(IMAGE)

clean:
	rm -rf node_modules
