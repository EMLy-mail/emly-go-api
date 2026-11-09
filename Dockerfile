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

RUN addgroup -S -g 10001 emly \
    && adduser -S -D -H -u 10001 -G emly -h /app emly \
    && mkdir -p /logs /data \
    && chown emly:emly /logs /data \
    && ln -s /logs /app/logs \
    && ln -s /data /app/data

USER 10001:10001

EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]