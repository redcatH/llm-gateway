package server

import (
	"bufio"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"
)

// statusRecorder 包装 http.ResponseWriter 以记录状态码与字节量，
// 同时透传 http.Flusher 与 http.Hijacker 接口，确保 SSE 流式 flush
// 与 WebSocket 升级等行为不被包装层破坏（保持转发透明）。
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

// Unwrap 暴露底层 ResponseWriter，供 http.ResponseController 使用。
func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

// WriteHeader 记录状态码后转发。
func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

// Write 累计写入字节数后转发。若未显式 WriteHeader，按 200 计。
func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush 透传到底层 ResponseWriter，保证 ReverseProxy 的 FlushInterval=-1
// 能真正把 SSE 数据立即推给客户端。
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 透传 WebSocket 等升级连接，保持透明。
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not implement http.Hijacker")
	}
	return h.Hijack()
}

// loggingMiddleware 记录每个请求的访问日志（method/path/status/bytes/耗时）。
// 它只读取元信息，不修改请求或响应内容，不影响透传透明性。
func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
