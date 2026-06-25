# llm-gateway

一个**透明透传**的 LLM API 反向代理网关，用 Go 标准库实现，零第三方依赖。

按请求协议将流量分流到不同上游：

- `/v1/messages`（含 `/antigravity/v1/messages`）→ **Anthropic 上游**
- 其余路径（`/v1/chat/completions`、`/v1/responses`、未知路径等）→ **OpenAI 上游**

请求头与请求体**字节级原样透传**，支持高并发与 SSE 流式；并对上游「HTTP 200 + SSE error 帧」
的假成功做首帧 peek 拦截，命中规则时返回 503 让客户端自动重试。

## 核心特性

- **双上游按协议路由**：OpenAI / Anthropic 协议分别指向独立上游，未配置时回退到统一 `UPSTREAM_URL`。
- **不改变原内容**：业务头与请求体字节级透传，不解析、不重序列化。
- **SSE error 拦截**：上游返回 200 但 SSE 流内夹带 error 时，首帧 peek 命中规则即拦截为 503 + `Retry-After`，
  让 OpenAI/Anthropic SDK 自动重试；未命中则原样透传，不破坏流。
- **SSE 流式不变形**：`FlushInterval=-1` + 不设读写超时，逐 token 输出。
- **文件日志**：`LOG_DIR` 启用后同步写文件，按大小滚动 + 自动清理；为空则仅 stdout。
- **高并发**：每请求一个 goroutine + 上游连接池复用。
- **健壮性**：panic recovery 防止单请求拖垮进程；优雅关闭等待在途 SSE 完成。
- **零依赖**：仅用 Go 标准库（`net/http` + `httputil.ReverseProxy` + `log/slog`）。

## 路由规则

| 请求路径 | 目标上游 |
|---|---|
| 含 `/v1/messages` | `UPSTREAM_ANTHROPIC_URL`（缺省回退 `UPSTREAM_URL`） |
| 其余所有路径 | `UPSTREAM_OPENAI_URL`（缺省回退 `UPSTREAM_URL`） |

至少需配置 `UPSTREAM_URL`，或同时配置两个协议专用上游。

### 路径重写

根据上游 URL **是否以 `/` 结尾**，采用不同的拼接策略：

**以 `/` 结尾** → 剥离客户端路径的 `/v1` 前缀，拼接到上游路径后（适用于上游路径已含版本号）：

| 上游 URL | 客户端请求 | 实际上游路径 |
|---|---|---|
| `https://host/v2/` | `/v1/messages` | `/v2/messages` |
| `https://host/v2/` | `/v1/chat/completions` | `/v2/chat/completions` |
| `https://host/` | `/v1/messages` | `/messages` |

**不以 `/` 结尾** → 上游路径 + 客户端完整路径（适用于上游路径是前缀，如 `/anthropic`）：

| 上游 URL | 客户端请求 | 实际上游路径 |
|---|---|---|
| `https://host/anthropic` | `/v1/messages` | `/anthropic/v1/messages` |
| `https://host/anthropic` | `/v1/chat/completions` | `/anthropic/v1/chat/completions` |

**无路径**（仅 `scheme://host`）→ 客户端路径原样透传：

| 上游 URL | 客户端请求 | 实际上游路径 |
|---|---|---|
| `https://host` | `/v1/messages` | `/v1/messages` |
| `https://api.openai.com` | `/v1/chat/completions` | `/v1/chat/completions` |

## SSE 拦截规则

启用 `SSE_INTERCEPT_ENABLED`（默认开）时，对上游 200+SSE 响应做首帧 peek，
命中下表任一规则即拦截为 503 + `Retry-After`：

| 命中条件 | 来源 |
|---|---|
| `code=10012` 且 message 含 `EngineInternalError` + `1105` | 引擎内部错误 |
| `code=10010` 且 message 含 `RecvFromEngineError` + `Engine Busy` | 引擎忙 |
| `code=11210` 且 message 含 `NotEnoughCvError` | 并发/容量不足 |
| Anthropic `overloaded_error` | 上游过载 |

拦截响应体为通用 JSON（`upstream_overloaded`），不泄漏上游供应商标识。
关闭拦截（`SSE_INTERCEPT_ENABLED=false`）则全部走纯透传，不做 peek。

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
| `UPSTREAM_URL` | — | 兜底默认上游；协议专用上游未配置时使用 |
| `UPSTREAM_OPENAI_URL` | — | OpenAI 协议专用上游（可选，回退 `UPSTREAM_URL`） |
| `UPSTREAM_ANTHROPIC_URL` | — | Anthropic 协议专用上游（可选，回退 `UPSTREAM_URL`） |
| `LISTEN_ADDR` | `:8080` | 监听地址 |
| `READ_HEADER_TIMEOUT` | `10s` | 读头超时（防慢速攻击） |
| `MAX_IDLE_CONNS_PER_HOST` | `100` | 到上游的每主机空闲连接数 |
| `IDLE_CONN_TIMEOUT` | `90s` | 上游空闲连接超时 |
| `UPSTREAM_INSECURE_SKIP_VERIFY` | `false` | 跳过上游 TLS 校验（仅调试） |
| `PRESERVE_HOST` | `false` | 保留客户端 Host（默认用上游 Host） |
| `LOG_LEVEL` | `info` | 日志级别：debug/info/warn/error |
| `LOG_DIR` | — | 日志文件目录；非空启用文件日志（同步 stdout + 文件） |
| `LOG_MAX_SIZE` | `100` | 单个日志文件最大 MB，超过自动滚动 |
| `LOG_MAX_BACKUPS` | `7` | 保留旧日志文件数；0 保留全部 |
| `LOG_MAX_AGE` | `0` | 旧日志保留天数；0 不按天清理 |
| `LOG_COMPRESS` | `true` | 是否 gzip 压缩旧日志 |
| `SSE_INTERCEPT_ENABLED` | `true` | 是否启用 SSE error 拦截（关闭则纯透传） |
| `SSE_RETRY_AFTER` | `5` | 拦截 503 响应的 `Retry-After` 秒数 |

## 快速开始

### Docker Compose（推荐）

```bash
# 1. 在项目根目录创建 .env（已被 .gitignore 忽略），填入上游地址：
#    UPSTREAM_OPENAI_URL=https://api.openai.com
#    UPSTREAM_ANTHROPIC_URL=https://api.anthropic.com

# 2. 构建并启动：
docker compose up -d --build

# 3. 验证：
curl http://localhost:8080/health
```

日志写入挂载卷 `./logs`（容器以非 root 运行，若报权限错误执行 `chown -R 1000:1000 ./logs`）。

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
docker run --rm -p 8080:8080 \
  -e UPSTREAM_OPENAI_URL=https://api.openai.com \
  -e UPSTREAM_ANTHROPIC_URL=https://api.anthropic.com \
  llm-gateway:latest
```

或拉取 GHCR 发布镜像（`v*` tag 触发自动发布）：

```bash
docker pull ghcr.io/redcath/llm-gateway:latest
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

### 3. 双上游路由验证

将 `UPSTREAM_OPENAI_URL` 与 `UPSTREAM_ANTHROPIC_URL` 指向不同上游，分别请求两条路径，
确认按协议分流（日志会打印命中的 `upstream_openai` / `upstream_anthropic`）：

```bash
curl -s http://localhost:8080/v1/chat/completions ...   # → OpenAI 上游
curl -s http://localhost:8080/v1/messages ...           # → Anthropic 上游
```

### 4. SSE 流式验证

将上游指向任意支持 SSE 的端点，用 `curl -N` 观察：

```bash
curl -N http://localhost:8080/<sse-endpoint>
# 应逐行实时输出，而非一次性缓冲返回
```

### 5. SSE 拦截验证

上游返回 200 但 SSE 首帧为命中规则的 error 时，网关应改返回 503 + `Retry-After`：

```bash
curl -i http://localhost:8080/<sse-endpoint>
# HTTP/1.1 503 Service Unavailable
# Retry-After: 5
```

### 6. 并发验证

```bash
hey -n 1000 -c 50 http://localhost:8080/anything
# 确认无连接错误
```

## 项目结构

```
├── cmd/gateway/main.go            # 入口：配置加载 + logger 装配 + 优雅启停
├── internal/
│   ├── config/config.go           # 环境变量加载与校验
│   ├── routing/routing.go         # 按路径选择 OpenAI / Anthropic 上游
│   ├── proxy/proxy.go             # ReverseProxy 纯透传（拦截关闭时回退）
│   ├── sse/
│   │   ├── proxy.go               # SSE 首帧 peek + 拦截/透传
│   │   ├── handler.go             # 命中决策（503 + Retry-After）+ DefaultRules
│   │   └── rule.go                # 规则匹配（Code / ErrorType / MsgContains）
│   ├── logdir/logdir.go           # 文件日志 handler（滚动 + stdout 多路输出）
│   └── server/
│       ├── server.go              # http.Server 装配（/health + 透传）
│       └── logging.go             # 访问日志 + panic recovery
```
