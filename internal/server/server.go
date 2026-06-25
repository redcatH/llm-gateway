// Package server 装配 http.Server：本地处理 /health，其余路径透传到上游。
package server

import (
	"log/slog"
	"net/http"
	"net/http/httputil"

	"xunfei-gateway/internal/config"
)

// New 装配 http.Server。
//
// 路由：
//   - GET /health → 本地返回 200，不转发到上游（健康检查不应消耗上游配额）。
//   - 其余所有路径 → 透传到上游（含 /v1/chat/completions、/v1/messages 及任意路径）。
//
// 关键：故意不设 ReadTimeout/WriteTimeout，否则会切断 SSE 长连接。
// 仅设 ReadHeaderTimeout 用于防御慢速头攻击。
func New(cfg *config.Config, rp *httputil.ReverseProxy, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler)
	mux.Handle("/", rp) // 兜底：透传所有未匹配的路径

	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           loggingMiddleware(logger, mux),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		// ReadTimeout / WriteTimeout 故意留零，避免切断 SSE 长连接。
	}
}

// healthHandler 返回固定的健康状态，不接触上游。
func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}
