package sse

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestProxyHandler 用 httptest.Server 模拟上游，表驱动覆盖拦截与透传各场景。
func TestProxyHandler(t *testing.T) {
	cases := []struct {
		name           string
		upstreamStatus int
		upstreamCT     string // 上游 Content-Type
		upstreamBody   string // 上游响应体
		wantStatus     int
		wantBodyExact  string // 透传场景：断言下游 body 与上游字节级一致
		wantBodyHas    string // 拦截场景：下游 body 应包含的子串
		wantBodyNotHas string // 拦截场景：下游 body 不应包含的子串
		wantHeader     string // 下游应包含的响应头
	}{
		{
			name:           "10012_with_1105_intercepted_to_503",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei request failed with Sid: x@dx code: 10012, msg: EngineInternalError:1105|{\"Code\":1105,\"Message\":\"The system is busy, please try again later.\"}, timeStamp:00:00:00"}}` + "\n\n",
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyHas:    "upstream engine busy",
			wantBodyNotHas: "EngineInternalError",
			wantHeader:     "Retry-After",
		},
		{
			name:           "10012_bad_request_subtype_passthrough",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei ... EngineInternalError:Bad Request, timeStamp:00:00:00"}}` + "\n\n",
			wantStatus:     http.StatusOK,
			wantBodyExact:  `data: {"error":{"code":10012,"message":"Xunfei ... EngineInternalError:Bad Request, timeStamp:00:00:00"}}` + "\n\n",
		},
		{
			name:           "10010_engine_busy_intercepted_to_503",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10010,"message":"Xunfei ... RecvFromEngineError:Engine Busy"}}` + "\n\n",
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyHas:    "upstream engine busy",
			wantBodyNotHas: "RecvFromEngineError",
			wantHeader:     "Retry-After",
		},
		{
			name:           "11210_not_enough_cv_intercepted_to_503",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":11210,"message":"Xunfei ... NotEnoughCvError"}}` + "\n\n",
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyHas:    "upstream engine busy",
			wantBodyNotHas: "NotEnoughCvError",
			wantHeader:     "Retry-After",
		},
		{
			// Anthropic 格式：event: error + {"type":"error","error":{"type":"overloaded_error",...}}
			name:           "anthropic_overloaded_intercepted_to_503",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Anthropic upstream is overloaded\"}}\n\n",
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyHas:    "upstream engine busy",
			wantBodyNotHas: "overloaded_error",
			wantHeader:     "Retry-After",
		},
		{
			// Anthropic 格式但 error_type 不在规则中 → 透传。
			name:           "anthropic_unruled_passthrough",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"bad input\"}}\n\n",
			wantStatus:     http.StatusOK,
			wantBodyExact:  "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"bad input\"}}\n\n",
		},
		{
			// 10300 未配置规则 → 仍透传，证明只拦已知 code。
			name:           "10300_unruled_passthrough",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10300,"message":"Xunfei ... read message from mom expired"}}` + "\n\n",
			wantStatus:     http.StatusOK,
			wantBodyExact:  `data: {"error":{"code":10300,"message":"Xunfei ... read message from mom expired"}}` + "\n\n",
		},
		{
			name:           "normal_stream_passthrough_byte_exact",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
			wantStatus:     http.StatusOK,
			wantBodyExact:  "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
		},
		{
			name:           "non_200_passthrough",
			upstreamStatus: http.StatusInternalServerError,
			upstreamCT:     "application/json",
			upstreamBody:   `{"error":"internal"}`,
			wantStatus:     http.StatusInternalServerError,
			wantBodyExact:  `{"error":"internal"}`,
		},
		{
			name:           "non_sse_200_passthrough",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "application/json",
			upstreamBody:   `{"id":"chatcmpl-1","choices":[]}`,
			wantStatus:     http.StatusOK,
			wantBodyExact:  `{"id":"chatcmpl-1","choices":[]}`,
		},
	}

	// DefaultRules 已含 overloaded_error 规则。
	rules := DefaultRules(5)

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.upstreamCT)
				w.WriteHeader(c.upstreamStatus)
				_, _ = io.WriteString(w, c.upstreamBody)
			}))
			defer upstream.Close()

			target, _ := url.Parse(upstream.URL)
			h := ProxyHandler(target, target, false, http.DefaultTransport, rules, slog.Default())

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
			req.Header.Set("Accept", "text/event-stream")
			h.ServeHTTP(rec, req)

			body := rec.Body.String()
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, c.wantStatus, body)
			}
			if c.wantBodyExact != "" && body != c.wantBodyExact {
				t.Errorf("body not byte-exact\ngot:  %q\nwant: %q", body, c.wantBodyExact)
			}
			if c.wantBodyHas != "" && !strings.Contains(body, c.wantBodyHas) {
				t.Errorf("body missing %q; got %q", c.wantBodyHas, body)
			}
			if c.wantBodyNotHas != "" && strings.Contains(body, c.wantBodyNotHas) {
				t.Errorf("body should not contain %q; got %q", c.wantBodyNotHas, body)
			}
			if c.wantHeader != "" && rec.Header().Get(c.wantHeader) == "" {
				t.Errorf("missing response header %q", c.wantHeader)
			}
		})
	}
}

// TestProxyHandlerRouting 验证按请求路径路由到不同上游：
// /v1/messages（含 /antigravity/v1/messages）→ anthropic 上游，其余 → openai 上游。
func TestProxyHandlerRouting(t *testing.T) {
	openai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"routed\":\"openai\"}\n\n")
	}))
	defer openai.Close()
	anthropic := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"routed\":\"anthropic\"}\n\n")
	}))
	defer anthropic.Close()

	openaiURL, _ := url.Parse(openai.URL)
	anthropicURL, _ := url.Parse(anthropic.URL)
	h := ProxyHandler(openaiURL, anthropicURL, false, http.DefaultTransport, DefaultRules(5), slog.Default())

	cases := []struct {
		path    string
		wantHas string
	}{
		{"/v1/chat/completions", `"openai"`},
		{"/v1/messages", `"anthropic"`},
		{"/antigravity/v1/messages", `"anthropic"`},
		{"/v1/responses", `"openai"`},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(`{}`))
			h.ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), c.wantHas) {
				t.Errorf("path %s: body=%q, want contains %q", c.path, rec.Body.String(), c.wantHas)
			}
		})
	}
}

// TestRuleMatches 验证规则的 code + ErrorType + 多子串 AND 匹配。
func TestRuleMatches(t *testing.T) {
	// 多子串 AND：必须同时含 "EngineInternalError" 与 "1105"。
	rule := Rule{Code: 10012, MsgContains: []string{"EngineInternalError", "1105"}}

	cases := []struct {
		name string
		m    Match
		want bool
	}{
		{"both_substrings_hit", Match{Code: 10012, Message: "msg EngineInternalError:1105 done"}, true},
		{"only_first_substring", Match{Code: 10012, Message: "EngineInternalError:Bad Request"}, false},
		{"only_second_substring", Match{Code: 10012, Message: "code 1105 something"}, false},
		{"wrong_code", Match{Code: 10010, Message: "EngineInternalError:1105"}, false},
		{"neither_substring", Match{Code: 10012, Message: "something else"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rule.matches(c.m); got != c.want {
				t.Errorf("matches = %v, want %v", got, c.want)
			}
		})
	}

	// 通配规则（Code=0 且 MsgContains 为空）匹配任意 error。
	wildcard := Rule{}
	if !wildcard.matches(Match{Code: 1, Message: "anything"}) {
		t.Error("wildcard rule should match any error")
	}

	// ErrorType 匹配。
	etRule := Rule{ErrorType: "overloaded_error"}
	if !etRule.matches(Match{ErrorType: "overloaded_error", Message: "busy"}) {
		t.Error("ErrorType match should hit")
	}
	if etRule.matches(Match{ErrorType: "invalid_request_error", Message: "bad"}) {
		t.Error("ErrorType mismatch should not hit")
	}
}

// TestParseErrorEvent 验证从 SSE 事件字节解析 error 帧（OpenAI + Anthropic 格式）。
func TestParseErrorEvent(t *testing.T) {
	cases := []struct {
		name        string
		event       string
		wantOK      bool
		wantCode    int
		wantErrType string
		wantMsgHas  string
	}{
		// OpenAI 格式
		{"openai_numeric_code", "data: {\"error\":{\"code\":10012,\"message\":\"busy\"}}\n\n", true, 10012, "", "busy"},
		{"openai_with_event_line", "event: error\ndata: {\"error\":{\"code\":10012,\"message\":\"x\"}}\n\n", true, 10012, "", "x"},
		// Anthropic 格式
		{"anthropic_overloaded", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"overloaded\"}}\n\n", true, 0, "overloaded_error", "overloaded"},
		{"anthropic_rate_limit", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"too many\"}}\n\n", true, 0, "rate_limit_error", "too many"},
		{"anthropic_with_numeric_code", "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"code\":10012,\"message\":\"xunfei busy\"}}\n\n", true, 10012, "api_error", "xunfei busy"},
		// 非 error 帧
		{"choices_not_error", "data: {\"choices\":[{\"delta\":{}}]}\n\n", false, 0, "", ""},
		{"done_marker", "data: [DONE]\n\n", false, 0, "", ""},
		{"comment_only", ": keepalive\n\n", false, 0, "", ""},
		{"anthropic_message_start", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{}}\n\n", false, 0, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, ok := parseErrorEvent([]byte(c.event))
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if m.Code != c.wantCode {
				t.Errorf("code = %d, want %d", m.Code, c.wantCode)
			}
			if m.ErrorType != c.wantErrType {
				t.Errorf("errorType = %q, want %q", m.ErrorType, c.wantErrType)
			}
			if c.wantMsgHas != "" && !strings.Contains(m.Message, c.wantMsgHas) {
				t.Errorf("message = %q, want contains %q", m.Message, c.wantMsgHas)
			}
		})
	}
}
