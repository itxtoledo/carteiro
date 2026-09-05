# syntax=docker/dockerfile:1
# UI stage: build the React dashboard. Its output (web/dist) is embedded into
# the Go binary, so the final image has no Node.js at all.
FROM node:22-alpine AS ui
WORKDIR /ui
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# Build stage: static binary (CGO disabled) so it runs on any architecture
# (amd64/arm64) with no runtime dependencies. web/fs.go embeds web/dist, which
# is copied from the UI stage before the compile.
FROM golang:1.26-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY web/fs.go ./web/
COPY --from=ui /ui/dist ./web/dist

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/carteiro ./cmd/carteiro

# Final stage: minimal image with root CA (required for DNS/TLS) and timezone.
FROM alpine:3.20
# The relay runs as root inside the container so it can bind the SMTP
# submission port 587 (ports below 1024) and write to mounted volumes
# regardless of their ownership.
RUN apk add --no-cache ca-certificates tzdata busybox-extras \
    && mkdir -p /var/lib/carteiro

COPY --from=build /out/carteiro /usr/local/bin/carteiro

WORKDIR /var/lib/carteiro

# Database + queue (persisted via volume).
VOLUME ["/var/lib/carteiro"]

EXPOSE 587
# Web dashboard + admin API (one listener; needs CARTEIRO_API_TOKEN).
EXPOSE 8080

# Health: TCP probe against the SMTP listener (always up). For a deeper
# probe, point a healthcheck at GET /health of the web/api listener instead.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3  CMD nc -z 127.0.0.1 587 || exit 1

ENTRYPOINT ["/usr/local/bin/carteiro"]
CMD []
