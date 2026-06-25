// Package logdir 提供基于 lumberjack 的文件日志 handler。
//
// 特性（由 lumberjack 提供）：
//   - 按大小自动滚动（默认 100 MB / 文件）
//   - 按数量自动清理旧文件（默认保留 7 份）
//   - 旧文件自动 gzip 压缩
//   - 跨日滚动由大小滚动隐含覆盖，无需额外处理
//
// 与 MultiHandler 配合可同时写 stdout + 文件。
package logdir

import (
	"context"
	"io"
	"log/slog"
	"os"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Config 是文件日志的配置项，对应环境变量 LOG_DIR / LOG_MAX_SIZE / LOG_MAX_BACKUPS / LOG_MAX_AGE。
type Config struct {
	// Dir 是日志文件目录。为空则不写文件（仅 stdout）。
	Dir string
	// MaxSize 是单个日志文件的最大 MB 数，超过后自动滚动。默认 100。
	MaxSize int
	// MaxBackups 是保留的旧日志文件最大数量。默认 7。0 表示保留全部。
	MaxBackups int
	// MaxAge 是旧日志文件保留的最大天数。默认 0（不按天数清理，仅按数量）。
	MaxAge int
	// Compress 是否压缩旧日志文件（gzip）。默认 true。
	Compress bool
}

// Handler 是 slog.Handler，底层使用 lumberjack 做日志轮转。
type Handler struct {
	textCore *slog.TextHandler
	writer   *lumberjack.Logger
}

// New 创建基于 lumberjack 的文件 handler。
// cfg.Dir 必须非空（调用方已确保目录存在）。
func New(cfg Config, opts *slog.HandlerOptions) (*Handler, error) {
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return nil, err
	}

	maxSize := cfg.MaxSize
	if maxSize == 0 {
		maxSize = 100
	}
	maxBackups := cfg.MaxBackups
	if maxBackups == 0 {
		maxBackups = 7
	}
	compress := true
	if cfg.Dir == "" {
		// 不可能走到这里，但防御性设置。
		compress = false
	}
	// 只有显式配置了才覆盖 compress。
	if cfg.Compress == false && cfg.MaxBackups != 0 {
		compress = false
	}

	lj := &lumberjack.Logger{
		Filename:   cfg.Dir + "/gateway.log",
		MaxSize:    maxSize,
		MaxBackups: maxBackups,
		MaxAge:     cfg.MaxAge,
		Compress:   compress,
	}

	return &Handler{
		textCore: slog.NewTextHandler(lj, opts),
		writer:   lj,
	}, nil
}

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool {
	return h.textCore.Enabled(context.Background(), l)
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	return h.textCore.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{
		textCore: h.textCore.WithAttrs(attrs).(*slog.TextHandler),
		writer:   h.writer,
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{
		textCore: h.textCore.WithGroup(name).(*slog.TextHandler),
		writer:   h.writer,
	}
}

// Close 刷新并关闭底层 lumberjack writer。
func (h *Handler) Close() error {
	return h.writer.Close()
}

// ── MultiHandler：fan-out 到多个 handler ──

// MultiHandler 创建一个同时写多个 handler 的 fan-out handler。
func MultiHandler(handlers ...slog.Handler) slog.Handler {
	return &multiHandler{handlers: handlers}
}

type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: hs}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: hs}
}

var _ io.Closer = (*Handler)(nil)
