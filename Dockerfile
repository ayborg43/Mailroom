# Stage 1: build the webmail frontend (Vite) into static/dist, which the Go
# binary embeds.
FROM node:24-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# vite.config.ts writes to ../internal/webmail/static/dist
RUN npm run build

# Stage 2: build the Go binary.
FROM golang:1.27-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
COPY --from=web /src/internal/webmail/static/dist ./internal/webmail/static/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mailserver ./cmd/mailserver

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /out/mailserver /usr/local/bin/mailserver

VOLUME ["/data"]
# Runs as root: binding port 25 requires it (or CAP_NET_BIND_SERVICE), and
# that's the simplest option for a container whose only job is running this
# one trusted binary.
ENTRYPOINT ["mailserver"]
CMD ["serve", "--config", "/etc/mailserver/config.yaml"]
