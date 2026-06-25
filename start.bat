@echo off
chcp 65001 >nul 2>&1

:: -- Upstream URLs (fallback: UPSTREAM_OPENAI/ANTHROPIC_URL default to UPSTREAM_URL)
if "%UPSTREAM_URL%"=="" (
    set UPSTREAM_URL=https://your-upstream.example.com
)
if "%UPSTREAM_OPENAI_URL%"==""   set UPSTREAM_OPENAI_URL=%UPSTREAM_URL%
if "%UPSTREAM_ANTHROPIC_URL%"=="" set UPSTREAM_ANTHROPIC_URL=%UPSTREAM_URL%

:: -- Optional overrides (defaults are in code)
if "%LISTEN_ADDR%"==""   set LISTEN_ADDR=:8080
if "%LOG_LEVEL%"==""     set LOG_LEVEL=debug
if "%LOG_DIR%"==""       set LOG_DIR=logs
if "%SSE_INTERCEPT_ENABLED%"=="" set SSE_INTERCEPT_ENABLED=true
if "%SSE_RETRY_AFTER%"==""       set SSE_RETRY_AFTER=5

echo [xunfei-gateway] UPSTREAM_OPENAI=%UPSTREAM_OPENAI_URL%
echo [xunfei-gateway] UPSTREAM_ANTHROPIC=%UPSTREAM_ANTHROPIC_URL%
echo [xunfei-gateway] LISTEN_ADDR=%LISTEN_ADDR%
echo [xunfei-gateway] SSE_INTERCEPT=%SSE_INTERCEPT_ENABLED%
echo [xunfei-gateway] LOG_LEVEL=%LOG_LEVEL%
echo [xunfei-gateway] LOG_DIR=%LOG_DIR%
echo.

go run ./cmd/gateway
