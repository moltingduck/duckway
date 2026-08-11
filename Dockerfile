# syntax=docker/dockerfile:1.7

# === Build base ===
FROM golang:1.25-alpine AS build-base

# Optional version string passed in via:
#   docker build --build-arg DUCKWAY_VERSION=$(git describe --always --dirty) ...
# Falls back to "docker" if not set so binaries still report something useful.
ARG DUCKWAY_VERSION=docker

WORKDIR /build
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ENV LDFLAGS="-s -w -X github.com/hackerduck/duckway/internal/version.Embedded=${DUCKWAY_VERSION}"

# Build binaries in target-specific stages so admin-only builds do not also
# cross-compile client download artifacts.
FROM build-base AS server-bin

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -buildvcs=false -ldflags="$LDFLAGS" \
      -o /out/duckway-server ./cmd/server/

FROM build-base AS admin-bin

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -buildvcs=false -ldflags="$LDFLAGS" \
      -o /out/duckway-admin ./cmd/admin/

FROM build-base AS gateway-bin

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -buildvcs=false -ldflags="$LDFLAGS" \
      -o /out/duckway-gateway ./cmd/gateway/

FROM build-base AS client-dist

# Cross-compile client for downloads
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -buildvcs=false -ldflags="$LDFLAGS" -o /dist/duckway-client-linux-amd64 ./cmd/client/ && \
    CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -buildvcs=false -ldflags="$LDFLAGS" -o /dist/duckway-client-linux-arm64 ./cmd/client/ && \
    CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -buildvcs=false -ldflags="$LDFLAGS" -o /dist/duckway-client-darwin-amd64 ./cmd/client/ && \
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -buildvcs=false -ldflags="$LDFLAGS" -o /dist/duckway-client-darwin-arm64 ./cmd/client/ && \
    CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -buildvcs=false -ldflags="$LDFLAGS" -o /dist/ducklion-linux-amd64 ./cmd/ducklion/ && \
    CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -buildvcs=false -ldflags="$LDFLAGS" -o /dist/ducklion-linux-arm64 ./cmd/ducklion/ && \
    CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -buildvcs=false -ldflags="$LDFLAGS" -o /dist/ducklion-darwin-amd64 ./cmd/ducklion/ && \
    CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -buildvcs=false -ldflags="$LDFLAGS" -o /dist/ducklion-darwin-arm64 ./cmd/ducklion/

# === Combined server (backwards compat) ===
FROM alpine:3.21 AS server

RUN apk add --no-cache ca-certificates tzdata

COPY --from=server-bin /out/duckway-server /usr/local/bin/duckway-server
COPY --from=client-dist /dist/ /srv/downloads/

VOLUME /data
EXPOSE 8080

ENV DUCKWAY_DATA_DIR=/data
ENV DUCKWAY_LISTEN=:8080

ENTRYPOINT ["duckway-server"]
CMD ["--data", "/data", "--port", "8080"]

# === Admin only ===
FROM alpine:3.21 AS admin

RUN apk add --no-cache ca-certificates tzdata

COPY --from=admin-bin /out/duckway-admin /usr/local/bin/duckway-admin

VOLUME /data
EXPOSE 9090

ENV DUCKWAY_DATA_DIR=/data
ENV DUCKWAY_ADMIN_LISTEN=:9090

ENTRYPOINT ["duckway-admin"]
CMD ["--data", "/data", "--port", "9090"]

# === Gateway only ===
FROM alpine:3.21 AS gateway

RUN apk add --no-cache ca-certificates tzdata

COPY --from=gateway-bin /out/duckway-gateway /usr/local/bin/duckway-gateway
COPY --from=client-dist /dist/ /srv/downloads/

VOLUME /data
EXPOSE 8080

ENV DUCKWAY_DATA_DIR=/data
ENV DUCKWAY_GATEWAY_LISTEN=:8080

ENTRYPOINT ["duckway-gateway"]
CMD ["--data", "/data", "--port", "8080"]

# === Client ===
FROM alpine:3.21 AS client

RUN apk add --no-cache ca-certificates curl jq

COPY --from=client-dist /dist/duckway-client-linux-amd64 /usr/local/bin/duckway

RUN mkdir -p /root/.duckway

WORKDIR /root
CMD ["sleep", "infinity"]
