IMAGE ?= ghcr.io/michaelruelas/boop-receiver:latest
GO ?= go

.PHONY: build test vet tidy run docker-build docker-push clean

build:
	$(GO) build -trimpath -o bin/receiver ./cmd/receiver

test:
	$(GO) test ./...

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

run: build
	./bin/receiver

docker-build:
	docker build -t $(IMAGE) .

docker-push: docker-build
	docker push $(IMAGE)

clean:
	rm -rf bin
