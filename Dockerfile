# --- Build stage ---
FROM golang:alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o emly-api .

# --- Runtime stage ---
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY --from=builder /build/emly-api .
COPY --from=builder /build/internal/database/schema ./internal/database/schema
COPY --from=builder /build/internal/handlers/templates ./internal/handlers/templates
COPY docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

RUN mkdir -p /logs

EXPOSE 8080

ENTRYPOINT ["/entrypoint.sh"]