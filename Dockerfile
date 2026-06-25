# =============================================================================
# xunfei-gateway 多阶段 Dockerfile
# Stage 1: 构建 Go 二进制（零 CGO，静态链接）
# Stage 2: 最小 alpine 运行镜像
# =============================================================================
ARG GOLANG_IMAGE=golang:1.26-alpine
ARG ALPINE_IMAGE=alpine:3.21
ARG GOPROXY=https://goproxy.cn,direct

# -----------------------------------------------------------------------------
# Stage 1: Builder
# -----------------------------------------------------------------------------
FROM ${GOLANG_IMAGE} AS builder
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
ENV CGO_ENABLED=0

WORKDIR /src

# 先拷 go.mod 利用缓存（本项目零依赖，go.sum 可能不存在）。
COPY go.mod ./
RUN go mod download

COPY . .

RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# -----------------------------------------------------------------------------
# Stage 2: Runtime
# -----------------------------------------------------------------------------
FROM ${ALPINE_IMAGE}

# 运行时依赖：ca-certificates（HTTPS）、tzdata（时区）、wget（健康检查）。
RUN apk add --no-cache ca-certificates tzdata wget && \
    addgroup -g 1000 app && \
    adduser -u 1000 -G app -s /bin/sh -D app

WORKDIR /app
COPY --from=builder /out/gateway /app/gateway

# 以非 root 用户运行。
USER app

# 默认监听 8080；可通过 LISTEN_ADDR 环境变量覆盖。
ENV LISTEN_ADDR=:8080
EXPOSE 8080

# 健康检查命中本地 /health（不消耗上游配额）。
# 注意：若改了 LISTEN_ADDR 的端口，需相应调整此处端口。
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s --retries=3 \
    CMD wget -q -T 3 -O /dev/null http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/app/gateway"]
