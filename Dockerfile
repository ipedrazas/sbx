# syntax=docker/dockerfile:1
ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
# Build provenance. .dockerignore strips .git/, so pass these in, e.g.:
#   docker build --build-arg GIT_SHA="$(git rev-parse --short=12 HEAD)" \
#     --build-arg BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" --build-arg VERSION=0.1.10 .
ARG VERSION=0.1.10
ARG GIT_SHA=unknown
ARG BUILD_TIME=unknown
ARG TARGETOS TARGETARCH
# modernc.org/sqlite is pure Go, so the binary is fully static.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X github.com/ipedrazas/sbx/internal/version.Version=${VERSION} \
      -X github.com/ipedrazas/sbx/internal/version.GitSHA=${GIT_SHA} \
      -X github.com/ipedrazas/sbx/internal/version.BuildTime=${BUILD_TIME}" \
    -o /out/sbx ./cmd/sbx

# distroless/static ships CA certificates and tzdata. Runs as root so the data
# dir stays at /root/.dstudio, matching existing volume mounts.
FROM gcr.io/distroless/static-debian12
COPY --from=builder /out/sbx /usr/local/bin/sbx
EXPOSE 9820
ENTRYPOINT ["/usr/local/bin/sbx"]
CMD ["--host", "0.0.0.0", "--port", "9820"]
