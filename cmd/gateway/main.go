// llm-gateway 是一个透明透传的 LLM API 反向代理网关。
// 处理 /v1/chat/completions（OpenAI 协议）与 /v1/messages（Anthropic 协议），
// 将所有请求头与请求体原样转发到单一固定上游；并对上游 200+SSE 的 error 帧
// 做首帧 peek 拦截（命中规则时返回 503 让客户端重试）。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/logdir"
	"llm-gateway/internal/proxy"
	"llm-gateway/internal/server"
	"llm-gateway/internal/sse"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// 配置阶段 logger 尚未初始化，用默认 logger 输出后退出。
		slog.Error("invalid config", "err", err.Error())
		os.Exit(1)
	}

	handlerOpts := &slog.HandlerOptions{Level: cfg.LogLevel}

	// stdout handler（始终启用）。
	stdoutHandler := slog.NewTextHandler(os.Stdout, handlerOpts)

	// 组装 logger：有 LOG_DIR 时同时写文件；否则仅 stdout。
	var logger *slog.Logger
	var fileHandler *logdir.Handler
	if cfg.LogDir != "" {
		var err error
		fileHandler, err = logdir.New(logdir.Config{
			Dir:        cfg.LogDir,
			MaxSize:    cfg.LogMaxSize,
			MaxBackups: cfg.LogMaxBackups,
			MaxAge:     cfg.LogMaxAge,
			Compress:   cfg.LogCompress,
		}, handlerOpts)
		if err != nil {
			slog.Error("cannot open log file", "dir", cfg.LogDir, "err", err.Error())
			os.Exit(1)
		}
		defer fileHandler.Close()
		logger = slog.New(logdir.MultiHandler(stdoutHandler, fileHandler))
	} else {
		logger = slog.New(stdoutHandler)
	}
	slog.SetDefault(logger)

	// 共享 Transport：ReverseProxy 与 sse.ProxyHandler 复用同一连接池。
	transport := proxy.NewTransport(cfg)

	// 按 SSE_INTERCEPT_ENABLED 选择 handler：
	//   启用 → sse.ProxyHandler（首帧 peek 拦截 + 透传）
	//   关闭 → ReverseProxy 纯透传回退
	var handler http.Handler
	if cfg.SSEInterceptEnabled {
		rules := sse.DefaultRules(cfg.SSERetryAfter)
		handler = sse.ProxyHandler(cfg.OpenAITarget, cfg.AnthropicTarget, cfg.PreserveHost, transport, rules, logger, cfg.LogDir)
	} else {
		handler = proxy.New(cfg, transport, logger)
	}

	srv := server.New(cfg, handler, logger)

	logger.Info("starting transparent gateway",
		"listen", cfg.ListenAddr,
		"upstream_openai", cfg.OpenAITarget.String(),
		"upstream_anthropic", cfg.AnthropicTarget.String(),
		"preserve_host", cfg.PreserveHost,
		"sse_intercept", cfg.SSEInterceptEnabled,
		"max_idle_conns_per_host", cfg.MaxIdleConnsPerHost,
	)

	// 在独立 goroutine 中监听，主 goroutine 等待退出信号。
	serveErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// 捕获中断信号以触发优雅关闭。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case <-stop:
		logger.Info("shutdown signal received, draining in-flight requests")
	case err := <-serveErr:
		logger.Error("server failed to start", "err", err.Error())
		os.Exit(1)
	}

	// 给在途请求（含进行中的 SSE 流）最多 30 秒完成。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		logger.Error("graceful shutdown failed", "err", err.Error())
		os.Exit(1)
	}
	logger.Info("server stopped cleanly")
}
