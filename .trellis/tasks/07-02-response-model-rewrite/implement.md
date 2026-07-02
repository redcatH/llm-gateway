# 执行计划：响应 model 字段改写

## 有序检查清单

### 1. 配置层
- [ ] `internal/config/config.go`：新增 `ModelRewriteMode string`、`ModelMap map[string]string`、`ModelDefault string` 字段。
- [ ] 新增 `parseModelMap(key string) (map[string]string, error)`：解析 `MODEL_MAP`（`,` 分条目，`:` 分 key/value）。
- [ ] `Load()` 集成三字段 + 校验：`ModelRewriteMode` ∈ {`off`,`passthrough`,`default`}（空值默认 `off`，非法报错）；`default` 模式要求 `ModelDefault` 非空。
- [ ] `internal/config/config_test.go`：覆盖三模式解析、映射表解析、空值、格式非法、`default` 模式缺 `ModelDefault` 报错。

### 2. 改写核心
- [ ] 新建 `internal/sse/rewrite.go`：模式常量（`ModeOff`/`ModePassthrough`/`ModeDefault`）、`RewriteConfig` 结构、`mapModel`、`rewriteModelInPlace`、`rewriteModelJSON`、`rewriteSSEEvent`（均按 mode 决定未命中行为）。
- [ ] `internal/sse/rewrite_test.go`：覆盖 OpenAI/Anthropic 各种 JSON 结构、三模式未命中行为、解析失败、无 model 字段、`off` 关闭。

### 3. 流式改写
- [ ] `internal/sse/proxy.go`：`readFirstEvent` 复用/重命名为 `readEvent`（首帧与后续帧共用）。
- [ ] 重构 `forwardStream` → 支持改写（首帧改写 + 逐事件读改写 + flush），新增 `modelMap/defaultModel/logger` 参数。
- [ ] `handleSSEResponse` 透传改写参数到 `forwardStream`。

### 4. 非流式改写
- [ ] `internal/sse/proxy.go`：新增 `isJSONResponse` 辅助。
- [ ] `ProxyHandler` 非流式 2xx 分支接入改写（读全量 + 改写 + `Del("Content-Length")`）。

### 5. 集成
- [ ] `ProxyHandler` 签名扩展（新增 `RewriteConfig` 参数）。
- [ ] `internal/server/server.go` / `cmd/gateway/main.go`：从 config 构造 `RewriteConfig` 并传入。
- [ ] 更新调用方与现有测试的签名。

### 6. 测试与回归
- [ ] `internal/sse/proxy_test.go`：新增改写用例（四种组合 × 三模式 + 流式多 chunk + 非流式 Content-Length）。
- [ ] error 拦截回归：现有拦截/dump 用例行为不变。
- [ ] 功能关闭回归：`MODEL_REWRITE_MODE=off` 时原样透传。

### 7. 文档
- [ ] `README.md`：新增 `MODEL_MAP`、`MODEL_DEFAULT` 环境变量说明。

## 验证命令

```bash
go test ./internal/config/... ./internal/sse/...
go vet ./...
go build ./...
```

## Review 门

- 步骤 5 完成后：本地跑一次真实上游请求（OpenAI/Anthropic × 流式/非流式），确认 model 改写正确 + error 拦截不破。
- 步骤 6 全绿后再进入收尾。

## 回滚点

- 改写功能由 `MODEL_MAP`/`MODEL_DEFAULT` 开关控制；清空这两个环境变量即恢复纯透传，无需回滚代码。
- 若需代码级回滚，改动集中在 `rewrite.go`（新文件）+ `proxy.go`/`config.go` 的扩展点，diff 局部。
