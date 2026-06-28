package sse

import (
	"encoding/json"
	"net/http"
	"strconv"
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
			// Anthropic overloaded_error —— 上游过载，客户端应重试。
			ErrorType: "overloaded_error",
			Handler:   retryableHandler(retryAfter),
		},
	}
}

// contextExceededHandler 返回一个 Handler：拦截并返回 400 + context_length_exceeded。
// 上下文超长是客户端错误，不可重试；返回标准 OpenAI 错误格式让客户端 SDK 正确处理。
func contextExceededHandler() Handler {
	return func(req *http.Request, m Match) Decision {
		body, _ := json.Marshal(map[string]any{
			"error": map[string]any{
				"message": "context length exceeded, reduce input tokens and retry",
				"type":    "invalid_request_error",
				"param":   nil,
				"code":    "context_length_exceeded",
			},
		})
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		return Decision{
			Intercept: true,
			Status:    http.StatusBadRequest, // 400
			Body:      body,
			Headers:   h,
		}
	}
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
