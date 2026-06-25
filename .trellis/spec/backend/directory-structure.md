# Directory Structure

> Go 标准布局：cmd/ 入口 + internal/ 内部包，零第三方依赖。

---

## Overview

本项目遵循 Go 标准项目布局，使用 `cmd/` 放入口、`internal/` 放不可导出的业务包。
不使用 `pkg/`、`api/`、`web/` 等目录——项目规模小，扁平结构足够。

---

## Directory Layout

```
xunfei-gateway/
├── cmd/
│   └── gateway/
│       └── main.go              # 入口：配置加载 + 优雅启停
├── internal/
│   ├── config/
│   │   └── config.go            # 环境变量加载与校验
│   ├── proxy/
│   │   └── proxy.go             # ReverseProxy 构建（透明转发核心）
│   └── server/
│       ├── server.go            # http.Server 装配（/health + 透传）
│       └── logging.go           # slog 访问日志（透传 Flusher/Hijacker）
├── bin/                         # 构建产物（.gitignore）
├── Dockerfile                   # 多阶段构建
├── Makefile                     # build/run/vet/docker 命令
├── go.mod                       # 零第三方依赖
└── README.md
```

---

## Module Organization

- **一个 internal 包 = 一个职责**：`config` 管配置、`proxy` 管转发、`server` 管 HTTP 服务。
- 包之间通过接口（函数签名）依赖，不定义 interface 除非有多实现需求。
- 新增功能时：先在 `internal/` 下建新包，再在 `cmd/gateway/main.go` 中组装。

---

## Naming Conventions

- 包名：小写单词，无下划线（`config`、`proxy`、`server`）。
- 文件名：小写 + 下划线分隔（`logging.go`、`config.go`）。
- 导出函数：大写开头，动词开头（`New`、`Load`）。
- 环境变量：大写 + 下划线（`UPSTREAM_URL`、`LISTEN_ADDR`、`LOG_LEVEL`）。
- Go module 名：`xunfei-gateway`（连字符，非下划线）。

---

## Examples

- 包职责单一：`internal/config/config.go` — 仅做环境变量解析与校验。
- 组装点唯一：`cmd/gateway/main.go` — 所有包在此连线，无分散初始化。
