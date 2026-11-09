# --- Build stage ---
FROM golang:alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o emly-api .

# --- Runtime stage ---
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata bash

WORKDIR /app

COPY --from=builder /build/emly-api .
COPY --from=builder /build/internal/database/schema ./internal/database/schema
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

# The server runs unprivileged. The UID/GID are fixed, not left to adduser,
# because they end up as file ownership on the volumes and docker-compose.yml's
# volumes-init repeats them: keep the two in sync.
#
# /app stays root-owned, so the process cannot rewrite its own binary. The
# relative defaults (LOG_DIR=logs, CLIENT_EVENTS_SNAPSHOT_PATH=data/... in
# .env.example) would resolve under it, so these links send them to the volumes.
RUN addgroup -S -g 10001 emly \
    && adduser -S -D -H -u 10001 -G emly -h /app emly \
    && mkdir -p /logs /data \
    && chown emly:emly /logs /data \
    && ln -s /logs /app/logs \
    && ln -s /data /app/data

# Numeric so that orchestrators can verify it is not root without reading
# /etc/passwd (Kubernetes runAsNonRoot refuses a user name).
USER 10001:10001

EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]