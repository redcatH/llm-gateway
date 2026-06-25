# xunfei-gateway

一个**透明透传**的 LLM API 反向代理网关，用 Go 标准库实现，零第三方依赖。

处理 `/v1/chat/completions`（OpenAI 协议）与 `/v1/messages`（Anthropic 协议），
将**所有请求头与请求体原样**转发到单一固定上游，支持高并发与 SSE 流式。

## 核心特性

- **不改变原内容**：请求头（业务头）与请求体字节级透传，不做任何改写。
- **SSE 流式不变形**：`FlushInterval=-1` + 不设读写超时，流式响应逐 token 输出。
- **高并发**：每请求一个 goroutine + 上游连接池复用。
- **零依赖**：仅用 Go 标准库（`net/http` + `httputil.ReverseProxy` + `log/slog`）。
- **优雅关闭**：收到信号后等待在途请求（含进行中的 SSE）完成。

## 关于"不改变内容"的边界

- 所有业务头（`Authorization`、`x-api-key`、`anthropic-version`、`anthropic-beta`、
  `Content-Type`、`User-Agent` 及任意自定义头）**原样透传**。
- 按 RFC 7230，连接控制头（`Connection`、`Keep-Alive`、`Transfer-Encoding`、
  `Upgrade`、`Proxy-Authorization` 等 hop-by-hop 头）会被自动剥离——这是协议对代理的
  强制要求，不属于"业务内容"。
- 默认会追加 `X-Forwarded-For`（客户端 IP），这是标准代理行为，原头仍保留。
- 请求体以 `io.Reader` 流式透传，不读入内存、不解析、不重序列化。

## 配置（环境变量）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `UPSTREAM_URL` | （必填） | 上游地址，如 `https://api.example.com` |
| `LISTEN_ADDR` | `:8080` | 监听地址 |
| `READ_HEADER_TIMEOUT` | `10s` | 读头超时（防慢速攻击） |
| `MAX_IDLE_CONNS_PER_HOST` | `100` | 到上游的每主机空闲连接数 |
| `IDLE_CONN_TIMEOUT` | `90s` | 上游空闲连接超时 |
| `UPSTREAM_INSECURE_SKIP_VERIFY` | `false` | 跳过上游 TLS 校验（仅调试） |
| `PRESERVE_HOST` | `false` | 保留客户端 Host（默认用上游 Host） |
| `LOG_LEVEL` | `info` | 日志级别：debug/info/warn/error |

## 快速开始

### 本地运行

```bash
# 用 httpbin 作上游，便于回显验证
make run
# 或直接
UPSTREAM_URL=https://httpbin.org go run ./cmd/gateway
```

### 构建

```bash
make build        # 产出 bin/gateway
./bin/gateway     # 需提供 UPSTREAM_URL
```

### Docker

```bash
make docker
docker run --rm -p 8080:8080 -e UPSTREAM_URL=https://api.example.com xunfei-gateway:latest
```

## 验证

### 1. 健康检查

```bash
curl -i http://localhost:8080/health
# HTTP/1.1 200 OK
# {"status":"ok"}
```

### 2. Header + Body 透传验证（关键）

用 httpbin 的 `/anything` 端点（回显收到的请求），确认头与体未被篡改：

```bash
curl -s http://localhost:8080/anything \
  -H "Authorization: Bearer sk-test" \
  -H "anthropic-version: 2023-06-01" \
  -H "X-Custom: preserved" \
  -d '{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}'
```

回显 JSON 中的 `headers` 应包含原样的 `Authorization`、`Anthropic-Version`、`X-Custom`，
`json` 字段应与发送的 body 完全一致。

### 3. SSE 流式验证

将 `UPSTREAM_URL` 指向任意支持 SSE 的上游，用 `curl -N` 观察：

```bash
curl -N http://localhost:8080/<sse-endpoint>
# 应逐行实时输出，而非一次性缓冲返回
```

### 4. 并发验证

```bash
hey -n 1000 -c 50 http://localhost:8080/anything
# 确认无连接错误
```

## 项目结构

```
├── cmd/gateway/main.go          # 入口：配置加载 + 优雅启停
└── internal/
    ├── config/config.go         # 环境变量加载与校验
    ├── proxy/proxy.go           # ReverseProxy 构建（透明转发核心）
    └── server/
        ├── server.go            # http.Server 装配（/health + 透传）
        └── logging.go           # slog 访问日志（透传 Flusher/Hijacker）
```
