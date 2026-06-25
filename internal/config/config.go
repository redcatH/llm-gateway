// Package config 负责从环境变量加载并校验网关运行配置。
// 刻意不引入 viper 等第三方库，仅用标准库，保持零依赖。
package config

import (
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 持有网关运行所需的全部配置，全部来自环境变量。
type Config struct {
	// UpstreamURL 是默认/兜底上游（向后兼容）。当未配置协议专用上游时使用。
	// 可为 nil（此时 OpenAI/Anthropic 专用上游必须都配置）。
	UpstreamURL *url.URL
	// UpstreamOpenAIURL 是 OpenAI 协议（/v1/chat/completions 等）的专用上游（可选）。
	// 未配置时回退到 UpstreamURL。
	UpstreamOpenAIURL *url.URL
	// UpstreamAnthropicURL 是 Anthropic 协议（/v1/messages）的专用上游（可选）。
	// 未配置时回退到 UpstreamURL。
	UpstreamAnthropicURL *url.URL
	// OpenAITarget 是解析后的 OpenAI 路由目标（UpstreamOpenAIURL ?? UpstreamURL），必非空。
	OpenAITarget *url.URL
	// AnthropicTarget 是解析后的 Anthropic 路由目标（UpstreamAnthropicURL ?? UpstreamURL），必非空。
	AnthropicTarget *url.URL

	// ListenAddr 是网关监听地址，如 ":8080"。
	ListenAddr string
	// ReadHeaderTimeout 限制读取请求头的最长时间，用于防御慢速攻击。
	// 注意：不设 ReadTimeout/WriteTimeout，避免切断 SSE 长连接。
	ReadHeaderTimeout time.Duration
	// MaxIdleConnsPerHost 是到上游的每主机空闲连接数（连接池大小）。
	MaxIdleConnsPerHost int
	// IdleConnTimeout 是上游空闲连接的超时时间。
	IdleConnTimeout time.Duration
	// UpstreamInsecureSkipVerify 是否跳过上游 TLS 证书校验（仅调试用）。
	UpstreamInsecureSkipVerify bool
	// PreserveHost 为 true 时保留客户端原始 Host 头转发；
	// 默认 false，使用上游 Host（标准反向代理行为）。
	PreserveHost bool
	// LogLevel 是 slog 日志级别。
	LogLevel slog.Level
	// LogDir 是日志文件目录。非空时同步写文件（lumberjack 按大小滚动 + 自动清理）。
	// 为空则仅输出到 stdout。
	LogDir string
	// LogMaxSize 是单个日志文件的最大 MB 数，超过后自动滚动。默认 100。
	LogMaxSize int
	// LogMaxBackups 是保留的旧日志文件最大数量。默认 7。0 表示保留全部。
	LogMaxBackups int
	// LogMaxAge 是旧日志文件保留的最大天数。默认 0（不按天数清理，仅按数量）。
	LogMaxAge int
	// LogCompress 是否压缩旧日志文件（gzip）。默认 true。
	LogCompress bool
	// SSEInterceptEnabled 是否启用 SSE error 拦截。
	// true 时对上游 200+SSE 响应做首帧 peek，命中规则则拦截；
	// false 时全部走纯透传（不 peek）。
	SSEInterceptEnabled bool
	// SSERetryAfter 是拦截后 503 响应的 Retry-After 秒数。
	SSERetryAfter int
}

// Load 从环境变量读取并校验配置。缺少必填项或格式非法时返回错误，
// 调用方应据此在启动阶段直接退出。
func Load() (*Config, error) {
	upstreamURL, err := parseUpstreamURL("UPSTREAM_URL")
	if err != nil {
		return nil, err
	}
	openaiURL, err := parseUpstreamURL("UPSTREAM_OPENAI_URL")
	if err != nil {
		return nil, err
	}
	anthropicURL, err := parseUpstreamURL("UPSTREAM_ANTHROPIC_URL")
	if err != nil {
		return nil, err
	}

	// 协议专用上游未配置时回退到默认上游。
	openaiTarget := openaiURL
	if openaiTarget == nil {
		openaiTarget = upstreamURL
	}
	anthropicTarget := anthropicURL
	if anthropicTarget == nil {
		anthropicTarget = upstreamURL
	}
	if openaiTarget == nil {
		return nil, fmt.Errorf("no OpenAI upstream: set UPSTREAM_OPENAI_URL or UPSTREAM_URL")
	}
	if anthropicTarget == nil {
		return nil, fmt.Errorf("no Anthropic upstream: set UPSTREAM_ANTHROPIC_URL or UPSTREAM_URL")
	}

	readHeaderTimeout, err := envDuration("READ_HEADER_TIMEOUT", 10*time.Second)
	if err != nil {
		return nil, err
	}
	idleConnTimeout, err := envDuration("IDLE_CONN_TIMEOUT", 90*time.Second)
	if err != nil {
		return nil, err
	}
	maxIdle, err := envInt("MAX_IDLE_CONNS_PER_HOST", 100)
	if err != nil {
		return nil, err
	}
	retryAfter, err := envInt("SSE_RETRY_AFTER", 5)
	if err != nil {
		return nil, err
	}

	return &Config{
		UpstreamURL:                upstreamURL,
		UpstreamOpenAIURL:          openaiURL,
		UpstreamAnthropicURL:       anthropicURL,
		OpenAITarget:               openaiTarget,
		AnthropicTarget:            anthropicTarget,
		ListenAddr:                 envString("LISTEN_ADDR", ":8080"),
		ReadHeaderTimeout:          readHeaderTimeout,
		MaxIdleConnsPerHost:        maxIdle,
		IdleConnTimeout:            idleConnTimeout,
		UpstreamInsecureSkipVerify: envBool("UPSTREAM_INSECURE_SKIP_VERIFY", false),
		PreserveHost:               envBool("PRESERVE_HOST", false),
		LogLevel:                   envLevel("LOG_LEVEL", slog.LevelInfo),
		LogDir:                     envString("LOG_DIR", ""),
		LogMaxSize:                 envIntWithDefault("LOG_MAX_SIZE", 100),
		LogMaxBackups:              envIntWithDefault("LOG_MAX_BACKUPS", 7),
		LogMaxAge:                  envIntWithDefault("LOG_MAX_AGE", 0),
		LogCompress:                envBool("LOG_COMPRESS", true),
		SSEInterceptEnabled:        envBool("SSE_INTERCEPT_ENABLED", true),
		SSERetryAfter:              retryAfter,
	}, nil
}

// ── 环境变量解析辅助函数 ──

// parseUpstreamURL 解析一个可选的上游 URL 环境变量。未设置时返回 (nil, nil)。
func parseUpstreamURL(key string) (*url.URL, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid %s %q: %w", key, raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%s must use http or https scheme, got %q", key, u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%s must contain a host", key)
	}
	return u, nil
}

func envString(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid int for %s=%q: %w", key, v, err)
	}
	return n, nil
}

// envIntWithDefault 解析环境变量为 int，未设置或非法时返回默认值（不报错）。
func envIntWithDefault(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("invalid duration for %s=%q: %w", key, v, err)
	}
	return d, nil
}

func envLevel(key string, def slog.Level) slog.Level {
	switch strings.ToLower(os.Getenv(key)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "info":
		return slog.LevelInfo
	default:
		return def
	}
}
