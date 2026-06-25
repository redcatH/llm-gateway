package server

import (
	"bufio"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
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
//
// 两个诊断要点：
//   - 请求开始即打 debug 日志：流式请求若 hang/panic，结束日志不会出现，
//     至少能看到请求已到达，区分"没到网关"与"到了但卡住"。
//   - recover 兜底：Go net/http 默认不 recover，handler panic 会拖死整个进程
//     （表现为客户端 ConnectionRefused + 无结构化日志）。此处捕获后记 Error + 返回 500。
func loggingMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 健康检查是基础设施探测（Docker 每 30s 一次），无业务价值，跳过访问日志避免刷屏。
		isHealth := r.URL.Path == "/health"
		start := time.Now()
		if !isHealth {
			logger.Debug("request start",
				"method", r.Method,
				"path", r.URL.Path,
				"remote", r.RemoteAddr,
			)
		}
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if rcv := recover(); rcv != nil {
				logger.Error("panic recovered",
					"method", r.Method,
					"path", r.URL.Path,
					"panic", fmt.Sprint(rcv),
					"stack", string(debug.Stack()),
					"duration_ms", time.Since(start).Milliseconds(),
				)
				// 仅在尚未写过响应头时补 500；流式中途崩溃则连接已破，不再写。
				if rec.status == 0 {
					http.Error(rec, "internal server error", http.StatusInternalServerError)
				}
			}
		}()
		next.ServeHTTP(rec, r)

		if isHealth {
			return
		}
		logger.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}
