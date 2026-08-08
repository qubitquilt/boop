FROM golang:1.26-alpine AS builder
WORKDIR /src
ARG VERSION=dev
COPY go.mod ./
RUN go mod download
COPY . .
# Run vet + tests before the binary so a failing test fails the build.
RUN go vet ./... && go test ./...
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/boop ./cmd/boop

FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/boop /usr/local/bin/boop
ENTRYPOINT ["boop"]
