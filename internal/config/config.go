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
	// OpenAITarget 是 OpenAI 协议（/v1/chat/completions 等）的路由目标，必填。
	OpenAITarget *url.URL
	// AnthropicTarget 是 Anthropic 协议（/v1/messages）的路由目标，必填。
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

	// ModelRewriteMode 控制响应 model 字段改写模式：
	//   off（默认，关闭）/ passthrough（未命中透传真名）/ default（未命中用 ModelDefault）。
	ModelRewriteMode string
	// ModelMap 是真实模型名→对外展示名的映射，仅 passthrough/default 模式生效。
	// 来自 MODEL_MAP 环境变量，格式 "key:value,key:value"。
	ModelMap map[string]string
	// ModelDefault 是 default 模式下未命中时的兜底对外名。
	ModelDefault string
}

// Load 从环境变量读取并校验配置。缺少必填项或格式非法时返回错误，
// 调用方应据此在启动阶段直接退出。
func Load() (*Config, error) {
	openaiURL, err := parseUpstreamURL("UPSTREAM_OPENAI_URL")
	if err != nil {
		return nil, err
	}
	anthropicURL, err := parseUpstreamURL("UPSTREAM_ANTHROPIC_URL")
	if err != nil {
		return nil, err
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

	modelMap, err := parseModelMap("MODEL_MAP")
	if err != nil {
		return nil, err
	}
	modelRewriteMode, err := parseRewriteMode("MODEL_REWRITE_MODE")
	if err != nil {
		return nil, err
	}
	modelDefault := envString("MODEL_DEFAULT", "")
	// default 模式必须提供兜底名，否则未命中时无值可填。
	if modelRewriteMode == "default" && modelDefault == "" {
		return nil, fmt.Errorf("MODEL_DEFAULT is required when MODEL_REWRITE_MODE=default")
	}

	return &Config{
		OpenAITarget:               openaiURL,
		AnthropicTarget:            anthropicURL,
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
		ModelRewriteMode:           modelRewriteMode,
		ModelMap:                   modelMap,
		ModelDefault:               modelDefault,
	}, nil
}

// ── 环境变量解析辅助函数 ──

// parseUpstreamURL 解析一个必填的上游 URL 环境变量。未设置或格式非法时返回错误。
func parseUpstreamURL(key string) (*url.URL, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return nil, fmt.Errorf("%s is required", key)
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

// parseModelMap 解析 MODEL_MAP 环境变量为真名→对外名映射。
// 格式 "key:value,key:value"；空字符串返回 nil（不报错，功能关闭）。
// 条目缺少冒号或 key 为空视为格式非法，返回错误。
// value 允许含冒号（按第一个冒号分割）。
func parseModelMap(key string) (map[string]string, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return nil, nil
	}
	m := make(map[string]string)
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue // 容忍首尾/连续逗号
		}
		idx := strings.Index(entry, ":")
		if idx <= 0 {
			return nil, fmt.Errorf("invalid %s entry %q: expected 'key:value'", key, entry)
		}
		k := strings.TrimSpace(entry[:idx])
		v := strings.TrimSpace(entry[idx+1:])
		if k == "" {
			return nil, fmt.Errorf("invalid %s entry %q: empty key", key, entry)
		}
		m[k] = v
	}
	return m, nil
}

// parseRewriteMode 解析 MODEL_REWRITE_MODE：off/passthrough/default。
// 空值默认 off；非法值返回错误。值转小写，大小写不敏感。
func parseRewriteMode(key string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(key))); v {
	case "", "off":
		return "off", nil
	case "passthrough", "default":
		return v, nil
	default:
		return "", fmt.Errorf("invalid %s %q: must be off/passthrough/default", key, v)
	}
}
