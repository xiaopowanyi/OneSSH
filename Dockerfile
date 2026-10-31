FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package*.json ./
RUN if [ -f package.json ]; then npm ci; fi
COPY web/ ./
RUN --mount=type=cache,target=/root/.npm \
    if [ -f package.json ]; then npm run build; else mkdir -p dist && printf '<!doctype html><title>OneSSH</title>' > dist/index.html; fi

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
WORKDIR /src
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN CGO_ENABLED=0 go run ./internal/searchx/helper/genassets
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/onessh ./cmd/onessh

FROM alpine:3.22
LABEL org.opencontainers.image.source="https://github.com/Lynricsy/OneSSH" \
      org.opencontainers.image.description="Centralized SSH gateway with WebUI and MCP"
RUN apk add --no-cache ca-certificates tzdata \
    && addgroup -S -g 10001 onessh \
    && adduser -S -D -u 10001 -G onessh onessh \
    && mkdir -p /data \
    && chown -R 10001:10001 /data
COPY --from=build /out/onessh /usr/local/bin/onessh
VOLUME ["/data"]
EXPOSE 8866
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:8866/healthz || exit 1
# 以非 root 运行。用 bind mount 挂载 /data 的既有部署，需先确认宿主机目录对 uid 10001 可写。
USER 10001:10001
ENTRYPOINT ["onessh"]
