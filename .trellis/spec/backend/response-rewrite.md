# Response Model Rewrite Spec

> 响应方向 `model` 字段改写的可执行契约。**请求方向零改动**，仅改响应。

---

## 1. Scope / Trigger

- **Trigger**：新增响应改写功能，涉及 `config`（env）→ `main` → `sse` 跨层契约，且改变响应 body（非透传）。
- **适用**：OpenAI `/v1/chat/completions` + Anthropic `/v1/messages`，流式与非流式。
- **不适用**：`/v1/responses`、Azure 协议、请求方向、4xx/5xx 错误响应、非 JSON 2xx 响应。

---

## 2. Signatures

```go
// internal/sse/rewrite.go
type RewriteConfig struct {
    Mode    string            // "off" / "passthrough" / "default"
    Map     map[string]string // 真名 → 对外名
    Default string            // default 模式兜底名
}

func (rc RewriteConfig) enabled() bool                      // Mode 为 passthrough/default
func mapModel(real string, rc RewriteConfig) string         // 命中→Map；未命中→按 mode
func rewriteModelJSON(data []byte, rc RewriteConfig) []byte // 递归改写所有 model 字段
func rewriteSSEEvent(event []byte, rc RewriteConfig) []byte // 改写 SSE 事件内 data 行

// internal/sse/proxy.go
func ProxyHandler(..., rc RewriteConfig) http.Handler
func forwardStream(w, resp, peeked, br, rc)   // 流式：逐事件改写
func copyResponseRewritten(w, resp, rc)       // 非流式 2xx JSON：读全量改写

// internal/config/config.go
func parseModelMap(key string) (map[string]string, error)
func parseRewriteMode(key string) (string, error)
```

---

## 3. Contracts

### 环境变量

| Key | 类型 | 必填 | 说明 |
|-----|------|------|------|
| `MODEL_REWRITE_MODE` | enum | 否（默认 `off`） | `off` / `passthrough` / `default`，大小写不敏感 |
| `MODEL_MAP` | map | 否 | `key:value,key:value`，按**第一个** `:` 分割，value 允许含 `:` |
| `MODEL_DEFAULT` | string | 条件必填 | `default` 模式下必填 |

### 改写位置（位置驱动，非类型驱动）

| 协议 | 流式 | 非流式 |
|------|------|--------|
| OpenAI | 每个 chunk 顶层 `model` | 顶层 `model` |
| Anthropic | 仅 `message_start` 的 `message.model` | 顶层 `model` |

### 未命中行为

| Mode | 命中 `MODEL_MAP` | 未命中 |
|------|------------------|--------|
| `off`（默认） | 不改写 | 不改写 |
| `passthrough` | 改写为对外名 | 透传真名 |
| `default` | 改写为对外名 | 改写为 `MODEL_DEFAULT` |

---

## 4. Validation & Error Matrix

| 条件 | 行为 |
|------|------|
| `MODEL_REWRITE_MODE` 非法值 | `Load` 报错，进程退出 |
| `MODE=default` + `MODEL_DEFAULT` 为空 | `Load` 报错 |
| `MODEL_MAP` 条目缺 `:` 或空 key | `Load` 报错 |
| `MODEL_MAP` value 含 `:` | 合法（按第一个 `:` 分割） |
| 响应 JSON 解析失败 | 原样透传该帧/body，不中断流 |
| 响应无 `model` 字段 | 原样透传 |
| 4xx/5xx 响应 | 不改写，透传 |
| 2xx 非 JSON 响应 | 不改写，透传 |
| `SSE_INTERCEPT_ENABLED=false` | 改写不生效（走纯透传回退 `proxy.New`） |
| 改写后 body 长度变化 | `Del("Content-Length")`，Go 自动定界 |

---

## 5. Good / Base / Bad Cases

- **Good**：`MODEL_MAP=xopglm51:glm-5.1,xopglm52:glm-5.2` + `MODE=default` + `MODEL_DEFAULT=glm-default` → 命中改对外名，未命中改 `glm-default`，真名永不泄漏。
- **Base**：`MODE=passthrough` + `MODEL_MAP=...` → 命中改，未命中透传真名（灰度/调试用，可观察未覆盖的真名）。
- **Bad**：`MODE=default` 但未设 `MODEL_DEFAULT` → 启动失败（避免运行时未命中无值可填）。

---

## 6. Tests Required

| 测试 | 断言点 |
|------|--------|
| `TestParseModelMap` | 空/单/多/空格修剪/value 含冒号/缺冒号报错/空 key 报错 |
| `TestParseRewriteMode` | 空→off、三合法值、非法报错、大写归一化 |
| `TestLoadModelRewriteConfig` | `default` 缺 `MODEL_DEFAULT` 报错；未设默认 off |
| `TestRewriteModelJSON` | OpenAI 顶层 / Anthropic `message.model` / 未命中 default / 解析失败透传 / off 关闭 / 其他字段保留 |
| `TestRewriteSSEEvent` | data 改写 / `event:` 行保留 / 无 model 字节级 / `[DONE]` / CRLF 输入 |
| `TestModelRewrite` | 四组合 × 三模式端到端 + `reasoning_content` 思考模式 |
| `TestModelRewriteDoesNotBreakErrorIntercept` | 改写启用时 SSE error 拦截仍返回 503 |

---

## 7. Wrong vs Correct

### Wrong：按 chunk 类型分支改写

```go
// 为每种 chunk 类型写分支 → 上游新增类型（audio/refusal/...）即漏改
switch chunkType {
case "content":        rewriteModel(chunk)
case "reasoning":      rewriteModel(chunk)
case "tool_calls":     rewriteModel(chunk)
}
```

### Correct：位置驱动 + 递归

```go
// 递归找所有 "model" 字段改写，任何 chunk 类型自动覆盖
func rewriteModelInPlace(v any, rc RewriteConfig) bool {
    switch val := v.(type) {
    case map[string]any:
        for k, item := range val {
            if k == "model" {
                if s, ok := item.(string); ok {
                    val[k] = mapModel(s, rc)
                }
                continue
            }
            rewriteModelInPlace(item, rc)
        }
    case []any:
        for _, item := range val {
            rewriteModelInPlace(item, rc)
        }
    }
}
```

**Why**：上游 chunk 类型会持续增长，位置驱动一次覆盖全部，无需维护类型分支表。

---

## Common Mistake：readEvent 吞 EOF 导致死循环

**Symptom**：`forwardStream` 改写启用后测试 30s 超时，goroutine 卡在 `readEvent` → `bufio.ReadBytes`。

**Cause**：`readEvent` 原逻辑先判"空行=事件边界"返回 `(buf, nil)`，再判 `err`。EOF 时 `ReadBytes` 返回 `(空, io.EOF)`，被空行分支吞掉，返回 `(空, nil)`；`forwardStream` 循环永远拿不到 EOF，死等下一个事件。原 `readFirstEvent` 只调一次（peek 首帧）未暴露，循环调用即触发。

**Fix**：EOF/错误检查必须**优先于**空行边界判断。

```go
// Correct：EOF 优先
if err != nil {
    return buf.Bytes(), err
}
if len(bytes.TrimRight(line, "\r\n")) == 0 {
    return buf.Bytes(), nil // 空行 = 事件边界
}
```

**Prevention**：任何"循环读取 + 边界判断"的流解析器，EOF 必须最先返回，否则会吞掉流终止信号导致死循环。
