IMAGE ?= ghcr.io/qubitquilt/boop-cli:latest
GO ?= go

.PHONY: build test vet tidy install docker-build docker-push clean

build:
	$(GO) build -trimpath -ldflags "-X main.version=$$(git -C ../.. describe --tags --always --dirty 2>/dev/null || echo dev)" -o bin/boop ./cmd/boop

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

install: build
	cp bin/boop $$(go env GOPATH)/bin/

docker-build:
	docker build -t $(IMAGE) .

docker-push: docker-build
	docker push $(IMAGE)

clean:
	rm -rf bin
