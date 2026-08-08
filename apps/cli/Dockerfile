FROM golang:1.26-alpine AS builder
WORKDIR /src
ARG VERSION=dev
COPY go.mod ./
RUN go mod download
COPY . .
# Vet and tests run in the CI test job natively. The builder stage
# may run under QEMU emulation for multi-arch builds, where Go's
# module indexer can segfault. Only build here.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/boop ./cmd/boop

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/boop /usr/local/bin/boop
ENTRYPOINT ["boop"]
