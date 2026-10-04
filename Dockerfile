# syntax=docker/dockerfile:1.7
# ==============================================================================
# BERMUDA Stealth Gateway NG — Production Hardened Multi-Stage Dockerfile
# Cloudflare CDN Protected & Edge Telemetry Semantic Environment
#
# Stage 1 (builder)    : Pure static Go 1.24 build (GOAMD64=v2)
# Stage 2 (downloader) : Multi-arch official Xray-core fetcher with SHA-256 validation
# Stage 3 (runtime)    : Minimal Alpine 3.21, true rootless immutability (UID 10001)
# ==============================================================================

ARG GO_VERSION=1.24
ARG ALPINE_VERSION=3.21
ARG XRAY_VERSION=v26.9.9

# ------------------------------------------------------------------------------
# Stage 1 — Go Gateway Static Builder
# ------------------------------------------------------------------------------
FROM golang:${GO_VERSION}-alpine${ALPINE_VERSION} AS builder

ARG TARGETARCH=amd64
ARG GOAMD64_LEVEL=v2

WORKDIR /src

# 1. Copy tracked module manifest and source files
COPY go.mod ./
COPY *.go ./

# 2. Compile static, stripped gateway binary with broad v2 vector compatibility
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) export GOARCH=amd64 GOAMD64="${GOAMD64_LEVEL}" ;; \
        arm64) export GOARCH=arm64 GOARM64=v8.0 ;; \
        *) echo "Unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    CGO_ENABLED=0 GOOS=linux \
    go build \
        -trimpath \
        -tags netgo,osusergo \
        -ldflags="-s -w -buildid=" \
        -o /out/bermuda-gateway .; \
    test -s /out/bermuda-gateway; \
    chmod 0555 /out/bermuda-gateway

# ------------------------------------------------------------------------------
# Stage 2 — Multi-Arch Official Xray-core Fetcher with Checksum Verification
# ------------------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION} AS xray-downloader

ARG XRAY_VERSION=v26.9.9
ARG TARGETARCH=amd64

WORKDIR /tmp/xray

RUN set -eux; \
    apk add --no-cache ca-certificates wget unzip; \
    case "${TARGETARCH}" in \
        amd64) XRAY_ARCH="64" ;; \
        arm64) XRAY_ARCH="arm64-v8a" ;; \
        *) echo "Unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    XRAY_ZIP="Xray-linux-${XRAY_ARCH}.zip"; \
    XRAY_BASE="https://github.com/XTLS/Xray-core/releases/download/${XRAY_VERSION}"; \
    echo "Downloading Xray-core ${XRAY_VERSION} (${XRAY_ZIP})..."; \
    wget -q "${XRAY_BASE}/${XRAY_ZIP}" -O "${XRAY_ZIP}"; \
    wget -q "${XRAY_BASE}/${XRAY_ZIP}.dgst" -O "${XRAY_ZIP}.dgst"; \
    EXPECTED_SHA=$(awk -F'= *' 'toupper($1) ~ /SHA2-256|SHA256/ {print $2; exit}' "${XRAY_ZIP}.dgst" | tr -d ' \r\n'); \
    if [ -n "${EXPECTED_SHA}" ]; then \
        echo "${EXPECTED_SHA}  ${XRAY_ZIP}" | sha256sum -c -; \
    else \
        echo "Warning: Checksum verification bypassed (no valid SHA256 digest in .dgst)" >&2; \
    fi; \
    mkdir -p /out/bin /out/assets; \
    unzip -q "${XRAY_ZIP}" xray -d /out/bin; \
    unzip -q "${XRAY_ZIP}" geoip.dat geosite.dat -d /out/assets; \
    chmod 0555 /out/bin/xray; \
    chmod 0444 /out/assets/*.dat; \
    test -s /out/bin/xray; \
    test -s /out/assets/geoip.dat; \
    test -s /out/assets/geosite.dat

# ------------------------------------------------------------------------------
# Stage 3 — Hardened Rootless Runtime (Minimal Alpine Base)
# ------------------------------------------------------------------------------
FROM alpine:${ALPINE_VERSION}

# Sanitized OCI Labels (Eliminates proxy/circumvention heuristic detection)
LABEL org.opencontainers.image.title="Edge Telemetry Gateway" \
      org.opencontainers.image.description="High-Throughput L7 Edge Stream & Metrics Gateway" \
      org.opencontainers.image.version="2.1-production" \
      org.opencontainers.image.licenses="MIT"

# 1. Install bare runtime dependencies and configure unprivileged user (UID 10001)
RUN set -eux; \
    apk add --no-cache ca-certificates tzdata wget; \
    update-ca-certificates; \
    addgroup -g 10001 -S bermuda; \
    adduser -u 10001 -S -D -H -G bermuda -h /app -s /sbin/nologin bermuda; \
    mkdir -p /app /usr/local/share/xray /usr/local/bin

# 2. Copy artifacts with root:root ownership to guarantee TRUE IMMUTABILITY at runtime.
# The non-root process (UID 10001) cannot alter, overwrite, or delete these files.
COPY --from=builder --chown=root:root /out/bermuda-gateway /usr/local/bin/bermuda-gateway
COPY --from=xray-downloader --chown=root:root /out/bin/xray /usr/local/bin/xray
COPY --from=xray-downloader --chown=root:root /out/assets/geoip.dat /usr/local/share/xray/geoip.dat
COPY --from=xray-downloader --chown=root:root /out/assets/geosite.dat /usr/local/share/xray/geosite.dat
COPY --chown=root:root config.json /app/config.json

# 3. Lock permissions: read-execute (0555) on binaries/directories, read-only (0444) on configs
RUN set -eux; \
    chown -R root:root /app /usr/local/share/xray /usr/local/bin; \
    chmod 0555 /app /usr/local/share/xray /usr/local/bin; \
    chmod 0555 /usr/local/bin/bermuda-gateway /usr/local/bin/xray; \
    chmod 0444 /app/config.json /usr/local/share/xray/geoip.dat /usr/local/share/xray/geosite.dat; \
    test -s /usr/local/bin/bermuda-gateway; \
    test -s /usr/local/bin/xray; \
    test -s /app/config.json; \
    test -s /usr/local/share/xray/geoip.dat; \
    test -s /usr/local/share/xray/geosite.dat

# 4. Standard runtime environment variables tuned for Cloudflare Edge & Railway
ENV XRAY_LOCATION_ASSET=/usr/local/share/xray \
    BERMUDA_XRAY_BIN=/usr/local/bin/xray \
    BERMUDA_XRAY_CONFIG=/app/config.json \
    BERMUDA_BACKEND_XH=127.0.0.1:18443 \
    BERMUDA_BACKEND_WS=127.0.0.1:18444 \
    BERMUDA_BACKEND_TR=127.0.0.1:18445 \
    BERMUDA_PATH_XH=/api/v1/sync \
    BERMUDA_PATH_WS=/api/v1/live \
    BERMUDA_PATH_TR=/api/v1/gateway \
    BERMUDA_HEALTH_PATH=/.well-known/hc-5b1e7c \
    GODEBUG=madvdontneed=1 \
    GOGC=100 \
    TZ=UTC \
    PORT=8080

USER 10001:10001
WORKDIR /app

# Platform dynamic port expose fallback
EXPOSE 8080

# Graceful termination signal mapping for Railway orchestration
STOPSIGNAL SIGTERM

# Container-level active healthcheck probe targeting the hardened stealth path
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
    CMD wget -q -T 3 -O /dev/null "http://127.0.0.1:${PORT:-8080}/.well-known/hc-5b1e7c" || exit 1

# PID 1 Process: The Go Gateway acts as the init supervisor and child-subreaper
CMD ["/usr/local/bin/bermuda-gateway"]
