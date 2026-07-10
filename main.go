// Package main 实现全平台下载器后端服务
//
// 该服务基于 Go 原生 net/http 实现，不依赖任何 Web 框架。
// 核心能力：
//   1. 托管 Web UI 静态资源（HTML / 字体 / 背景图 / JS）
//   2. 通过 yt-dlp 解析任意平台链接，提取媒体直链与格式信息
//   3. 优先让客户端直连下载（节省服务器带宽），不可直连时回退到服务器代理
//   4. 服务器代理下载时用 io.Copy 流式推送，内存占用恒定
//   5. 同时监听 443(HTTPS) 与 80(HTTP->HTTPS跳转) 端口
//   6. 启动时自动生成自签证书，兼容 Cloudflare 代理回源
package main

import (
        "bytes"
        "crypto/ecdsa"
        "crypto/elliptic"
        "crypto/rand"
        "crypto/tls"
        "crypto/x509"
        "crypto/x509/pkix"
        "encoding/json"
        "encoding/pem"
        "fmt"
        "io"
        "log"
        "math/big"
        "net"
        "net/http"
        "os"
        "os/exec"
        "path/filepath"
        "strings"
        "time"
)

// ====== 全局配置 ======

var httpsAddr = ":443"
var httpAddr = ":80"
var staticDir = "./static"
var tempRoot = "/tmp/ytdlp-downloads"
var certDir = "/app/certs"

const ytdlpBinary = "yt-dlp"
const ffmpegBinary = "ffmpeg"

// formatSelector yt-dlp 格式选择器
//
// 设计目标：保证下载最高画质、最高音质、最高帧率。
//
// 选择策略：
//   -f bestvideo+bestaudio/best
//   --format-sort res,fps,tbr
//
// 原理说明：
//   1. bestvideo+bestaudio：选择最高画质视频流 + 最高音质音频流，由 ffmpeg 合并
//   2. --format-sort res,fps,tbr：强制 yt-dlp 按以下优先级排序选择流
//      - res（分辨率）：优先最高分辨率，覆盖 8K/4K/1080p/720p
//      - fps（帧率）：优先最高帧率，覆盖 120fps/60fps/30fps
//      - tbr（总码率）：同分辨率同帧率时，优先最高码率流
//        → YouTube 4K 视频通常有 H.264(~15Mbps) / VP9(~40Mbps) / AV1(~25Mbps) 多个流
//        → 按 tbr 排序后，VP9 流码率最高会被选中（与其他下载器一致，475MB+）
//   3. --merge-output-format mkv：合并为 mkv 容器，支持所有编码组合
//
// 为什么不用 [vcodec*=vp09] 过滤？
//   - 某些视频可能没有 VP9 流，强制过滤会导致匹配失败回退到低画质
//   - 用 --format-sort tbr 更可靠，无论什么编码都选最高码率（即最高画质）
//   - VP9/AV1 码率通常高于 H.264，按 tbr 排序自然优先选 VP9/AV1
//
const formatSelector = "bestvideo+bestaudio/best"

// downloadCounter 已废弃：所有下载统一存放在 tempRoot，不再需要唯一子目录

// ====== 主入口 ======

func main() {
        if port := os.Getenv("HTTPS_PORT"); port != "" {
                httpsAddr = ":" + strings.TrimPrefix(port, ":")
        }
        if port := os.Getenv("HTTP_PORT"); port != "" {
                httpAddr = ":" + strings.TrimPrefix(port, ":")
        }
        if port := os.Getenv("PORT"); port != "" && os.Getenv("HTTPS_PORT") == "" {
                httpsAddr = ":" + strings.TrimPrefix(port, ":")
        }
        if dir := os.Getenv("STATIC_DIR"); dir != "" {
                staticDir = dir
        }
        if dir := os.Getenv("TEMP_DIR"); dir != "" {
                tempRoot = dir
        }
        if dir := os.Getenv("CERT_DIR"); dir != "" {
                certDir = dir
        }

        if err := os.MkdirAll(tempRoot, 0o755); err != nil {
                log.Fatalf("无法创建临时目录 %s: %v", tempRoot, err)
        }

        // 启动时清理上次运行残留的临时文件（防止异常退出导致磁盘堆积）
        cleanStartupCache()

        checkDependencies()

        mux := http.NewServeMux()
        mux.HandleFunc("/", handleRoot)
        mux.HandleFunc("/api/parse", handleParse)         // 解析接口：返回 JSON
        mux.HandleFunc("/api/download", handleDownload)   // 代理下载接口：返回文件流
        mux.HandleFunc("/api/health", handleHealth)

        // 后台清理：每 10 分钟检查一次，清理超过 30 分钟的残留临时文件
        go cleanupTempFiles(10*time.Minute, 30*time.Minute)

        cert, err := ensureSelfSignedCert()
        if err != nil {
                log.Fatalf("生成自签证书失败: %v", err)
        }

        log.Printf("YT下载器服务已启动")
        log.Printf("HTTPS 监听地址: %s", httpsAddr)
        log.Printf("HTTP  监听地址: %s (自动跳转 HTTPS)", httpAddr)
        log.Printf("静态资源目录: %s", staticDir)
        log.Printf("临时下载目录: %s", tempRoot)

        go func() {
                httpServer := &http.Server{
                        Addr:              httpAddr,
                        Handler:           http.HandlerFunc(redirectToHTTPS),
                        ReadHeaderTimeout: 10 * time.Second,
                }
                if err := httpServer.ListenAndServe(); err != nil {
                        log.Printf("HTTP 跳转服务启动失败(端口 %s): %v", httpAddr, err)
                }
        }()

        httpsServer := &http.Server{
                Addr:              httpsAddr,
                Handler:           mux,
                ReadHeaderTimeout: 30 * time.Second,
                TLSConfig: &tls.Config{
                        MinVersion:   tls.VersionTLS12,
                        Certificates: []tls.Certificate{cert},
                },
        }

        if err := httpsServer.ListenAndServeTLS("", ""); err != nil {
                log.Fatalf("HTTPS 服务启动失败: %v", err)
        }
}

func redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
        if r.URL.Path == "/api/health" {
                handleHealth(w, r)
                return
        }
        host := r.Host
        if host == "" {
                host = r.URL.Host
        }
        if h, _, err := net.SplitHostPort(host); err == nil {
                host = h
        }
        target := "https://" + host + r.URL.RequestURI()
        http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// ====== 证书生成 ======

func ensureSelfSignedCert() (tls.Certificate, error) {
        if err := os.MkdirAll(certDir, 0o755); err != nil {
                return tls.Certificate{}, fmt.Errorf("创建证书目录失败: %w", err)
        }
        certFile := filepath.Join(certDir, "server.crt")
        keyFile := filepath.Join(certDir, "server.key")
        if _, err1 := os.Stat(certFile); err1 == nil {
                if _, err2 := os.Stat(keyFile); err2 == nil {
                        cert, err := tls.LoadX509KeyPair(certFile, keyFile)
                        if err == nil {
                                log.Printf("已加载现有自签证书: %s", certFile)
                                return cert, nil
                        }
                }
        }
        log.Printf("正在生成新的自签证书...")
        certPEM, keyPEM, err := generateSelfSignedCert()
        if err != nil {
                return tls.Certificate{}, fmt.Errorf("生成证书失败: %w", err)
        }
        if err := os.WriteFile(certFile, certPEM, 0o644); err != nil {
                return tls.Certificate{}, fmt.Errorf("写入证书文件失败: %w", err)
        }
        if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
                return tls.Certificate{}, fmt.Errorf("写入私钥文件失败: %w", err)
        }
        log.Printf("自签证书已生成: %s", certFile)
        cert, err := tls.X509KeyPair(certPEM, keyPEM)
        if err != nil {
                return tls.Certificate{}, fmt.Errorf("加载生成的证书失败: %w", err)
        }
        return cert, nil
}

func generateSelfSignedCert() ([]byte, []byte, error) {
        priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
        if err != nil {
                return nil, nil, err
        }
        serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
        if err != nil {
                return nil, nil, err
        }
        template := x509.Certificate{
                SerialNumber: serialNumber,
                Subject: pkix.Name{
                        Organization: []string{"QuanPingTai XiaZaiQi"},
                        CommonName:   "localhost",
                },
                NotBefore:             time.Now().Add(-time.Hour),
                NotAfter:              time.Now().AddDate(10, 0, 0),
                KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
                ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
                BasicConstraintsValid: true,
                DNSNames:              []string{"localhost"},
                IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1), net.IPv4(0, 0, 0, 0), net.ParseIP("::1")},
        }
        certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
        if err != nil {
                return nil, nil, err
        }
        certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
        keyDER, err := x509.MarshalECPrivateKey(priv)
        if err != nil {
                return nil, nil, err
        }
        keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
        return certPEM, keyPEM, nil
}

func checkDependencies() {
        if _, err := exec.LookPath(ytdlpBinary); err != nil {
                log.Printf("警告: 未在 PATH 中找到 %s", ytdlpBinary)
        }
        if _, err := exec.LookPath(ffmpegBinary); err != nil {
                log.Printf("警告: 未在 PATH 中找到 %s，音视频合并将失败", ffmpegBinary)
        }
}

// ====== 路由处理 ======

func handleRoot(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodGet {
                http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
                return
        }
        if r.URL.Path == "/" {
                serveIndex(w, r)
                return
        }
        relPath := strings.TrimPrefix(r.URL.Path, "/")
        cleanPath := filepath.Clean(relPath)
        if cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) || filepath.IsAbs(cleanPath) {
                http.NotFound(w, r)
                return
        }
        target := filepath.Join(staticDir, cleanPath)
        info, err := os.Stat(target)
        if err != nil || info.IsDir() {
                http.NotFound(w, r)
                return
        }
        http.ServeFile(w, r, target)
}

// serveIndex 读取 index.html 并在 </body> 前注入 app.js 脚本引用
// 用 rune 码点拼接标签，杜绝字面量被剥离的问题
func serveIndex(w http.ResponseWriter, r *http.Request) {
        content, err := os.ReadFile(filepath.Join(staticDir, "index.html"))
        if err != nil {
                log.Printf("读取 index.html 失败: %v", err)
                http.Error(w, "Internal Server Error", http.StatusInternalServerError)
                return
        }
        lt := string(rune(60)) // <
        gt := string(rune(62)) // >
        injectTag := lt + "script src=\"/app.js\"" + gt + lt + "/script" + gt
        bodyCloseTag := lt + "/body" + gt
        html := string(content)
        if !strings.Contains(html, injectTag) {
                html = strings.Replace(html, bodyCloseTag, injectTag+bodyCloseTag, 1)
        }
        w.Header().Set("Content-Type", "text/html; charset=utf-8")
        w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
        w.Header().Set("X-Content-Type-Options", "nosniff")
        if _, err := io.WriteString(w, html); err != nil {
                log.Printf("写入 index.html 响应失败: %v", err)
        }
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
        w.Header().Set("Content-Type", "application/json")
        w.Write([]byte(`{"status":"ok","service":"yt-downloader"}`))
}

// ====== 数据结构 ======

// ParseRequest 解析请求
type ParseRequest struct {
        URL string `json:"url"`
}

// ParseResponse 解析响应
type ParseResponse struct {
        Success          bool              `json:"success"`
        Title            string            `json:"title,omitempty"`
        Extractor        string            `json:"extractor,omitempty"`
        ExtractorKey     string            `json:"extractor_key,omitempty"`
        WebpageURL       string            `json:"webpage_url,omitempty"`
        Format           string            `json:"format,omitempty"`
        Resolution       string            `json:"resolution,omitempty"`
        Ext              string            `json:"ext,omitempty"`
        DirectURL        string            `json:"direct_url,omitempty"`
        Headers          map[string]string `json:"headers,omitempty"`
        FallbackRequired bool              `json:"fallback_required"`
        Filename         string            `json:"filename,omitempty"`
        Error            string            `json:"error,omitempty"`
}

// ytdlpInfo yt-dlp -J 输出的关键字段
type ytdlpInfo struct {
        Title        string `json:"title"`
        Extractor    string `json:"extractor"`
        ExtractorKey string `json:"extractor_key"`
        WebpageURL   string `json:"webpage_url"`
        Ext          string `json:"ext"`
        URL          string            `json:"url"`
        Format       string            `json:"format"`
        FormatID     string            `json:"format_id"`
        Width        int               `json:"width"`
        Height       int               `json:"height"`
        Vcodec       string            `json:"vcodec"`
        Acodec       string            `json:"acodec"`
        Protocol     string            `json:"protocol"`
        HTTPHeaders  map[string]string `json:"http_headers"`
        RequestedFormats []ytdlpFormat `json:"requested_formats"`
}

// ytdlpFormat 单个格式流
type ytdlpFormat struct {
        URL      string `json:"url"`
        FormatID string `json:"format_id"`
        Ext      string `json:"ext"`
        Width    int    `json:"width"`
        Height   int    `json:"height"`
        Vcodec   string `json:"vcodec"`
        Acodec   string `json:"acodec"`
        Protocol string `json:"protocol"`
}

// handleParse 解析接口：调用 yt-dlp 解析链接，返回 JSON（直链/格式信息）
func handleParse(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
                http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
                return
        }

        var req ParseRequest
        contentType := r.Header.Get("Content-Type")
        if strings.Contains(contentType, "application/json") {
                if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                        writeJSON(w, http.StatusBadRequest, ParseResponse{Success: false, Error: "请求体解析失败: " + err.Error()})
                        return
                }
        } else {
                if err := r.ParseForm(); err != nil {
                        writeJSON(w, http.StatusBadRequest, ParseResponse{Success: false, Error: "表单解析失败: " + err.Error()})
                        return
                }
                req.URL = strings.TrimSpace(r.FormValue("url"))
        }

        url := strings.TrimSpace(req.URL)
        if url == "" {
                writeJSON(w, http.StatusBadRequest, ParseResponse{Success: false, Error: "URL 不能为空"})
                return
        }

        // 从输入中提取真实 URL（支持"【标题】 https://b23.tv/xxx"这类分享文本）
        url = extractURL(url)
        if url == "" {
                writeJSON(w, http.StatusBadRequest, ParseResponse{Success: false, Error: "未找到有效的 http:// 或 https:// 链接"})
                return
        }

        log.Printf("解析请求: %s", url)

        info, stderr, err := parseWithYtDlp(url)
        if err != nil {
                errMsg := err.Error()
                if stderr != "" {
                        errMsg = errMsg + "; stderr: " + stderr
                }
                log.Printf("解析失败: %s", errMsg)

                writeJSON(w, http.StatusOK, ParseResponse{
                        Success: false,
                        Error:   "无法解析此链接：" + errMsg,
                })
                return
        }

        // 构造响应
        resp := ParseResponse{
                Success:      true,
                Title:        info.Title,
                Extractor:    info.Extractor,
                ExtractorKey: info.ExtractorKey,
                WebpageURL:   info.WebpageURL,
                Ext:          info.Ext,
                Headers:      info.HTTPHeaders,
        }

        // 构造文件名
        if resp.Title != "" {
                resp.Filename = sanitizeFilename(info.Title) + "." + pickExt(info)
        } else {
                resp.Filename = "video." + pickExt(info)
        }

        // 判断是否可直连下载
        // 不可直连的情况：
        //   1. requested_formats 非空（bestvideo+bestaudio 分离流，需要 ffmpeg 合并）
        //   2. 协议为 m3u8/m3u8_native（HLS 分片流，浏览器无法直接下载）
        //   3. 协议为 dash（DASH 分片流）
        //   4. url 为空
        canDirect := false
        if len(info.RequestedFormats) > 0 {
                // 分离流，需要合并，必须走代理
                resp.FallbackRequired = true
                resp.Format = fmt.Sprintf("bestvideo+bestaudio (需合并, %dx%d)", info.RequestedFormats[0].Width, info.RequestedFormats[0].Height)
                if len(info.RequestedFormats) > 0 {
                        resp.Resolution = fmt.Sprintf("%dx%d", info.RequestedFormats[0].Width, info.RequestedFormats[0].Height)
                }
                log.Printf("分离流，需代理合并: %s", resp.Format)
        } else if info.URL != "" && isDirectProtocol(info.Protocol) {
                // 单文件 + http/https 协议，可直连
                canDirect = true
                resp.DirectURL = info.URL
                resp.FallbackRequired = false
                if info.Format != "" {
                        resp.Format = info.Format
                } else {
                        resp.Format = fmt.Sprintf("%s (format_id=%s)", info.Ext, info.FormatID)
                }
                resp.Resolution = fmt.Sprintf("%dx%d", info.Width, info.Height)
                log.Printf("可直连下载: %s, 协议=%s", resp.Format, info.Protocol)
        } else {
                // HLS/DASH 或其他协议，需代理
                resp.FallbackRequired = true
                resp.Format = fmt.Sprintf("%s (协议=%s, 需代理)", info.Ext, info.Protocol)
                resp.Resolution = fmt.Sprintf("%dx%d", info.Width, info.Height)
                log.Printf("需代理下载: 协议=%s", info.Protocol)
        }

        _ = canDirect

        log.Printf("解析成功: title=%s, extractor=%s, fallback=%v", resp.Title, resp.Extractor, resp.FallbackRequired)
        writeJSON(w, http.StatusOK, resp)
}

// parseWithYtDlp 调用 yt-dlp -J 仅解析不下载
//
// 参数说明：
//   -J              : 输出 JSON 元数据（不下载文件）
//   -f <selector>   : 格式选择器，与下载时一致
//   --no-playlist   : 不展开播放列表
//   --no-warnings   : 抑制警告
//   --flat-playlist : 若是播放列表只取第一个
func parseWithYtDlp(url string) (*ytdlpInfo, string, error) {
        args := []string{
                "-J",
                "-f", formatSelector,
                "--format-sort", "res,fps,tbr", // 按分辨率→帧率→总码率排序，确保选最高码率流
                "--no-playlist",
                "--no-warnings",
                "--no-progress",
                url,
        }

        cmd := exec.Command(ytdlpBinary, args...)
        var stdout, stderr bytes.Buffer
        cmd.Stdout = &stdout
        cmd.Stderr = &stderr

        startTime := time.Now()
        if err := cmd.Run(); err != nil {
                return nil, stderr.String(), fmt.Errorf("yt-dlp 解析失败: %w", err)
        }
        log.Printf("yt-dlp 解析耗时: %s", time.Since(startTime).Truncate(time.Millisecond))

        var info ytdlpInfo
        if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
                return nil, stderr.String(), fmt.Errorf("解析 yt-dlp JSON 输出失败: %w", err)
        }
        return &info, stderr.String(), nil
}

// extractURL 从用户输入中提取真实 URL
func extractURL(input string) string {
        input = strings.TrimSpace(input)
        if input == "" {
                return ""
        }

        // 正则匹配 http:// 或 https:// 开头的 URL
        // 匹配到第一个空白字符或字符串结尾为止
        // 支持 URL 中的常见字符：字母数字 / : . ? = & - _ ~ % # + @ ! $ ' ( ) * , ;
        idx := strings.Index(input, "http://")
        if idx < 0 {
                idx = strings.Index(input, "https://")
        }
        if idx < 0 {
                return ""
        }

        // 从 idx 开始截取到第一个空白字符
        rest := input[idx:]
        for i, ch := range rest {
                // 空白字符（空格、制表符、换行、回车、中文空格）作为 URL 结束符
                if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == '\u3000' {
                        return rest[:i]
                }
        }
        return rest
}

func isDirectProtocol(protocol string) bool {
        switch protocol {
        case "http", "https":
                return true
        default:
                return false
        }
}

// pickExt 选择输出扩展名
func pickExt(info *ytdlpInfo) string {
        if info.Ext != "" {
                return info.Ext
        }
        if len(info.RequestedFormats) > 0 {
                return "mkv" // 分离流合并后用 mkv
        }
        return "mp4"
}

// writeJSON 写入 JSON 响应
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
        w.Header().Set("Content-Type", "application/json; charset=utf-8")
        w.WriteHeader(status)
        if err := json.NewEncoder(w).Encode(v); err != nil {
                log.Printf("写入 JSON 响应失败: %v", err)
        }
}

// ====== 代理下载接口 ======

// handleDownload 代理下载接口（回退模式）
//
// 当直链不可用时（分离流需合并、HLS/DASH 分片、需登录态等），
// 前端调用此接口，服务器用 yt-dlp 下载并合并后，以文件流返回给前端。
func handleDownload(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
                http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
                return
        }

        var url string
        contentType := r.Header.Get("Content-Type")
        if strings.Contains(contentType, "application/json") {
                var req ParseRequest
                if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
                        http.Error(w, "请求体解析失败: "+err.Error(), http.StatusBadRequest)
                        return
                }
                url = strings.TrimSpace(req.URL)
        } else {
                if err := r.ParseForm(); err != nil {
                        http.Error(w, "表单解析失败: "+err.Error(), http.StatusBadRequest)
                        return
                }
                url = strings.TrimSpace(r.FormValue("url"))
        }

        if url == "" {
                http.Error(w, "URL 不能为空", http.StatusBadRequest)
                return
        }
        // 从输入中提取真实 URL（支持"【标题】 https://b23.tv/xxx"这类分享文本）
        url = extractURL(url)
        if url == "" {
                http.Error(w, "未找到有效的 http:// 或 https:// 链接", http.StatusBadRequest)
                return
        }

        log.Printf("代理下载请求: %s", url)

        // 所有下载统一存放在 tempRoot 同一个文件夹内
        // 推送完成后清理目录内所有文件（不保留任何文件）
        outDir := tempRoot
        if err := os.MkdirAll(outDir, 0o755); err != nil {
                http.Error(w, "创建临时目录失败: "+err.Error(), http.StatusInternalServerError)
                return
        }

        filePath, err := runYtDlp(url, outDir)
        if err != nil {
                log.Printf("代理下载失败: %v", err)
                // 下载失败，清理目录内所有文件
                cleanSize := cleanAllFiles(outDir)
                log.Printf("下载失败，已清理临时文件 (释放 %.2f MB)", float64(cleanSize)/1024/1024)
                http.Error(w, "下载失败: "+err.Error(), http.StatusInternalServerError)
                return
        }

        // 推送文件流给前端
        streamErr := streamFile(w, r, filePath)

        // 推送完成后，清理目录内所有文件（不保留任何文件）
        cleanSize := cleanAllFiles(outDir)
        log.Printf("✅ 临时文件已清理: %s (释放 %.2f MB)", filePath, float64(cleanSize)/1024/1024)

        if streamErr != nil {
                log.Printf("文件推送失败: %v", streamErr)
                return
        }

        log.Printf("代理下载完成: %s", url)
}

// cleanAllFiles 清理目录内所有文件和子目录（保留目录本身供下次使用）
// 返回清理的总字节数
func cleanAllFiles(dir string) int64 {
        var totalCleaned int64
        entries, err := os.ReadDir(dir)
        if err != nil {
                return 0
        }
        for _, e := range entries {
                fullPath := filepath.Join(dir, e.Name())
                // 统计大小（文件直接取 Size，目录递归统计）
                if info, err := e.Info(); err == nil && !e.IsDir() {
                        totalCleaned += info.Size()
                }
                // 删除文件或子目录
                if err := os.RemoveAll(fullPath); err != nil {
                        log.Printf("清理失败: %s: %v", fullPath, err)
                }
        }
        return totalCleaned
}

// runYtDlp 调用 yt-dlp 下载最高画质视频（含音频）
func runYtDlp(url, outDir string) (string, error) {
        beforeFiles := make(map[string]bool)
        if entries, err := os.ReadDir(outDir); err == nil {
                for _, e := range entries {
                        beforeFiles[e.Name()] = true
                }
        }

        outputTemplate := filepath.Join(outDir, "%(title).80s [%(id)s].%(ext)s")

        ffmpegPath, err := exec.LookPath(ffmpegBinary)
        if err != nil {
                log.Printf("警告: 未在 PATH 中找到 ffmpeg: %v", err)
                ffmpegPath = ffmpegBinary
        } else {
                log.Printf("ffmpeg 路径: %s", ffmpegPath)
        }

        args := []string{
                "-f", formatSelector,
                "--format-sort", "res,fps,tbr", // 按分辨率→帧率→总码率排序，确保选最高码率流（VP9/AV1优先于H.264）
                "--merge-output-format", "mkv", // mkv 支持所有编码组合，确保音视频合并成功
                "--no-playlist",
                "--no-warnings",
                "--newline",
                "--no-progress",
                "-o", outputTemplate,
                "--ffmpeg-location", ffmpegPath,
                url,
        }

        log.Printf("yt-dlp 参数: %s %s", ytdlpBinary, strings.Join(args, " "))

        cmd := exec.Command(ytdlpBinary, args...)
        var stderr bytes.Buffer
        cmd.Stderr = &stderr

        startTime := time.Now()
        if err := cmd.Run(); err != nil {
                return "", fmt.Errorf("yt-dlp 执行失败: %w; stderr: %s", err, stderr.String())
        }
        log.Printf("yt-dlp 执行耗时: %s", time.Since(startTime).Truncate(time.Millisecond))

        filePath, err := findDownloadedFile(outDir, beforeFiles)
        if err != nil {
                return "", fmt.Errorf("%w; stderr: %s", err, stderr.String())
        }

        info, _ := os.Stat(filePath)
        log.Printf("输出文件: %s (%.2f MB)", filePath, float64(info.Size())/1024/1024)
        return filePath, nil
}

// findDownloadedFile 在输出目录中查找本次下载生成的文件
// 优先返回已合并文件，若只有分离流文件则报错
func findDownloadedFile(outDir string, beforeFiles map[string]bool) (string, error) {
        entries, err := os.ReadDir(outDir)
        if err != nil {
                return "", fmt.Errorf("读取输出目录失败: %w", err)
        }

        var mergedFiles []string
        var streamFiles []string
        var maxSize int64
        var bestFile string

        for _, e := range entries {
                if e.IsDir() {
                        continue
                }
                name := e.Name()
                if beforeFiles[name] {
                        continue
                }
                lowerName := strings.ToLower(name)
                if strings.HasSuffix(lowerName, ".part") ||
                        strings.HasSuffix(lowerName, ".ytdl") ||
                        strings.HasSuffix(lowerName, ".temp") ||
                        strings.HasSuffix(lowerName, ".frag") ||
                        strings.HasSuffix(lowerName, ".tmp") ||
                        strings.HasSuffix(lowerName, ".json") {
                        continue
                }
                info, err := e.Info()
                if err != nil {
                        continue
                }
                fullPath := filepath.Join(outDir, name)
                if isStreamFile(name) {
                        streamFiles = append(streamFiles, fullPath)
                        log.Printf("检测到分离流文件（合并可能失败）: %s (%.2f MB)", name, float64(info.Size())/1024/1024)
                        continue
                }
                mergedFiles = append(mergedFiles, fullPath)
                if info.Size() > maxSize {
                        maxSize = info.Size()
                        bestFile = fullPath
                }
        }

        if bestFile != "" {
                log.Printf("发现 %d 个已合并文件，选择最大的: %s", len(mergedFiles), filepath.Base(bestFile))
                return bestFile, nil
        }
        if len(streamFiles) > 0 {
                names := make([]string, 0, len(streamFiles))
                for _, f := range streamFiles {
                        names = append(names, filepath.Base(f))
                }
                return "", fmt.Errorf("音视频合并失败：ffmpeg 未正确执行，只产生了分离流文件 %v", names)
        }
        return "", fmt.Errorf("yt-dlp 未生成输出文件")
}

// isStreamFile 检测文件名是否为 yt-dlp 的分离流文件
func isStreamFile(name string) bool {
        lower := strings.ToLower(name)
        dotIdx := strings.LastIndex(lower, ".")
        if dotIdx < 0 {
                return false
        }
        stem := lower[:dotIdx]
        for i := 0; i < len(stem)-2; i++ {
                if stem[i] == '.' && stem[i+1] == 'f' && i+2 < len(stem) {
                        allDigit := true
                        for j := i + 2; j < len(stem); j++ {
                                if stem[j] < '0' || stem[j] > '9' {
                                        allDigit = false
                                        break
                                }
                        }
                        if allDigit && i+2 < len(stem) {
                                return true
                        }
                }
        }
        return false
}

// streamFile 以流的形式将文件推送到 HTTP 响应
//
// 关键设计：
//   1. 先设置 Content-Disposition 触发浏览器下载
//   2. 使用 io.Copy 流式传输，内存占用恒定（默认 32KB 缓冲）
//   3. 设置 Content-Length，让浏览器显示下载进度
//   4. 推送完成后立即 Flush，确保数据全部发送
//   5. 文件名做 sanitize 处理，避免 HTTP 头注入
func streamFile(w http.ResponseWriter, r *http.Request, filePath string) error {
        info, err := os.Stat(filePath)
        if err != nil {
                return fmt.Errorf("文件不存在: %w", err)
        }

        f, err := os.Open(filePath)
        if err != nil {
                return fmt.Errorf("打开文件失败: %w", err)
        }
        defer f.Close()

        filename := filepath.Base(filePath)
        filename = sanitizeFilename(filename)
        contentType := guessMimeType(filename)

        // 设置响应头，触发浏览器下载
        w.Header().Set("Content-Type", contentType)
        w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
        w.Header().Set("Content-Length", fmt.Sprintf("%d", info.Size()))
        w.Header().Set("Cache-Control", "no-cache")
        // 禁用代理缓存，确保大文件传输不被中间层截断
        w.Header().Set("X-Accel-Buffering", "no")

        log.Printf("开始推送文件: %s (%.2f MB)", filename, float64(info.Size())/1024/1024)

        // 流式推送，io.Copy 内部使用 32KB 缓冲，内存占用恒定
        written, err := io.Copy(w, f)
        if err != nil {
                log.Printf("io.Copy 异常: 已推送 %d 字节, 错误: %v", written, err)
                return fmt.Errorf("io.Copy 失败: %w", err)
        }

        // 确保所有缓冲数据都发送到客户端
        if flusher, ok := w.(http.Flusher); ok {
                flusher.Flush()
        }

        log.Printf("文件推送完成: %s (已推送 %d 字节)", filename, written)
        return nil
}

func sanitizeFilename(name string) string {
        name = strings.ReplaceAll(name, `"`, "'")
        name = strings.ReplaceAll(name, "\n", " ")
        name = strings.ReplaceAll(name, "\r", " ")
        return name
}

func guessMimeType(filename string) string {
        ext := strings.ToLower(filepath.Ext(filename))
        switch ext {
        case ".mp4":
                return "video/mp4"
        case ".webm":
                return "video/webm"
        case ".mkv":
                return "video/x-matroska"
        case ".m4a":
                return "audio/mp4"
        case ".mp3":
                return "audio/mpeg"
        case ".opus":
                return "audio/ogg"
        case ".ogg":
                return "audio/ogg"
        case ".wav":
                return "audio/wav"
        case ".flv":
                return "video/x-flv"
        default:
                return "application/octet-stream"
        }
}

// cleanStartupCache 服务启动时清理上次运行残留的临时文件
// 防止容器异常退出导致临时文件堆积占用磁盘
func cleanStartupCache() {
        entries, err := os.ReadDir(tempRoot)
        if err != nil {
                log.Printf("启动清理: 读取临时目录失败: %v", err)
                return
        }
        cleaned := 0
        var totalSize int64
        for _, entry := range entries {
                target := filepath.Join(tempRoot, entry.Name())
                if info, err := entry.Info(); err == nil {
                        totalSize += info.Size()
                }
                if err := os.RemoveAll(target); err == nil {
                        cleaned++
                }
        }
        if cleaned > 0 {
                log.Printf("启动清理: 已清理 %d 个残留文件 (释放 %.2f MB)", cleaned, float64(totalSize)/1024/1024)
        } else {
                log.Printf("启动清理: 无残留临时文件")
        }
}

// cleanupTempFiles 后台定期清理超过 maxAge 的临时文件
// 作为兜底机制，防止推送完成后清理失败导致文件残留
func cleanupTempFiles(interval, maxAge time.Duration) {
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        for range ticker.C {
                entries, err := os.ReadDir(tempRoot)
                if err != nil {
                        continue
                }
                now := time.Now()
                for _, entry := range entries {
                        info, err := entry.Info()
                        if err != nil {
                                continue
                        }
                        if now.Sub(info.ModTime()) > maxAge {
                                target := filepath.Join(tempRoot, entry.Name())
                                size := info.Size()
                                if err := os.RemoveAll(target); err == nil {
                                        log.Printf("已清理过期临时文件: %s (释放 %.2f MB, 存在 %s)",
                                                filepath.Base(target), float64(size)/1024/1024, now.Sub(info.ModTime()).Truncate(time.Second))
                                }
                        }
                }
        }
}
