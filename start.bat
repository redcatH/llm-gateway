@echo off
chcp 65001 >nul 2>&1

:: ── 必填：上游地址 ──
if "%UPSTREAM_URL%"=="" (
    set UPSTREAM_URL=https://your-upstream.example.com
)

:: ── 可选配置（默认值已在代码中，此处仅覆盖需要调整的项）──
if "%LISTEN_ADDR%"==""   set LISTEN_ADDR=:8080
if "%LOG_LEVEL%"==""     set LOG_LEVEL=debug
if "%SSE_INTERCEPT_ENABLED%"=="" set SSE_INTERCEPT_ENABLED=true
if "%SSE_RETRY_AFTER%"==""       set SSE_RETRY_AFTER=5

echo [xunfei-gateway] UPSTREAM_URL=%UPSTREAM_URL%
echo [xunfei-gateway] LISTEN_ADDR=%LISTEN_ADDR%
echo [xunfei-gateway] SSE_INTERCEPT=%SSE_INTERCEPT_ENABLED%
echo [xunfei-gateway] LOG_LEVEL=%LOG_LEVEL%
echo.

go run ./cmd/gateway
