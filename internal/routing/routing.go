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

// SelectTarget 按请求路径在上游 openAI 与 anthropic 之间选择。
// 路径含 "/v1/messages" → anthropic；否则 → openai。
func SelectTarget(req *http.Request, openAI, anthropic *url.URL) *url.URL {
	if strings.Contains(req.URL.Path, "/v1/messages") {
		return anthropic
	}
	return openAI
}
