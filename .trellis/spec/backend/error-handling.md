# Error Handling

> 启动阶段 fatal 退出；运行阶段记录日志 + 返回通用错误响应，不泄漏内部细节。

---

## Overview

两层错误处理策略：
1. **启动阶段**（配置加载）：直接 `slog.Error` + `os.Exit(1)`，无法运行就不启动。
2. **运行阶段**（代理转发）：记录详细日志，返回通用 JSON 错误，不暴露内部信息。

---

## Error Types

不定义自定义 error 类型。项目规模小，直接使用标准库 `fmt.Errorf` + `%w` 包装。

---

## Error Handling Patterns

### 启动阶段：fail fast

```go
// cmd/gateway/main.go — 配置错误直接退出
cfg, err := config.Load()
if err != nil {
    slog.Error("invalid config", "err", err.Error())
    os.Exit(1)
}
```

### 运行阶段：记录 + 通用响应

```go
// internal/proxy/proxy.go — ErrorHandler 不泄漏内部细节
ErrorHandler: func(w http.ResponseWriter, req *http.Request, err error) {
    logger.Error("upstream proxy error",
        "err", err.Error(),
        "method", req.Method,
        "path", req.URL.Path,
    )
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    w.WriteHeader(http.StatusBadGateway)
    _ = json.NewEncoder(w).Encode(map[string]string{
        "error": "upstream unreachable",
    })
},
```

### 配置校验：描述性错误

```go
// internal/config/config.go — 每个校验失败都给出可操作的提示
if upstream.Scheme != "http" && upstream.Scheme != "https" {
    return nil, fmt.Errorf("UPSTREAM_URL must use http or https scheme, got %q", upstream.Scheme)
}
```

---

## API Error Responses

统一 JSON 格式，Content-Type 为 `application/json; charset=utf-8`：

| 场景 | HTTP 状态码 | 响应体 |
|------|------------|--------|
| 上游不可达 | 502 | `{"error": "upstream unreachable"}` |
| 健康检查 | 200 | `{"status": "ok"}` |

**原则**：错误响应不包含堆栈、内部路径、上游地址等细节。

---

## Common Mistakes

- ❌ 在错误响应中泄漏上游 URL 或内部错误详情。
- ❌ 用 `panic` 处理可预见的错误（配置缺失、网络故障）。
- ❌ 吞掉错误（`_ = ...`）而不记录日志——唯一例外是 `json.NewEncoder(w).Encode` 的写入错误（客户端可能已断开）。
