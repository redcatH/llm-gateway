# 响应 model 字段改写以隐藏上游真实模型名

## Goal

网关在透传上游（讯飞）响应时，将响应中的 `model` 字段从上游真实模型名（如 `xopglm51`、`xopglmv47flash`）改写为对外展示名（如 `glm-5.1`、`glm-4.7-flash`），使下游客户端无法获知实际使用的上游模型。**仅改响应方向，请求方向保持透传不变。**

## Background

- 当前网关为纯透传（仅 SSE error 帧首帧 peek 拦截），响应原样下发，`model` 字段会暴露上游真实模型代号。
- 下游客户端只能发送上游真实模型名（透传项目约束），故请求方向不做反向映射，仅在响应方向改写（**路线 B**）。
- 上游真实模型名多样且会增长（已观测到 `xopglm51` / `xopglm52` / `xopglmv47flash` 三种），需可配置映射表 + 未命中兜底。

## Requirements

### 功能需求

- 仅修改响应中的 `model` 字段；其余字段（`id` / `object` / `role` / `content` / `delta` / `usage` / `choices` / `finish_reason` / `stop_reason` / `tool_calls` / `reasoning_content` / `thinking` 等）原样保留，不改变响应结构。
- 覆盖四种响应组合：
  - OpenAI `/v1/chat/completions` 流式：每个 SSE chunk 顶层 `model`
  - OpenAI `/v1/chat/completions` 非流式：顶层 `model`
  - Anthropic `/v1/messages` 流式：仅 `message_start` 事件的 `data.message.model`（其余事件无 model，透传）
  - Anthropic `/v1/messages` 非流式：顶层 `model`
- 思考模式（OpenAI `reasoning_content`、Anthropic `thinking`）、tool_calls、refusal、include_usage 等各类帧/字段均通过"位置驱动"统一覆盖，无需为每种类型单独处理。
- 改写映射由环境变量 `MODEL_MAP` 配置，格式 `真名:对外名,真名:对外名`（如 `xopglm51:glm-5.1,xopglm52:glm-5.2,xopglmv47flash:glm-4.7-flash`）。
- 改写行为由环境变量 `MODEL_REWRITE_MODE` 显式控制，三种模式：
  - `off`：关闭改写，所有响应纯透传（默认，向后兼容）。
  - `passthrough`：命中 `MODEL_MAP` 的改写为对外名，未命中的原样透传真名。
  - `default`：命中 `MODEL_MAP` 的改写为对外名，未命中的改写为 `MODEL_DEFAULT` 配置的兜底名。
- `MODEL_DEFAULT` 仅 `default` 模式使用（该模式下必填，启动校验）。
- 仅对 2xx 成功响应改写；4xx/5xx 错误响应原样透传，不改写。
- 请求方向零改动（下游发真名，网关透传给上游）。

### 非功能需求

- 不破坏现有 SSE error 拦截功能（503/400 拦截、10012 请求体 dump 等）。
- 流式响应保持逐帧 flush，不缓冲，首字节延迟无可感知变化。
- JSON 解析失败的帧保守原样透传，不吞流、不中断。
- 非流式响应改写后 body 长度变化，必须正确处理 `Content-Length`（删除后由 Go 自动定界），下游不截断/挂起。
- `MODEL_REWRITE_MODE=off`（默认）时改写功能完全关闭，行为与现状一致（向后兼容）。

## Acceptance Criteria

- [ ] 配置 `MODEL_MAP` 后，OpenAI/Anthropic 流式与非流式四种响应的 `model` 均被改写为对应对外名，其余字段不变。
- [ ] 思考模式（OpenAI `reasoning_content`、Anthropic `thinking`）响应的 model 正确改写，思考内容字节不受影响。
- [ ] OpenAI 流式的 tool_calls / finish / include_usage 等各类 chunk 的 model 均改写。
- [ ] `passthrough` 模式：命中改写，未命中原样透传真名。
- [ ] `default` 模式：命中改写，未命中替换为 `MODEL_DEFAULT`，响应中不出现真实模型名。
- [ ] 4xx/5xx 错误响应原样透传，不做改写。
- [ ] 现有 SSE error 拦截功能行为不变（拦截、放行、dump 路径均回归通过）。
- [ ] 流式响应逐帧下发，下游收到的时序与改写前一致。
- [ ] 非流式响应下游能完整读取 body，无截断/挂起/unexpected EOF。
- [ ] JSON 解析失败的帧原样透传，不中断流。
- [ ] `MODEL_REWRITE_MODE=off` 时所有响应原样透传（功能关闭）。
- [ ] 新增单元测试覆盖：四种响应组合、未命中、解析失败、error 拦截回归、配置解析。

## Out of Scope

- 请求方向的反向 model 映射（下游发真名，无需反向）。
- `/v1/responses`、Azure OpenAI 特殊协议、其它第三方自定义协议。
- `system_fingerprint` 等非 model 指纹字段的处理。
- model 字段以外的任何响应内容改写。
