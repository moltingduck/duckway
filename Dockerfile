# === Build stage ===
FROM golang:1.25-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /duckway-server ./cmd/server/
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /duckway-client ./cmd/client/

# === Server image ===
FROM alpine:3.21 AS server

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /duckway-server /usr/local/bin/duckway-server

VOLUME /data
EXPOSE 8080

ENV DUCKWAY_DATA_DIR=/data
ENV DUCKWAY_LISTEN=:8080

ENTRYPOINT ["duckway-server"]
CMD ["--data", "/data", "--port", "8080"]

# === Client image ===
FROM alpine:3.21 AS client

RUN apk add --no-cache ca-certificates curl jq

COPY --from=builder /duckway-client /usr/local/bin/duckway

RUN mkdir -p /root/.duckway

WORKDIR /root
CMD ["sleep", "infinity"]
