package sse

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Decision 是 Handler 对命中 error 的处理决策。
type Decision struct {
	// Intercept 为 true 表示拦截（不透传原流），按下方字段返回响应；
	// 为 false 表示放行原样透传。
	Intercept bool
	// Status 是拦截时的 HTTP 状态码。
	Status int
	// Body 是拦截时的响应体。
	Body []byte
	// Headers 是拦截时的响应头（如 Retry-After、Content-Type）。
	Headers http.Header
}

// Handler 处理一个命中的 SSE error，返回决策。
// 实现可以是"返回 503 客户端重试""网关自重试""仅记录"等，逐步完善。
type Handler func(req *http.Request, m Match) Decision

// DefaultRules 返回初始规则集。命中任一规则即拦截并返回 503 + Retry-After，
// 让客户端 SDK 自动重试。后续新增规则只需在此 append。
func DefaultRules(retryAfter int) []Rule {
	return []Rule{
		{
			// 10012 EngineInternalError:1105（The system is busy）。
			// 多子串 AND：同时含 "EngineInternalError" 与 "1105"，避免误匹配 Bad Request 子类型。
			Code:        10012,
			MsgContains: []string{"EngineInternalError", "1105"},
			Handler:     retryableHandler(retryAfter),
		},
		{
			// 10010 RecvFromEngineError:Engine Busy —— 引擎忙，临时性可重试。
			// 多子串 AND：同时含 "RecvFromEngineError" 与 "Engine Busy"，避免误匹配 10110。
			Code:        10010,
			MsgContains: []string{"RecvFromEngineError", "Engine Busy"},
			Handler:     retryableHandler(retryAfter),
		},
		{
			// 10110 ServiceIsBusyError:Engine Busy —— 服务忙，临时性可重试。
			// 与 10010 同属"引擎忙"，但错误码/错误名不同，单独成条。
			Code:        10110,
			MsgContains: []string{"ServiceIsBusyError", "Engine Busy"},
			Handler:     retryableHandler(retryAfter),
		},
		{
			// 11210 NotEnoughCvError —— 并发/容量不足，退避后重试。
			Code:        11210,
			MsgContains: []string{"NotEnoughCvError"},
			Handler:     retryableHandler(retryAfter),
		},
		{
			// 10012 EngineInternalError + model_context_window_exceeded —— 上下文超长，
			// 客户端错误，不可重试。返回 400 + context_length_exceeded 让客户端 SDK 正确处理。
			Code:        10012,
			MsgContains: []string{"EngineInternalError", "model_context_window_exceeded"},
			Handler:     contextExceededHandler(),
		},
		{
			// 10012 EngineInternalError + unsupported content type —— 模型不支持该内容类型
			// （如纯文本模型收到 image_url），客户端错误，不可重试。
			// 返回 400 + invalid_request_error，按请求路径区分 OpenAI/Anthropic 格式。
			Code:        10012,
			MsgContains: []string{"EngineInternalError", "unsupported content type"},
			Handler:     unsupportedContentTypeHandler(),
		},
		{
			// Anthropic overloaded_error —— 上游过载，客户端应重试。
			ErrorType: "overloaded_error",
			Handler:   retryableHandler(retryAfter),
		},
	}
}

// contextExceededHandler 返回一个 Handler：拦截并返回 400 + context_length_exceeded。
// 上下文超长是客户端错误，不可重试；按请求路径返回 OpenAI 或 Anthropic 标准错误格式。
func contextExceededHandler() Handler {
	return func(req *http.Request, m Match) Decision {
		const msg = "context length exceeded, reduce input tokens and retry"
		return Decision{
			Intercept: true,
			Status:    http.StatusBadRequest,
			Body:      formatBadRequest(req, msg, "context_length_exceeded"),
			Headers:   jsonHeader(),
		}
	}
}

// unsupportedContentTypeHandler 返回一个 Handler：拦截并返回 400 + invalid_request_error。
// 模型不支持该内容类型（如纯文本模型收到 image_url），客户端错误，不可重试。
// 按请求路径返回 OpenAI 或 Anthropic 标准错误格式。
func unsupportedContentTypeHandler() Handler {
	return func(req *http.Request, m Match) Decision {
		const msg = "model does not support this content type, only text is allowed"
		return Decision{
			Intercept: true,
			Status:    http.StatusBadRequest,
			Body:      formatBadRequest(req, msg, ""),
			Headers:   jsonHeader(),
		}
	}
}

// formatBadRequest 按请求路径返回 OpenAI 或 Anthropic 格式的 400 错误体。
// 路径含 /v1/messages → Anthropic 格式；其余 → OpenAI 格式。
// code 为空时 OpenAI 格式不设 code 字段（null）。
func formatBadRequest(req *http.Request, message, code string) []byte {
	if strings.Contains(req.URL.Path, "/v1/messages") {
		// Anthropic 格式
		body, _ := json.Marshal(map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "invalid_request_error",
				"message": message,
			},
		})
		return body
	}
	// OpenAI 格式
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    stringOrNil(code),
		},
	})
	return body
}

// stringOrNil 返回非空字符串，空字符串返回 nil（JSON 序列化为 null）。
func stringOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// jsonHeader 返回 Content-Type: application/json 的 Header。
func jsonHeader() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	return h
}

// retryableHandler 返回一个 Handler：拦截并返回 503 + Retry-After + JSON error。
// 文案通用，不泄漏上游供应商标识；客户端 SDK（OpenAI/Anthropic）默认会重试 5xx。
func retryableHandler(retryAfter int) Handler {
	return func(req *http.Request, m Match) Decision {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"code":    "upstream_overloaded",
				"message": "upstream engine busy, please retry",
				"type":    "server_error",
			},
		})
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set("Retry-After", strconv.Itoa(retryAfter))
		return Decision{
			Intercept: true,
			Status:    http.StatusServiceUnavailable, // 503
			Body:      body,
			Headers:   h,
		}
	}
}
