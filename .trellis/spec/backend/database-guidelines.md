# Database Guidelines

> 本项目无数据库。纯代理网关，不持久化任何数据。

---

## Overview

xunfei-gateway 是无状态透明代理，不使用任何数据库、缓存或持久化存储。
所有配置来自环境变量，所有请求实时转发，无本地状态。

---

## Applicability

如果未来需要添加数据库（如限流计数、审计日志），应：

1. 在 `internal/` 下新建包（如 `internal/store/`）。
2. 仍坚持零依赖原则——优先用标准库 `database/sql`。
3. 连接字符串通过环境变量注入，不硬编码。
4. 更新本规范文件。
