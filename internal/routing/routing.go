// Package routing 按请求协议（路径）在上游之间选择目标。
//
// 判断依据：请求路径含 "/v1/messages" 视为 Anthropic 协议
// （覆盖 /v1/messages 与 /antigravity/v1/messages）；其余路径
// （/v1/chat/completions、/v1/responses、未知路径等）走 OpenAI 默认上游。
package routing

import (
	"net/http"
	"net/url"
	"strings"
)

// IsAnthropicPath 判断路径是否为 Anthropic 协议（含 /v1/messages）。
func IsAnthropicPath(path string) bool {
	return strings.Contains(path, "/v1/messages")
}

// SelectTarget 按请求路径在上游 openAI 与 anthropic 之间选择。
// 路径含 "/v1/messages" → anthropic；否则 → openai。
func SelectTarget(req *http.Request, openAI, anthropic *url.URL) *url.URL {
	if IsAnthropicPath(req.URL.Path) {
		return anthropic
	}
	return openAI
}

// RewritePath 根据上游 target 的路径决定最终出站路径。
//
// 规则：
//   - target.Path 以 "/" 结尾（如 https://host/v2/）→ 剥离客户端路径的 "/v1" 前缀，
//     拼接到 target.Path 后。例：target=/v2/ + req=/v1/messages → /v2/messages
//   - target.Path 不以 "/" 结尾（如 https://host/anthropic）→ 上游路径 + 客户端完整路径。
//     例：target=/anthropic + req=/v1/messages → /anthropic/v1/messages
//   - target.Path 为空（如 https://host）→ 客户端路径原样透传。
//     例：target=host + req=/v1/messages → /v1/messages
func RewritePath(target *url.URL, reqPath string) string {
	tp := target.Path
	// 上游无路径 → 客户端路径原样透传
	if tp == "" {
		return reqPath
	}
	// 上游路径以 / 结尾 → 视为路径前缀，剥离客户端 /v1 后拼接
	if strings.HasSuffix(tp, "/") {
		stripped := reqPath
		if strings.HasPrefix(stripped, "/v1") {
			stripped = stripped[len("/v1"):]
		}
		if stripped == "" {
			stripped = "/"
		}
		if !strings.HasPrefix(stripped, "/") {
			stripped = "/" + stripped
		}
		return strings.TrimRight(tp, "/") + stripped
	}
	// 上游路径不以 / 结尾 → 上游路径 + 客户端完整路径
	return tp + reqPath
}
