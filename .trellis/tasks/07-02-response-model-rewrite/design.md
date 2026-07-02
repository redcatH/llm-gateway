# 技术设计：响应 model 字段改写

## 边界与范围

- **改写介入点**：仅响应方向的透传路径
  - 流式：`forwardStream`（重构为逐事件改写）
  - 非流式：`copyResponse` 的 2xx JSON 分支（读全量 + 改写 + 删 Content-Length）
- **不动**：
  - 请求方向（`buildUpstreamRequest` 原样转发请求体）
  - SSE error 拦截路径（`writeDecision` 返回的自造 503/400 body 无 model 字段）
  - `logNonSuccessResponse`（5xx 日志路径）

## 配置层（internal/config/config.go）

新增三个配置字段：

| 字段 | 环境变量 | 类型 | 说明 |
|------|----------|------|------|
| `ModelRewriteMode` | `MODEL_REWRITE_MODE` | string，取值 `off`/`passthrough`/`default` | 改写模式，默认 `off` |
| `ModelMap` | `MODEL_MAP` | `map[string]string` | 真名→对外名映射，格式 `key:value,key:value` |
| `ModelDefault` | `MODEL_DEFAULT` | `string` | `default` 模式下的兜底对外名 |

- 新增 `parseModelMap(key string) (map[string]string, error)`：按 `,` 分割条目，每条按 `:` 分割 key/value。空字符串返回空 map（不报错）。
- `ModelRewriteMode` 直接读取并校验值 ∈ {`off`,`passthrough`,`default`}，空值默认 `off`，非法值 `Load` 返回错误。
- **启动校验**：`default` 模式下 `ModelDefault` 必须非空，否则 `Load` 返回错误；`passthrough`/`default` 模式下 `ModelMap` 为空时记 warning（允许启动）。
- 标准库 `strings.Split` 即可，零新依赖。

## 改写核心（internal/sse/rewrite.go，新建）

定义模式字符串常量与配置结构：

```go
const (
    ModeOff         = "off"
    ModePassthrough = "passthrough"
    ModeDefault     = "default"
)

type RewriteConfig struct {
    Mode    string            // off / passthrough / default
    Map     map[string]string // 真名→对外名
    Default string            // default 模式的兜底名
}
```

### mapModel

```go
// mapModel 将真实模型名映射为对外名。命中映射表返回对外名；
// 未命中按 mode 处理：passthrough 返回原值，default 返回 Default。
func mapModel(real string, rc RewriteConfig) string {
    if mapped, ok := rc.Map[real]; ok {
        return mapped
    }
    if rc.Mode == ModeDefault {
        return rc.Default // default 模式下 Default 已由 Load 校验非空
    }
    return real // passthrough
}
```

### rewriteModelJSON

通用 JSON 改写：解析为 `any`，递归遍历所有 `model` 字段（字段名在 LLM 响应中唯一，递归安全——`content`/`tool_calls` 等结构不含 model 字段），改写后重序列化。

```go
// rewriteModelJSON 解析 JSON，递归改写所有 model 字段，重序列化返回。
// 解析失败 / 无 model 字段 / 无需改写时原样返回 data（保守透传）。
// Mode==off 时直接返回 data（功能关闭）。
func rewriteModelJSON(data []byte, rc RewriteConfig) []byte {
    if rc.Mode == ModeOff {
        return data
    }
    var v any
    if json.Unmarshal(data, &v) != nil {
        return data // 解析失败，原样透传
    }
    if !rewriteModelInPlace(v, rc) {
        return data // 无 model 字段或无需改
    }
    out, err := json.Marshal(v)
    if err != nil {
        return data
    }
    return out
}
```

`rewriteModelInPlace(v any, rc RewriteConfig) bool`：递归遍历 `map[string]any`，遇 `key=="model"` 且值为 string 时调 `mapModel` 改写；返回是否有改动。

**字节变化说明**：重序列化会改变字段顺序（Go `map[string]any` Marshal 按 key 字典序）与空格。LLM SDK 只认 JSON 语义、不依赖字节顺序，可接受。不保序（保序需 token 级编辑，复杂度不值得）。

## 流式改写（重构 forwardStream）

当前 `forwardStream(w, resp, peeked, br)` 逐 4096 字节块读 + flush。重构为逐 SSE 事件改写：

```go
func forwardStream(w, resp, peeked, br, rc RewriteConfig) {
    copyHeader(w.Header(), resp.Header)
    w.Header().Del("Content-Length") // 流式本无此头，Del 为 no-op，统一处理
    w.WriteHeader(resp.StatusCode)
    flusher, _ := w.(http.Flusher)
    // 首帧（已 peek）：含 model 则改写后写出，否则原样
    if len(peeked) > 0 {
        w.Write(rewriteSSEEvent(peeked, rc))
        flush(flusher)
    }
    // 后续：逐事件读，每个事件按需改写后写出 flush
    for {
        event, err := readEvent(br) // 复用按空行边界读逻辑
        if len(event) > 0 {
            w.Write(rewriteSSEEvent(event, rc))
            flush(flusher)
        }
        if err != nil { break }
    }
}
```

- `rewriteSSEEvent(event, rc)`：遍历事件的每个 `data:` 行，对每行 payload 调 `rewriteModelJSON`；非 data 行（如 `event:`）原样。用 `bytes.Contains(event, "model")` 短路：不含 model 的事件直接返回原字节，零解析（Anthropic 的 `content_block_delta` 等大量帧走快路径）。
- `readFirstEvent` 复用/重命名为 `readEvent`（读到空行边界返回一个事件），首帧和后续帧共用。

**与 error 拦截共存**（handleSSEResponse 内）：
1. `readEvent` peek 首帧 → `parseErrorEvent` 判断 error（读操作，不改字节）。
2. 命中规则 → `writeDecision` 拦截（自造 body 无 model，不动）。
3. 否则 → `forwardStream` 改写转发（首帧 + 后续逐事件改写）。
- error 帧无 model 字段，`rewriteSSEEvent` 自动跳过；正常帧无 error，`parseErrorEvent` 自动跳过。两者字段正交，顺序无冲突。
- 复用同一 `peeked` 与 `bufio.Reader`，不二次缓冲。

## 非流式改写（ProxyHandler 2xx JSON 分支）

```go
// 非流式 2xx + 启用改写 + JSON 响应
if resp.StatusCode == 200 && rc.Mode != ModeOff && isJSONResponse(resp) {
    body, _ := io.ReadAll(resp.Body)
    out := rewriteModelJSON(body, rc)
    copyHeader(w.Header(), resp.Header)
    w.Header().Del("Content-Length") // body 长度已变，删掉让 Go 自动定界
    w.WriteHeader(resp.StatusCode)
    w.Write(out)
    return
}
copyResponse(w, resp) // 其余原样透传
```

- `isJSONResponse`：Content-Type 含 `application/json`。非 JSON（如 HTML 错误页）不改写。
- `Del("Content-Length")` 后 Go 自动用 chunked 或重算，下游 SDK 均支持。
- 5xx 路径仍走 `logNonSuccessResponse` + `copyResponse`，不接入改写。

## ProxyHandler 签名扩展

`ProxyHandler` 新增 `rc RewriteConfig` 参数。`internal/server/server.go` / `cmd/gateway/main.go` 从 `config.Load()` 构造 `sse.RewriteConfig{Mode, Map, Default}` 并传入。

## 性能

- `bytes.Contains` 短路过滤无 model 的帧（Anthropic 流式几乎全走快路径）。
- 流式逐事件 flush，不缓冲，TTFB 无感。
- 映射表启动时构建，请求时只读 map 查询 O(1)，无锁。
- JSON 解析微秒级，相对上游秒级延迟 < 0.1%，非瓶颈。

## 向后兼容

- `MODEL_REWRITE_MODE` 未设置（默认 `off`）→ 改写功能关闭，所有响应原样透传，行为与现状完全一致。
- 改写逻辑通过 `Mode==off` 短路，零开销。

## 风险与缓解

| 风险 | 缓解 |
|------|------|
| 重序列化改变字节顺序 | SDK 不依赖字节顺序，可接受；不保序 |
| 上游新增未观测的真名 | `default` 模式 `MODEL_DEFAULT` 兜底不泄漏；`passthrough` 模式按需选 |
| JSON 解析失败 | 保守原样透传，不吞流 |
| 破坏 error 拦截 | 字段正交，不改 error 路径；回归测试覆盖 |
| Content-Length 错乱 | 改写路径统一 `Del("Content-Length")` |
