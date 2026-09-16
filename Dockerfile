# syntax=docker/dockerfile:1

# ---------------------------------------------------------------------------
# Stage 1: builder
# ---------------------------------------------------------------------------
FROM golang:1.26-alpine AS builder

WORKDIR /src

# Dependencies live in their own layer: as long as go.mod/go.sum do not change,
# this layer is reused from the cache and rebuilds are much faster.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o /out/wallet-api ./cmd/api

# ---------------------------------------------------------------------------
# Stage 2: runtime (distroless)
# ---------------------------------------------------------------------------
# distroless/static is preferred over "scratch" because it already ships CA
# certificates, tzdata, and a non-root user - everything this service needs,
# without adding a shell or a package manager (the attack surface stays minimal).
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/wallet-api /wallet-api
WORKDIR /
USER nonroot:nonroot

EXPOSE 8080

# The binary also exposes a `healthcheck` subcommand, so the health check works
# in an image that has no shell and no curl.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/wallet-api", "healthcheck"]

ENTRYPOINT ["/wallet-api"]
