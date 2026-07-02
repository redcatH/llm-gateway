@echo off
chcp 65001 >nul 2>&1

:: -- Upstream URLs (both required: OpenAI + Anthropic)
:: 本地运行前请填入真实上游地址（勿提交真实地址到仓库）。
if "%UPSTREAM_OPENAI_URL%"==""   set UPSTREAM_OPENAI_URL=https://your-openai-upstream.example.com
if "%UPSTREAM_ANTHROPIC_URL%"=="" set UPSTREAM_ANTHROPIC_URL=https://your-anthropic-upstream.example.com

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
if "%MODEL_REWRITE_MODE%"==""    set MODEL_REWRITE_MODE=off
if "%MODEL_MAP%"==""             set MODEL_MAP=
if "%MODEL_DEFAULT%"==""         set MODEL_DEFAULT=

echo [llm-gateway] UPSTREAM_OPENAI=%UPSTREAM_OPENAI_URL%
echo [llm-gateway] UPSTREAM_ANTHROPIC=%UPSTREAM_ANTHROPIC_URL%
echo [llm-gateway] LISTEN_ADDR=%LISTEN_ADDR%
echo [llm-gateway] SSE_INTERCEPT=%SSE_INTERCEPT_ENABLED%
echo [llm-gateway] MODEL_REWRITE_MODE=%MODEL_REWRITE_MODE%
echo [llm-gateway] LOG_LEVEL=%LOG_LEVEL%
echo [llm-gateway] LOG_DIR=%LOG_DIR% (max_size=%LOG_MAX_SIZE%MB, max_backups=%LOG_MAX_BACKUPS%, compress=%LOG_COMPRESS%)
echo.

go run ./cmd/gateway
