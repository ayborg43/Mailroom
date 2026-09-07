FROM golang:1.27-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
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
