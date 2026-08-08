FROM golang:1.26-alpine AS builder
WORKDIR /src
ARG VERSION=dev
COPY go.mod ./
RUN go mod download
COPY . .
# Run vet before the binary so a failing vet fails the build.
# Tests run in the CI test job natively; skip here because the
# multi-arch builder stage may run under QEMU emulation where
# net/http/httptest can produce runtime panics.
RUN go vet ./...
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/boop ./cmd/boop

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/boop /usr/local/bin/boop
ENTRYPOINT ["boop"]
