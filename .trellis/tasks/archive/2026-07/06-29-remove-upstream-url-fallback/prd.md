# 移除 UPSTREAM_URL 兜底，强制双协议上游显式配置

## Goal

删除 `UPSTREAM_URL` 兜底机制。网关是纯代理，上游目标必须准确；
兜底会让协议专用上游未配置时静默回退到错误上游，掩盖配置错误。

## Background

当前路由（README:29-30）：`/v1/messages` → Anthropic 上游，其余 → OpenAI 上游。
两个上游语义不同。`UPSTREAM_URL` 作为兜底存在隐患：
- 只配 `UPSTREAM_URL=https://api.openai.com` → Anthropic 协议请求静默发到 OpenAI 上游，
  启动不报错，运行时才坏。
- 强制 `UPSTREAM_OPENAI_URL` + `UPSTREAM_ANTHROPIC_URL` 显式配置 → 配错在启动即 fail。

## Requirements

- 删除 `UPSTREAM_URL` 环境变量及其兜底回退逻辑。
- `UPSTREAM_OPENAI_URL` 与 `UPSTREAM_ANTHROPIC_URL` 均为必填，缺一即启动失败。
- 同步更新所有引用：config.go、Makefile、start.bat、docker-compose.yml、.env.example、README.md。
- 错误信息明确指向缺失的协议专用上游（不再提 `UPSTREAM_URL`）。

## Acceptance Criteria

- [ ] `UPSTREAM_URL` 在代码与配置文件中不再出现。
- [ ] 仅配 `UPSTREAM_OPENAI_URL` 时启动报错，提示需配 `UPSTREAM_ANTHROPIC_URL`。
- [ ] 仅配 `UPSTREAM_ANTHROPIC_URL` 时启动报错，提示需配 `UPSTREAM_OPENAI_URL`。
- [ ] 两者均配齐时正常启动。
- [ ] `go build ./...` 与现有测试通过。
- [ ] README / .env.example / Makefile / start.bat / docker-compose.yml 与新行为一致。

## Notes

- 开发回显（Makefile 用 httpbin.org）由设一个变量改为设两个，可接受。
- 若未来出现"双协议聚合上游"场景，再重新引入统一上游变量（YAGNI）。
