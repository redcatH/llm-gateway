// Package proxy 构建透明透传反向代理。
//
// 设计目标：转发时不改变任何原始内容（请求头与请求体）。
// 实现手段：使用标准库 httputil.ReverseProxy，仅设置上游地址，
// 不触碰 Out.Header，body 以 io.Reader 流式透传。
package proxy

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"xunfei-gateway/internal/config"
)

// New 构建并返回一个透明透传反向代理。
//
// 关键配置说明：
//   - Rewrite 仅调用 r.SetURL(target)，不修改 Out.Header。
//     ReverseProxy 在调用 Rewrite 之前已把入站 Header 完整复制到出站，
//     并按 RFC 7230 自动剥离 hop-by-hop 头（Connection/Keep-Alive/
//     Transfer-Encoding/Upgrade/Proxy-Authorization 等——这是协议强制的
//     连接控制头剥离，并非内容变更）。所有业务头（Authorization、
//     x-api-key、anthropic-version、anthropic-beta、Content-Type、
//     User-Agent 及任意自定义头）原样透传。
//   - FlushInterval=-1 使每次写入立即 flush，保证 SSE 流式响应
//     （/v1/chat/completions、/v1/messages 的 stream）逐 token 输出。
//   - Transport 连接池复用上游连接，支撑高并发。
//   - body 透传：ReverseProxy 直接把 r.In.Body 作为 io.Reader 挂到
//     r.Out.Body，零缓冲、零解析，字节级一致。
func New(cfg *config.Config, logger *slog.Logger) *httputil.ReverseProxy {
	target := cfg.UpstreamURL

	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// 仅设置目标 scheme/host/path，header 与 body 由 ReverseProxy 原样透传。
			r.SetURL(target)
			if cfg.PreserveHost {
				// SetURL 会把 Out.Host 改为上游 Host；此处按需恢复客户端原始 Host。
				r.Out.Host = r.In.Host
			}
		},
		// -1 表示每次写入立即 flush，SSE 流式代理的生命线。
		FlushInterval: -1,
		Transport:     newTransport(cfg),
		ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
			logger.Error("upstream proxy error",
				"err", err.Error(),
				"method", req.Method,
				"path", req.URL.Path,
			)
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadGateway)
			// 不泄漏内部细节，仅返回通用错误。
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "upstream unreachable",
			})
		},
	}
	return rp
}

// newTransport 构建到上游的 HTTP 传输层，启用连接池与 HTTP/2。
func newTransport(cfg *config.Config) *http.Transport {
	tlsCfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
	}
	if cfg.UpstreamInsecureSkipVerify {
		// 仅用于调试自签证书的上游，生产环境应保持 false。
		tlsCfg.InsecureSkipVerify = true
	}

	return &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          cfg.MaxIdleConnsPerHost,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		IdleConnTimeout:       cfg.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsCfg,
	}
}
