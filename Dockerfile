# ============================================================
# 全平台下载器 - 多阶段 Dockerfile
# 阶段 1: 使用 golang 镜像编译 Go 二进制
# 阶段 2: 使用 python 镜像运行（yt-dlp 依赖 Python），并安装完整版 ffmpeg
# ============================================================

# ---------- 阶段 1: 编译 Go 后端 ----------
FROM golang:1.23-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

COPY go.mod ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=docker" \
    -o downloader .

# ---------- 阶段 2: 运行时镜像 ----------
FROM python:3.12-slim

ENV TZ=Asia/Shanghai \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# 安装完整版 ffmpeg（含 x264/x265/av1/libvpx 等编解码器，4K 合并必需）
# 同时安装 ca-certificates、curl（健康检查）、tini（PID 1 信号处理）
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        ffmpeg \
        ca-certificates \
        curl \
        tini \
        && \
    rm -rf /var/lib/apt/lists/*

# 安装最新版 yt-dlp（通过 pip）
RUN pip install --upgrade pip && \
    pip install --upgrade yt-dlp

# 创建非 root 用户运行（安全最佳实践）
RUN useradd -m -u 1000 -s /bin/sh appuser

WORKDIR /app

# 从 builder 阶段复制编译产物
COPY --from=builder /build/downloader /app/downloader

# 复制静态资源
COPY static/ /app/static/

# 创建临时下载目录、证书目录、cookies 目录并赋权
RUN mkdir -p /tmp/ytdlp-downloads /app/certs /app/cookies && \
    chown -R appuser:appuser /app /tmp/ytdlp-downloads

# 切换到非 root 用户
USER appuser

# 暴露 443(HTTPS) 与 80(HTTP跳转) 端口
EXPOSE 443 80

# 健康检查（使用 HTTPS，忽略自签证书警告）
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD curl -fsSk https://localhost:443/api/health || exit 1

# 使用 tini 作为 PID 1，正确处理信号
ENTRYPOINT ["/usr/bin/tini", "--"]

# 启动全平台下载器
CMD ["/app/downloader"]
