# Logging Guidelines

> 使用 Go 标准库 `log/slog`，结构化文本格式，stdout 输出。

---

## Overview

项目使用 `log/slog` + `slog.NewTextHandler`，零依赖日志方案。
日志输出到 stdout（Docker/容器友好），级别通过 `LOG_LEVEL` 环境变量控制。

---

## Log Levels

| 级别 | 何时使用 | 示例 |
|------|---------|------|
| `debug` | 开发调试，生产环境关闭 | （当前未使用，预留） |
| `info` | 正常业务事件 | 请求访问日志、服务启动/关闭 |
| `warn` | 可恢复的异常情况 | 上游 4xx 响应体诊断 |
| `error` | 需要关注的故障 | 上游代理错误、服务启动失败、上游 5xx 响应体诊断 |

---

## Structured Logging

使用 slog 的键值对格式，不用格式化字符串：

```go
// ✅ 正确：结构化键值对
logger.Info("starting transparent gateway",
    "listen", cfg.ListenAddr,
    "upstream_openai", cfg.OpenAITarget.String(),
)

// ❌ 错误：格式化字符串
logger.Info(fmt.Sprintf("starting on %s", cfg.ListenAddr))
```

### 访问日志字段

每个请求记录以下字段（见 `internal/server/logging.go`）：

| 字段 | 含义 |
|------|------|
| `method` | HTTP 方法 |
| `path` | 请求路径 |
| `status` | 响应状态码 |
| `bytes` | 响应字节数 |
| `duration_ms` | 请求耗时（毫秒） |

---

## What to Log

- 服务启动参数（监听地址、上游地址、关键配置）
- 每个请求的访问日志（method/path/status/bytes/duration）
- 上游代理错误（err/method/path）
- 上游 4xx/5xx 响应体诊断：透明代理对上游非成功响应必须记录响应体才能定位真实错误
  （如讯飞用 HTTP 500 包装的 1105 "system is busy"，或用 400 包装的客户端类错误，不看 body 无法识别真实错误码）。
  实现见 `internal/sse/proxy.go` 的 `logNonSuccessResponse`，约束见下方 "What NOT to Log" 例外。
- 优雅关闭事件

---

## What NOT to Log

- ❌ 请求头中的 `Authorization`、`x-api-key` 等认证信息
- ❌ 请求体内容（可能含用户对话数据）
- ❌ 上游响应体内容
  - **例外**：上游 4xx/5xx 异常诊断时，可记录截断后的响应体，约束如下：
    - `status >= 400` 触发：5xx 记 Error（上游故障），4xx 记 Warn（客户端错误，但讯飞常把真实错误码包装在 400 body 里，需可见）；
    - 截断上限 8KB（`maxLogBodyBytes`），超长标 `truncated=true` 并记原始 `body_bytes`；
    - 不得包含认证头；body 读出后用 `NopCloser` 重置，保证透传字节级不变。
- ❌ 客户端 IP 的完整追踪（仅 `X-Forwarded-For` 由代理自动处理）

---

## Logger 初始化

```go
// cmd/gateway/main.go — 统一初始化
logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
    Level: cfg.LogLevel,
}))
slog.SetDefault(logger)
```

所有包通过参数接收 `*slog.Logger`，不使用全局 `slog.Info()`（main.go 中配置阶段除外，此时 logger 尚未初始化）。
