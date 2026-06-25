// Package logdir 提供按日期滚动的文件日志 handler，零第三方依赖。
//
// 文件名格式：<dir>/<YYYY-MM-DD>.log，跨日自动切换新文件。
// 与 logdir.MultiHandler 配合可同时写 stdout + 文件。
package logdir

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Handler 是 slog.Handler，按日期写文件，跨日自动切换。
type Handler struct {
	mu       sync.Mutex
	dir      string
	opts     slog.HandlerOptions
	date     string // 当前文件对应的日期 "2006-01-02"
	file     *os.File
	textCore *slog.TextHandler
}

// New 创建按日期滚动的文件 handler。
// dir 必须是已存在的目录（调用方负责 os.MkdirAll）。
func New(dir string, opts *slog.HandlerOptions) (*Handler, error) {
	h := &Handler{dir: dir}
	if opts != nil {
		h.opts = *opts
	}
	if err := h.openFile(time.Now()); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Handler) Enabled(_ context.Context, l slog.Level) bool {
	minLevel := slog.LevelInfo
	if h.opts.Level != nil {
		minLevel = h.opts.Level.Level()
	}
	return l >= minLevel
}

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := time.Now()
	today := now.Format("2006-01-02")
	if today != h.date {
		if err := h.openFile(now); err != nil {
			return err
		}
	}
	return h.textCore.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &Handler{
		dir:      h.dir,
		opts:     h.opts,
		date:     h.date,
		file:     h.file,
		textCore: h.textCore.WithAttrs(attrs).(*slog.TextHandler),
	}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	h.mu.Lock()
	defer h.mu.Unlock()
	return &Handler{
		dir:      h.dir,
		opts:     h.opts,
		date:     h.date,
		file:     h.file,
		textCore: h.textCore.WithGroup(name).(*slog.TextHandler),
	}
}

// Close 关闭当前打开的日志文件。
func (h *Handler) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.file != nil {
		return h.file.Close()
	}
	return nil
}

func (h *Handler) openFile(now time.Time) error {
	if h.file != nil {
		_ = h.file.Close()
	}
	h.date = now.Format("2006-01-02")
	name := filepath.Join(h.dir, h.date+".log")
	f, err := os.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", name, err)
	}
	h.file = f
	h.textCore = slog.NewTextHandler(f, &h.opts)
	return nil
}

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
