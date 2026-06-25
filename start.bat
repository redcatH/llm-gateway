@echo off
chcp 65001 >nul 2>&1

:: -- Upstream URLs (fallback: UPSTREAM_OPENAI/ANTHROPIC_URL default to UPSTREAM_URL)
:: 本地运行前请填入真实上游地址（勿提交真实地址到仓库）。
if "%UPSTREAM_URL%"=="" (
    set UPSTREAM_URL=https://your-upstream.example.com
)
if "%UPSTREAM_OPENAI_URL%"==""   set UPSTREAM_OPENAI_URL=%UPSTREAM_URL%
if "%UPSTREAM_ANTHROPIC_URL%"=="" set UPSTREAM_ANTHROPIC_URL=%UPSTREAM_URL%

:: -- Optional overrides (defaults are in code)
if "%LISTEN_ADDR%"==""   set LISTEN_ADDR=:8080
if "%LOG_LEVEL%"==""     set LOG_LEVEL=debug
if "%LOG_DIR%"==""       set LOG_DIR=logs
if "%LOG_MAX_SIZE%"==""       set LOG_MAX_SIZE=100
if "%LOG_MAX_BACKUPS%"==""    set LOG_MAX_BACKUPS=7
if "%LOG_MAX_AGE%"==""        set LOG_MAX_AGE=0
if "%LOG_COMPRESS%"==""       set LOG_COMPRESS=true
if "%SSE_INTERCEPT_ENABLED%"=="" set SSE_INTERCEPT_ENABLED=true
if "%SSE_RETRY_AFTER%"==""       set SSE_RETRY_AFTER=5

echo [llm-gateway] UPSTREAM_OPENAI=%UPSTREAM_OPENAI_URL%
echo [llm-gateway] UPSTREAM_ANTHROPIC=%UPSTREAM_ANTHROPIC_URL%
echo [llm-gateway] LISTEN_ADDR=%LISTEN_ADDR%
echo [llm-gateway] SSE_INTERCEPT=%SSE_INTERCEPT_ENABLED%
echo [llm-gateway] LOG_LEVEL=%LOG_LEVEL%
echo [llm-gateway] LOG_DIR=%LOG_DIR% (max_size=%LOG_MAX_SIZE%MB, max_backups=%LOG_MAX_BACKUPS%, compress=%LOG_COMPRESS%)
echo.

go run ./cmd/gateway
