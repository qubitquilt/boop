# syntax=docker/dockerfile:1.7
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/receiver ./cmd/receiver

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/receiver /usr/local/bin/receiver
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/receiver"]
