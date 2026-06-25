// Package proxy 构建透明透传反向代理与共享 HTTP 传输层。
//
// 设计目标：转发时不改变任何原始内容（请求头与请求体）。
// ReverseProxy 仅在 SSE_INTERCEPT_ENABLED=false 时作为纯透传回退使用；
// 启用拦截时由 internal/sse 包的 ProxyHandler 接管，二者共享同一个 Transport。
package proxy

import (
	"crypto/tls"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/routing"
)

// NewTransport 构建到上游的 HTTP 传输层（连接池 + HTTP/2）。
// 导出供 sse 包共享同一个 Transport，复用连接池。
func NewTransport(cfg *config.Config) *http.Transport {
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

// New 构建并返回一个透明透传反向代理（用于 SSE_INTERCEPT_ENABLED=false 的纯透传回退）。
//
// 关键配置说明：
//   - Rewrite 仅调用 r.SetURL(target)，不修改 Out.Header。
//     ReverseProxy 在调用 Rewrite 之前已把入站 Header 完整复制到出站，
//     并按 RFC 7230 自动剥离 hop-by-hop 头。所有业务头原样透传。
//   - FlushInterval=-1 使每次写入立即 flush，保证 SSE 流式响应逐 token 输出。
//   - Transport 由调用方传入（与 sse 包共享连接池）。
//   - body 透传：ReverseProxy 直接把 r.In.Body 作为 io.Reader 挂到 r.Out.Body，字节级一致。
func New(cfg *config.Config, transport http.RoundTripper, logger *slog.Logger) *httputil.ReverseProxy {
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			// 按请求协议（路径）选上游；header 与 body 由 ReverseProxy 原样透传。
			target := routing.SelectTarget(r.In, cfg.OpenAITarget, cfg.AnthropicTarget)
			r.SetURL(target)
			// 路径重写：上游以 / 结尾时剥离 /v1 前缀拼接到上游路径后
			r.Out.URL.Path = routing.RewritePath(target, r.In.URL.Path)
			r.Out.URL.RawPath = ""
			if cfg.PreserveHost {
				// SetURL 会把 Out.Host 改为上游 Host；此处按需恢复客户端原始 Host。
				r.Out.Host = r.In.Host
			}
		},
		// -1 表示每次写入立即 flush，SSE 流式代理的生命线。
		FlushInterval: -1,
		Transport:     transport,
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
