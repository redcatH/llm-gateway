APP      := gateway
BIN      := bin/$(APP)
PKG      := ./cmd/gateway

# 默认上游用 httpbin.org 便于回显验证；生产部署请覆盖 UPSTREAM_URL。
UPSTREAM ?= https://httpbin.org

.PHONY: build run tidy vet docker clean help

build: ## 编译二进制到 bin/gateway
	go build -trimpath -ldflags="-s -w" -o $(BIN) $(PKG)

run: ## 以示例上游直接 go run
	UPSTREAM_URL=$(UPSTREAM) LISTEN_ADDR=:8080 LOG_LEVEL=info go run $(PKG)

tidy: ## 整理依赖
	go mod tidy

vet: ## 静态检查
	go vet ./...

docker: ## 构建 Docker 镜像
	docker build -t llm-gateway:latest .

clean: ## 清理构建产物
	rm -rf bin/

help: ## 显示帮助
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-10s\033[0m %s\n", $$1, $$2}'
