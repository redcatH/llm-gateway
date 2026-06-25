# Quality Guidelines

> 零依赖、标准库优先、环境变量配置、go vet 检查。

---

## Overview

项目核心质量标准：**零第三方依赖**，仅用 Go 标准库。
代码风格简洁，注释用中文，函数短小职责单一。

---

## Forbidden Patterns

- ❌ 引入第三方依赖（viper、gin、logrus 等）——项目刻意保持零依赖。
- ❌ 硬编码配置值——所有可配置项通过环境变量注入。
- ❌ 设置 `ReadTimeout`/`WriteTimeout`——会切断 SSE 长连接。
- ❌ 在代理层修改请求头或请求体——违反透明透传原则。
- ❌ `panic` 处理可预见错误。
- ❌ `console.log` 等价物——生产代码不用 `fmt.Println`，统一用 `slog`。

---

## Required Patterns

- ✅ 环境变量配置 + 合理默认值（见 `internal/config/config.go` 的 `envXxx` 辅助函数）。
- ✅ 优雅关闭：`signal.Notify` + `srv.Shutdown(ctx)` + 超时等待。
- ✅ `statusRecorder` 包装 `ResponseWriter` 时透传 `Flusher` + `Hijacker` 接口。
- ✅ 错误响应不泄漏内部细节（通用消息 + 详细日志）。
- ✅ Docker 多阶段构建，非 root 用户运行。
- ✅ `.env` 在 `.gitignore` 中（含密钥禁止提交）。

---

## Testing Requirements

当前无测试文件。新增测试时：

- 使用标准库 `testing` 包，不引入 testify 等框架。
- 测试文件与源文件同目录：`proxy_test.go` 放在 `internal/proxy/`。
- 优先测试：配置校验、代理错误处理、statusRecorder 行为。
- 运行：`go test ./...`

---

## Code Review Checklist

- [ ] 是否引入了新的第三方依赖？（应避免）
- [ ] 配置项是否通过环境变量暴露？（应暴露）
- [ ] 错误响应是否泄漏内部信息？（不应泄漏）
- [ ] 日志是否使用 slog 结构化格式？（应使用）
- [ ] SSE 流式是否被超时设置破坏？（不应破坏）
- [ ] `go vet ./...` 是否通过？（应通过）
- [ ] Dockerfile 是否以非 root 运行？（应如此）

---

## Build & Verify Commands

```bash
make vet      # 静态检查
make build    # 编译
make run      # 本地运行（httpbin 上游）
make docker   # Docker 构建
```
