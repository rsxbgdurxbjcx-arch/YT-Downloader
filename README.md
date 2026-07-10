# YT下载器

> 基于 Go + yt-dlp + ffmpeg 的视频/音频下载器，单一二进制 + Web UI，Docker 一键部署。
> 专注 YouTube 最高画质下载，支持 4K 120帧 HDR 与 8K 视频。

![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?logo=go)
![yt-dlp](https://img.shields.io/badge/yt--dlp-latest-red)
![ffmpeg](https://img.shields.io/badge/ffmpeg-full-blue)
![Docker](https://img.shields.io/badge/Docker-ready-2496ED?logo=docker)
![License](https://img.shields.io/badge/License-MIT-green)

## 项目介绍

**YT下载器** 是一个轻量级、高性能的 Web 端视频/音频下载工具。它将 [yt-dlp](https://github.com/yt-dlp/yt-dlp) 的强大下载能力封装在一个极简的 Web 界面背后，用户只需粘贴链接、点击下载，即可获取最高画质的视频文件。

项目采用 **Go 原生 `net/http`** 实现后端，**不依赖 Nginx、不依赖 Python Web 框架**，编译后为单一二进制文件，同时托管 Web UI 静态资源与 API 接口。后端通过 `os/exec` 调用 yt-dlp，下载完成后使用 `io.Copy` 将文件流式推送到浏览器，**4K/8K 大文件下载过程中服务端内存占用恒定**（默认 32KB 缓冲区）。


## 功能特性

- **最高画质支持**：专为 YouTube 优化，支持 **4K 120帧 HDR** 与 **8K (4320p)** 视频下载
- **智能直连/代理**：可直连的媒体流由客户端直接下载（节省服务器带宽），不可直连的（HLS/DASH 分片流）由服务器代理下载
- **流式传输**：后端 `io.Copy` 流式推送，服务端内存恒定；前端使用浏览器原生下载管理器，客户端内存占用最低
- **自动缓存清理**：推送完成后立即清理临时文件，启动时清空残留，后台每 10 分钟清理过期文件
- **单一二进制**：Go 编译产物为单一可执行文件，同时托管 Web UI 与 API，零外部依赖
- **Docker 一键部署**：提供 Dockerfile 与 docker-compose.yml，`docker compose up -d` 即可上线
- **HTTPS 支持**：启动时自动生成自签证书，兼容 Cloudflare 代理回源
- **安全加固**：非 root 用户运行、tini 作为 PID 1、健康检查、临时文件自动清理

## 格式选择器

### 选择策略

```go
const formatSelector = "bestvideo+bestaudio/best"
```

| 选择器 | 说明 |
|--------|------|
| `bestvideo` | yt-dlp 自动选最高分辨率 + 最高帧率 + 最高码率的视频流 |
| `bestaudio` | yt-dlp 自动选最高音质音频流 |
| `best` | 回退（单文件，含音视频） |

**解析阶段**额外使用 `--format-sort res,fps,tbr` 排序：
- `res`：分辨率优先（8K > 4K > 1080p > 720p）
- `fps`：帧率优先（120fps > 60fps > 30fps）
- `tbr`：总码率优先（同分辨率同帧率时选最高码率）

**下载阶段**使用 `--merge-output-format mkv` 合并为 mkv 容器，支持所有编码组合（H.264/VP9/AV1 视频 + MP4A/Opus 音频），确保音视频合并成功。

### 各分辨率支持

| 分辨率 | 视频编码 | 帧率 | 音频编码 | 支持状态 |
|--------|----------|------|----------|----------|
| 720p | H.264 / VP9 / AV1 | 30fps / 60fps | Opus / MP4A | ✅ 最高画质 |
| 1080p | H.264 / VP9 / AV1 | 30fps / 60fps | Opus / MP4A | ✅ 最高画质 |
| 1080p HDR | AV1 | 60fps | Opus | ✅ 最高画质 |
| 2160p (4K) | VP9 / AV1 | 30fps / 60fps | Opus | ✅ 最高画质 |
| 2160p (4K) HDR | AV1 | 60fps | Opus | ✅ 最高画质 |
| 4320p (8K) | AV1 | 30fps / 60fps | Opus | ✅ 最高画质 |
| 4320p (8K) HDR | AV1 | 120fps | Opus | ✅ 最高画质 |

> **说明**：`bestvideo` 会自动选择该视频可用的最高分辨率 + 最高帧率 + 最高码率流。YouTube 不同视频提供的流可能不同（部分视频可能没有 8K 或 120fps），下载器会自动选该视频可用的最高画质。

## Docker 一键部署

### 前置要求

- Git
- Docker 20.10+
- Docker Compose v2+

### 快速启动

```bash
# 1. 克隆仓库
git clone https://github.com/rsxbgdurxbjcx-arch/YT-Downloader.git
cd YT-Downloader

# 2. 一键构建并启动
docker compose up -d --build

# 3. 查看日志
docker compose logs -f

# 4. 访问 Web UI
# 浏览器打开 https://<服务器IP>/
```

### 端口说明

容器内外默认均使用 **443**（HTTPS）和 **80**（HTTP→HTTPS 跳转）端口。

如需修改宿主端口（例如改为 8443），编辑 `docker-compose.yml`：

```yaml
ports:
  - "8443:443"   # HTTPS
  - "8080:80"    # HTTP 跳转
```

### HTTPS 证书

服务启动时自动生成 ECDSA P-256 自签证书（10 年有效期），兼容 Cloudflare 代理回源（SSL 模式设为 "Full"）。

证书持久化到 Docker volume `certs`，重启容器不会重新生成。

## 一键卸载 YT下载器

```bash
# 进入项目目录
cd /opt/YT-Downloader

# 1. 停止并删除容器
docker compose down

# 2. 删除 Docker 镜像
docker rmi $(docker images -q --filter "reference=*yt-downloader*") 2>/dev/null
docker rmi $(docker images -q --filter "reference=*quanpingtai-xiazaiqi*") 2>/dev/null

# 3. 删除数据卷（缓存、证书、cookies）
docker volume rm yt-downloader_ytdlp-cache yt-downloader_certs yt-downloader_cookies 2>/dev/null

# 4. 删除项目目录
cd /opt && rm -rf YT-Downloader

# 5. 清理 Docker 无用资源（可选）
docker system prune -a -f
```

## 一键更新容器内的 yt-dlp 与 ffmpeg

yt-dlp 与 ffmpeg 更新频繁（YouTube 等平台经常变更接口），建议定期更新。

### 方式一：容器内直接更新（快速）

```bash
cd /opt/YT-Downloader

# 更新 yt-dlp 到最新版
docker compose exec downloader yt-dlp -U

# 更新 ffmpeg
docker compose exec downloader bash -c "apt-get update && apt-get install -y --only-upgrade ffmpeg"

# 重启容器使更新生效
docker compose restart

# 验证版本
docker compose exec downloader yt-dlp --version
docker compose exec downloader ffmpeg -version | head -1
```

### 方式二：重建镜像（推荐，确保镜像可复现）

```bash
cd /opt/YT-Downloader

# 拉取最新代码并重建镜像（不用缓存）
git pull origin main
docker compose build --no-cache

# 重启服务
docker compose up -d

# 验证版本
docker compose exec downloader yt-dlp --version
docker compose exec downloader ffmpeg -version | head -1
```

### 方式三：定时自动更新（可选）

创建更新脚本 `/opt/yt-update.sh`：

```bash
#!/bin/bash
cd /opt/YT-Downloader
docker compose exec downloader yt-dlp -U
docker compose exec downloader bash -c "apt-get update && apt-get install -y --only-upgrade ffmpeg"
docker compose restart
echo "$(date): yt-dlp 和 ffmpeg 已更新" >> /var/log/yt-update.log
```

添加到 crontab（每天凌晨 3 点自动更新）：

```bash
chmod +x /opt/yt-update.sh
crontab -e
# 添加以下行：
0 3 * * * /opt/yt-update.sh >> /var/log/yt-update.log 2>&1
```

## 磁盘清理机制

### 缓存路径

| 路径 | 说明 |
|------|------|
| `/tmp/ytdlp-downloads/` | 临时下载目录（容器内） |

所有下载的视频文件统一存放在 `/tmp/ytdlp-downloads/` 同一个文件夹内，下载完成后立即清理。

### 四层清理保障

| 时机 | 触发条件 | 行为 | 日志示例 |
|------|----------|------|----------|
| **推送完成后** | 每次下载推送完成 | `cleanAllFiles` 清理目录内所有文件 | `✅ 临时文件已清理: xxx.mkv (释放 * MB)` |
| **下载失败时** | yt-dlp 下载失败 | `cleanAllFiles` 清理目录内所有文件 | `下载失败，已清理临时文件 (释放 * MB)` |
| **服务启动时** | 容器重启 | `cleanStartupCache` 清空所有残留文件 | `启动清理: 已清理 2 个残留文件 (释放 * MB)` |
| **后台定时** | 每 10 分钟 | `cleanupTempFiles` 清理超过 30 分钟的残留文件 | `已清理过期临时文件: xxx.mkv (释放 * MB, 存在 *m*s)` |

### 清理流程

```
下载前: /tmp/ytdlp-downloads/
        └── (空目录)

下载中: /tmp/ytdlp-downloads/
        ├── video.f137.mp4      ← 分离视频流
        ├── video.f251.webm     ← 分离音频流
        └── video.mkv           ← 合并后的最终文件

推送完成后: /tmp/ytdlp-downloads/
            └── (空目录，所有文件已清理)
               ↑ 目录本身保留，供下次下载使用
```

### 手动清理命令

```bash
# 查看缓存目录大小（应为 0 或很小）
docker compose exec downloader du -sh /tmp/ytdlp-downloads/

# 查看残留文件（应为空）
docker compose exec downloader ls -la /tmp/ytdlp-downloads/

# 手动清理缓存
docker compose exec downloader rm -rf /tmp/ytdlp-downloads/*

# 清理 Docker 无用镜像/层（释放宿主机磁盘）
docker system prune -a -f
```

## 技术栈

| 组件 | 技术 | 说明 |
|------|------|------|
| 后端 | Go 1.23+ / `net/http` | 原生 HTTP 服务，无 Web 框架 |
| 下载引擎 | yt-dlp | 支持 1000+ 视频网站 |
| 音视频合并 | ffmpeg | 完整版，含 x264/x265/av1 编解码器 |
| 前端 | 原生 HTML/CSS/JS | 樱花飘落动画 + 自定义字体 |
| 容器 | Docker / Docker Compose | 多阶段构建，非 root 运行 |
| HTTPS | Go crypto/tls | 自签 ECDSA 证书，兼容 Cloudflare |

## API 接口

| 接口 | 方法 | 说明 |
|------|------|------|
| `/` | GET | Web UI 首页 |
| `/api/parse` | POST | 解析链接，返回 JSON（直链/格式信息） |
| `/api/download` | POST | 代理下载，返回文件流 |
| `/api/health` | GET | 健康检查 |

## 本地开发

### 环境要求

- Go 1.21+
- Python 3.8+（用于运行 yt-dlp）
- ffmpeg（完整版）
- yt-dlp

### 编译运行

```bash
# 编译
go build -o yt-downloader .

# 运行（默认监听 443/80，需 root 权限）
sudo ./yt-downloader

# 或使用非特权端口
HTTPS_PORT=8443 HTTP_PORT=8081 ./yt-downloader
```

## 常见问题

### Q: 下载的视频画质不是最高？

本项目使用 `bestvideo+bestaudio` 格式选择器，自动选最高分辨率+最高帧率+最高码率流。如果画质异常，请更新 yt-dlp：`docker compose exec downloader yt-dlp -U`。

### Q: 下载的视频没有声音？

本项目使用 `bestvideo+bestaudio` 分离流 + ffmpeg 合并的方式，输出 mkv 容器确保音视频合并成功。若仍无声音，请检查 ffmpeg 是否正确安装。

### Q: 如何修改监听端口？

通过环境变量修改：
- `HTTPS_PORT`：HTTPS 端口（默认 443）
- `HTTP_PORT`：HTTP 端口（默认 80）

### Q: 磁盘占用大？

缓存会自动清理（推送完成后立即清理 + 启动时清理 + 后台定时清理）。如需手动清理：
```bash
docker compose exec downloader rm -rf /tmp/ytdlp-downloads/*
docker system prune -a -f
```

## 许可证

本项目基于 MIT 许可证开源。yt-dlp 与 ffmpeg 遵循各自的开源协议。

## 致谢

- [yt-dlp](https://github.com/yt-dlp/yt-dlp) - 强大的视频下载工具
- [ffmpeg](https://ffmpeg.org/) - 多媒体处理瑞士军刀
- [Go](https://go.dev/) - 简洁高效的编程语言
