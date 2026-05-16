FROM golang:1.26-alpine AS builder

WORKDIR /build

# Cache dependencies first
COPY go.mod go.sum ./
RUN go mod download

# Build the binary
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /build/deepseek-cursor-proxy ./cmd/deepseek-cursor-proxy

FROM alpine:3.23 AS runtime

# Install ngrok + ca-certificates for HTTPS
RUN set -eux; \
    apk add --no-cache ca-certificates; \
    arch="$(apk --print-arch)"; \
    case "$arch" in \
        x86_64)  ngrok_arch="amd64" ;; \
        aarch64) ngrok_arch="arm64" ;; \
        *) echo "Unsupported architecture: $arch"; exit 1 ;; \
    esac; \
    wget -qO- "https://bin.equinox.io/c/bNyj1mQVY4c/ngrok-v3-stable-linux-${ngrok_arch}.tgz" \
        | tar xz -C /usr/local/bin; \
    ngrok version

# Copy the Go binary
COPY --from=builder /build/deepseek-cursor-proxy /usr/local/bin/deepseek-cursor-proxy

EXPOSE 9000
EXPOSE 4040

VOLUME /data

# Copy entrypoint script
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod +x /usr/local/bin/docker-entrypoint.sh

ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["deepseek-cursor-proxy"]
