# === Build stage ===
FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build all binaries
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /duckway-server ./cmd/server/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /duckway-admin ./cmd/admin/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /duckway-gateway ./cmd/gateway/

# Cross-compile client for downloads
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /dist/duckway-client-linux-amd64 ./cmd/client/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags="-s -w" -o /dist/duckway-client-linux-arm64 ./cmd/client/
RUN CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w" -o /dist/duckway-client-darwin-amd64 ./cmd/client/
RUN CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w" -o /dist/duckway-client-darwin-arm64 ./cmd/client/

# === Combined server (backwards compat) ===
FROM alpine:3.21 AS server

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /duckway-server /usr/local/bin/duckway-server
COPY --from=builder /dist/ /srv/downloads/

VOLUME /data
EXPOSE 8080

ENV DUCKWAY_DATA_DIR=/data
ENV DUCKWAY_LISTEN=:8080

ENTRYPOINT ["duckway-server"]
CMD ["--data", "/data", "--port", "8080"]

# === Admin only ===
FROM alpine:3.21 AS admin

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /duckway-admin /usr/local/bin/duckway-admin

VOLUME /data
EXPOSE 9090

ENV DUCKWAY_DATA_DIR=/data
ENV DUCKWAY_ADMIN_LISTEN=:9090

ENTRYPOINT ["duckway-admin"]
CMD ["--data", "/data", "--port", "9090"]

# === Gateway only ===
FROM alpine:3.21 AS gateway

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /duckway-gateway /usr/local/bin/duckway-gateway
COPY --from=builder /dist/ /srv/downloads/

VOLUME /data
EXPOSE 8080

ENV DUCKWAY_DATA_DIR=/data
ENV DUCKWAY_GATEWAY_LISTEN=:8080

ENTRYPOINT ["duckway-gateway"]
CMD ["--data", "/data", "--port", "8080"]

# === Client ===
FROM alpine:3.21 AS client

RUN apk add --no-cache ca-certificates curl jq

COPY --from=builder /dist/duckway-client-linux-amd64 /usr/local/bin/duckway

RUN mkdir -p /root/.duckway

WORKDIR /root
CMD ["sleep", "infinity"]
