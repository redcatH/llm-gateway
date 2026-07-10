package sse

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
			// 10012 Bad Request 子类型未命中规则 → 拦截 422 + 纯 JSON payload（无 logDir 不 dump）。
			name:           "10012_bad_request_subtype_intercepted_to_422",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei ... EngineInternalError:Bad Request, timeStamp:00:00:00"}}` + "\n\n",
			wantStatus:     http.StatusUnprocessableEntity,
			wantBodyExact:  `{"error":{"code":10012,"message":"Xunfei ... EngineInternalError:Bad Request, timeStamp:00:00:00"}}`,
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
			// 真实线上日志：10110 ServiceIsBusyError:Engine Busy，应拦截为 503。
			name:           "10110_service_is_busy_intercepted_to_503",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10110,"message":"Xunfei request failed with Sid: cht000e7eef@dx19efa253ca2ba60322 code: 10110, msg: ServiceIsBusyError:Engine Busy, timeStamp:23:00:05.958"}}` + "\n\n",
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyHas:    "upstream engine busy",
			wantBodyNotHas: "ServiceIsBusyError",
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
			// 讯飞 Anthropic 路径：error 对象无结构化 code，真实 code 10012 藏在 message 里。
			// 期望从 message 回填 code 后命中 10012 规则 → 拦截 503。
			name:           "xunfei_anthropic_code_in_message_intercepted_to_503",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"Xunfei claude request failed with Sid: cht000dd27a@dx code: 10012, msg: EngineInternalError:1105|{\\\"Code\\\":1105,\\\"Message\\\":\\\"The system is busy, please try again later.\\\"}, timeStamp:17:50:17.382\",\"type\":\"api_error\"}}\n\n",
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyHas:    "upstream engine busy",
			wantBodyNotHas: "EngineInternalError",
			wantHeader:     "Retry-After",
		},
		{
			// 10012 + model_context_window_exceeded → 拦截为 400 + context_length_exceeded（OpenAI 路径）。
			name:           "10012_context_window_exceeded_intercepted_to_400",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei request failed with Sid: cht000d99f7@dx19f0dd14b94ba5b432 code: 10012, msg: EngineInternalError:error, status code: 400, status: 400 Bad Request, message: provider error: finish_reason=model_context_window_exceeded, timeStamp:18:40:51.623"}}` + "\n\n",
			wantStatus:     http.StatusBadRequest,
			wantBodyHas:    "context_length_exceeded",
			wantBodyNotHas: "EngineInternalError",
		},
		{
			// 10012 + unsupported content type → 拦截为 400 + invalid_request_error（OpenAI 路径）。
			name:           "10012_unsupported_content_type_intercepted_to_400",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei request failed with Sid: cht000ddd8e@dx19f0e187211b958312 code: 10012, msg: EngineInternalError:error, status code: 400, status: 400 Bad Request, message: invalid character 'd' looking for beginning of value, body: data:{\"error\":{\"code\":\"ModelArts.81001\",\"message\":\"Inference failed: request param validation error, Value error, message[90].content[0] has unsupported content type: 'image_url', only supported type(s): 'text'.\"}}, timeStamp:19:58:32.528"}}` + "\n\n",
			wantStatus:     http.StatusBadRequest,
			wantBodyHas:    "only text is allowed",
			wantBodyNotHas: "EngineInternalError",
		},
		{
			// 10012 + Invalid content type（英文文案变体）→ 拦截为 400。
			name:           "10012_invalid_content_type_intercepted_to_400",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei claude request failed with Sid: cht000d38d1@dx19f03008e55b8ab162 code: 10012, msg: EngineInternalError:error, status code: 400, status: 400 Bad Request, message: Invalid content type. image_url is only supported by certain models, timeStamp:16:16:37.896"}}` + "\n\n",
			wantStatus:     http.StatusBadRequest,
			wantBodyHas:    "only text is allowed",
			wantBodyNotHas: "EngineInternalError",
		},
		{
			// 10012 + 参数非法（中文文案变体）→ 拦截为 400。
			name:           "10012_param_invalid_chinese_intercepted_to_400",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei claude request failed with Sid: cht000d4817@dx19f030097cab894652 code: 10012, msg: EngineInternalError:error, status code: 400, status: 400 Bad Request, message: messages.content.type 参数非法，取值范围 ['text'], timeStamp:16:16:41.175"}}` + "\n\n",
			wantStatus:     http.StatusBadRequest,
			wantBodyHas:    "only text is allowed",
			wantBodyNotHas: "EngineInternalError",
		},
		{
			// 10012 + input token limit（上下文超长文案变体）→ 拦截为 400 + context_length_exceeded。
			name:           "10012_input_token_limit_intercepted_to_400",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei request failed with Sid: cht000d5627@dx19f06c95e3ab828262 code: 10012, msg: EngineInternalError:error, status code: 400, status: 400 Bad Request, message: input token limit is 202752, timeStamp:09:54:52.353"}}` + "\n\n",
			wantStatus:     http.StatusBadRequest,
			wantBodyHas:    "context_length_exceeded",
			wantBodyNotHas: "EngineInternalError",
		},
		{
			// 10012 + The system is busy（1105 英文描述版）→ 拦截为 503 + Retry-After。
			name:           "10012_the_system_is_busy_intercepted_to_503",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10012,"message":"Xunfei request failed with Sid: cht000dc15e@dx19efff4a635b894652 code: 10012, msg: EngineInternalError:The system is busy, please try again later., timeStamp:02:04:58.461"}}` + "\n\n",
			wantStatus:     http.StatusServiceUnavailable,
			wantBodyHas:    "upstream engine busy",
			wantBodyNotHas: "EngineInternalError",
			wantHeader:     "Retry-After",
		},
		{
			// Anthropic 格式但 error_type 不在规则中 → 未命中拦截为 422 + 纯 JSON payload。
			name:           "anthropic_unruled_intercepted_to_422",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"bad input\"}}\n\n",
			wantStatus:     http.StatusUnprocessableEntity,
			wantBodyExact:  `{"type":"error","error":{"type":"invalid_request_error","message":"bad input"}}`,
		},
		{
			// 10300 未配置规则 → 未命中拦截为 422 + 纯 JSON payload。
			name:           "10300_unruled_intercepted_to_422",
			upstreamStatus: http.StatusOK,
			upstreamCT:     "text/event-stream",
			upstreamBody:   `data: {"error":{"code":10300,"message":"Xunfei ... read message from mom expired"}}` + "\n\n",
			wantStatus:     http.StatusUnprocessableEntity,
			wantBodyExact:  `{"error":{"code":10300,"message":"Xunfei ... read message from mom expired"}}`,
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
			h := ProxyHandler(target, target, false, http.DefaultTransport, rules, slog.Default(), "", RewriteConfig{})

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
	h := ProxyHandler(openaiURL, anthropicURL, false, http.DefaultTransport, DefaultRules(5), slog.Default(), "", RewriteConfig{})

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

// TestNonSuccessResponseLogged 验证 5xx 响应的 body 被记录到日志，且透传字节级不变。
func TestNonSuccessResponseLogged(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	logger := slog.New(h)

	upstreamBody := `{"error":{"code":"10012","message":"xunfei response error: EngineInternalError:1105|The system is busy"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	ph := ProxyHandler(target, target, false, http.DefaultTransport, DefaultRules(5), logger, "", RewriteConfig{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	ph.ServeHTTP(rec, req)

	// 透传不变：500 + 原样 body。
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if rec.Body.String() != upstreamBody {
		t.Errorf("body not byte-exact\ngot:  %q\nwant: %q", rec.Body.String(), upstreamBody)
	}

	// 日志含 body 内容、status、path。
	logOut := buf.String()
	for _, want := range []string{
		"upstream non-success response",
		"status=500",
		"/v1/chat/completions",
		`EngineInternalError:1105`,
		`The system is busy`,
	} {
		if !strings.Contains(logOut, want) {
			t.Errorf("log missing %q\ngot: %s", want, logOut)
		}
	}
}

// Test4xxLoggedAsWarn 验证 4xx（客户端错误）记 Warn 级别日志（含 body），仍原样透传。
func Test4xxLoggedAsWarn(t *testing.T) {
	var buf bytes.Buffer
	// handler 级别放到 Warn，才能捕获 4xx 的 Warn 日志。
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})
	logger := slog.New(h)

	upstreamBody := `{"error":{"message":"invalid api key"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized) // 401
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	ph := ProxyHandler(target, target, false, http.DefaultTransport, DefaultRules(5), logger, "", RewriteConfig{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	ph.ServeHTTP(rec, req)

	// 透传不变。
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if rec.Body.String() != upstreamBody {
		t.Errorf("body not byte-exact\ngot:  %q\nwant: %q", rec.Body.String(), upstreamBody)
	}
	// 4xx 记 Warn（不是 Error）：日志含 body、status、path，且级别为 WARN。
	logOut := buf.String()
	for _, want := range []string{
		"level=WARN",
		"upstream non-success response",
		"status=401",
		`invalid api key`,
	} {
		if !strings.Contains(logOut, want) {
			t.Errorf("log missing %q\ngot: %s", want, logOut)
		}
	}
}

// TestNonSuccessResponseTruncated 验证超大 body 被截断并标注 truncated=true。
func TestNonSuccessResponseTruncated(t *testing.T) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})
	logger := slog.New(h)

	// 16KB body，超过 8KB 上限。
	big := strings.Repeat("x", 16*1024)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, big)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	ph := ProxyHandler(target, target, false, http.DefaultTransport, DefaultRules(5), logger, "", RewriteConfig{})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	ph.ServeHTTP(rec, req)

	// 透传仍是完整 body（截断只影响日志，不影响透传）。
	if rec.Body.String() != big {
		t.Errorf("passthrough body should be full, got len=%d want=%d", len(rec.Body.String()), len(big))
	}
	// 日志标注截断。
	logOut := buf.String()
	if !strings.Contains(logOut, "truncated=true") {
		t.Errorf("log should mark truncated=true; got: %s", logOut)
	}
	if !strings.Contains(logOut, "body_bytes=16384") {
		t.Errorf("log should record full body_bytes=16384; got: %s", logOut)
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

// TestDumpFileName 验证转储文件名：优先 X-Request-Id（filepath.Base 防穿越），否则毫秒时间戳。
func TestDumpFileName(t *testing.T) {
	now := time.Date(2026, 6, 27, 12, 20, 49, 989000000, time.UTC)

	cases := []struct {
		name   string
		header string // X-Request-Id 值；空表示不设置
		want   string
	}{
		{name: "uses_x_request_id", header: "abc-123", want: "abc-123"},
		{name: "trims_whitespace", header: "  abc-123  ", want: "abc-123"},
		{name: "path_traversal_sanitized", header: "../../etc/passwd", want: "passwd"},
		{name: "timestamp_when_absent", header: "", want: "20260627T122049.989Z"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if c.header != "" {
				req.Header.Set("X-Request-Id", c.header)
			}
			if got := dumpFileName(req, now); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestIs10012BadRequest 验证仅 10012 + EngineInternalError:Bad Request 命中。
func TestIs10012BadRequest(t *testing.T) {
	cases := []struct {
		name string
		m    Match
		want bool
	}{
		{"bad_request_subtype", Match{Code: 10012, Message: "code: 10012, msg: EngineInternalError:Bad Request, timeStamp:00:00:00"}, true},
		{"1105_subtype", Match{Code: 10012, Message: "EngineInternalError:1105"}, false},
		{"other_subtype", Match{Code: 10012, Message: "EngineInternalError:Other"}, false},
		{"missing_engine_error", Match{Code: 10012, Message: "Bad Request"}, false},
		{"wrong_code", Match{Code: 10010, Message: "EngineInternalError:Bad Request"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := is10012BadRequest(c.m); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestDumpRequestBody 验证转储与防覆盖（同名追加 -N）。
func TestDumpRequestBody(t *testing.T) {
	dir := t.TempDir()

	dumpRequestBody(dir, "req-1", []byte(`{"a":1}`), slog.Default())
	dumpRequestBody(dir, "req-1", []byte(`{"b":2}`), slog.Default()) // 同名 → req-1-2.json

	first, err := os.ReadFile(filepath.Join(dir, "requests", "req-1.json"))
	if err != nil {
		t.Fatalf("expected req-1.json: %v", err)
	}
	if string(first) != `{"a":1}` {
		t.Errorf("first = %q, want {\"a\":1}", string(first))
	}

	second, err := os.ReadFile(filepath.Join(dir, "requests", "req-1-2.json"))
	if err != nil {
		t.Fatalf("expected req-1-2.json (collision suffix): %v", err)
	}
	if string(second) != `{"b":2}` {
		t.Errorf("second = %q, want {\"b\":2}", string(second))
	}

	// 空 logDir / 空 body → 跳过，不报错。
	dumpRequestBody("", "req-1", []byte(`{"c":3}`), slog.Default())
	dumpRequestBody(dir, "req-empty", nil, slog.Default())
	if _, err := os.Stat(filepath.Join(dir, "requests", "req-empty.json")); !os.IsNotExist(err) {
		t.Errorf("empty body should not produce a file")
	}
}

// TestUnmatched10012DumpsRequestBody 端到端：10012 Bad Request 时转储完整请求体，响应仍透传。
func TestUnmatched10012DumpsRequestBody(t *testing.T) {
	upstreamBody := `data: {"error":{"code":10012,"message":"Xunfei request failed with Sid: x@dx code: 10012, msg: EngineInternalError:Bad Request, timeStamp:00:00:00"}}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	dir := t.TempDir()
	target, _ := url.Parse(upstream.URL)
	h := ProxyHandler(target, target, false, http.DefaultTransport, DefaultRules(5), slog.Default(), dir, RewriteConfig{})

	reqBody := `{"messages":[{"role":"tool","tool_call_id":"x","content":"r"}]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Request-Id", "test-req-1")
	h.ServeHTTP(rec, req)

	// 行为：未命中规则 → 拦截 422 + 纯 JSON payload（m.Raw，去掉 data: 前缀）。
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if rec.Body.String() != `{"error":{"code":10012,"message":"Xunfei request failed with Sid: x@dx code: 10012, msg: EngineInternalError:Bad Request, timeStamp:00:00:00"}}` {
		t.Errorf("body not the raw JSON payload\ngot:  %q\nwant: {\"error\":{...EngineInternalError:Bad Request...}}", rec.Body.String())
	}

	// dump 文件存在且内容 == 原始请求体（完整未截断）。
	dumped, err := os.ReadFile(filepath.Join(dir, "requests", "test-req-1.json"))
	if err != nil {
		t.Fatalf("expected dump file: %v", err)
	}
	if string(dumped) != reqBody {
		t.Errorf("dumped body = %q, want %q", string(dumped), reqBody)
	}
}

// TestNoDumpWhenNot10012BadRequest 验证非目标错误不产生 dump 文件。
func TestNoDumpWhenNot10012BadRequest(t *testing.T) {
	cases := []struct {
		name         string
		upstreamBody string
	}{
		{
			// 1105 子类型被规则拦截 → 走 matched，不进 dump 分支。
			name:         "10012_1105_intercepted",
			upstreamBody: `data: {"error":{"code":10012,"message":"Xunfei ... EngineInternalError:1105|{\"Code\":1105}"}}` + "\n\n",
		},
		{
			// 其他 10012 子类型，落 !matched 但不命中 is10012BadRequest → 不 dump。
			name:         "10012_other_subtype_unmatched",
			upstreamBody: `data: {"error":{"code":10012,"message":"Xunfei ... EngineInternalError:Other, timeStamp:00:00:00"}}` + "\n\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, c.upstreamBody)
			}))
			defer upstream.Close()

			dir := t.TempDir()
			target, _ := url.Parse(upstream.URL)
			h := ProxyHandler(target, target, false, http.DefaultTransport, DefaultRules(5), slog.Default(), dir, RewriteConfig{})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
			req.Header.Set("X-Request-Id", "should-not-dump")
			h.ServeHTTP(rec, req)

			entries, err := os.ReadDir(filepath.Join(dir, "requests"))
			if err == nil && len(entries) > 0 {
				t.Errorf("expected no dump files, got %d", len(entries))
			}
			// 目录不存在（从未写）也算通过。
		})
	}
}

// TestBadRequestFormatByProtocol 验证 400 错误按请求路径返回 OpenAI 或 Anthropic 格式。
func TestBadRequestFormatByProtocol(t *testing.T) {
	rules := DefaultRules(5)

	cases := []struct {
		name         string
		path         string
		upstreamBody string
		wantBodyHas  string // body 应包含的子串
		wantBodyNot  string // body 不应包含的子串
	}{
		{
			// context_window_exceeded + Anthropic 路径 → Anthropic 格式
			name:         "context_exceeded_anthropic_format",
			path:         "/v1/messages",
			upstreamBody: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Xunfei claude request failed with Sid: x@dx code: 10012, msg: EngineInternalError:error, message: provider error: finish_reason=model_context_window_exceeded, timeStamp:00:00:00\"}}\n\n",
			wantBodyHas:  "\"type\":\"error\"",
			wantBodyNot:  "\"param\"",
		},
		{
			// unsupported content type + Anthropic 路径 → Anthropic 格式
			name:         "unsupported_content_type_anthropic_format",
			path:         "/v1/messages",
			upstreamBody: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"Xunfei claude request failed with Sid: x@dx code: 10012, msg: EngineInternalError:error, message: unsupported content type: 'image_url', only supported type(s): 'text'.\"}}\n\n",
			wantBodyHas:  "\"type\":\"error\"",
			wantBodyNot:  "\"param\"",
		},
		{
			// context_window_exceeded + OpenAI 路径 → OpenAI 格式
			name:         "context_exceeded_openai_format",
			path:         "/v1/chat/completions",
			upstreamBody: `data: {"error":{"code":10012,"message":"Xunfei ... EngineInternalError:error, message: finish_reason=model_context_window_exceeded, timeStamp:00:00:00"}}` + "\n\n",
			wantBodyHas:  "\"param\":null",
			wantBodyNot:  "\"type\":\"error\"",
		},
		{
			// unsupported content type + OpenAI 路径 → OpenAI 格式
			name:         "unsupported_content_type_openai_format",
			path:         "/v1/chat/completions",
			upstreamBody: `data: {"error":{"code":10012,"message":"Xunfei ... EngineInternalError:error, message: unsupported content type: 'image_url', only supported type(s): 'text'."}}` + "\n\n",
			wantBodyHas:  "\"param\":null",
			wantBodyNot:  "\"type\":\"error\"",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, c.upstreamBody)
			}))
			defer upstream.Close()

			target, _ := url.Parse(upstream.URL)
			h := ProxyHandler(target, target, false, http.DefaultTransport, rules, slog.Default(), "", RewriteConfig{})

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(`{}`))
			req.Header.Set("Accept", "text/event-stream")
			h.ServeHTTP(rec, req)

			body := rec.Body.String()
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, body)
			}
			if !strings.Contains(body, c.wantBodyHas) {
				t.Errorf("body missing %q; got %q", c.wantBodyHas, body)
			}
			if strings.Contains(body, c.wantBodyNot) {
				t.Errorf("body should not contain %q; got %q", c.wantBodyNot, body)
			}
			// 所有 400 拦截都应含 invalid_request_error
			if !strings.Contains(body, "invalid_request_error") {
				t.Errorf("body missing 'invalid_request_error'; got %q", body)
			}
		})
	}
}

// TestFormatBadRequest 验证 formatBadRequest 对不同路径返回正确的 JSON 结构。
func TestFormatBadRequest(t *testing.T) {
	t.Run("openai_path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		body := formatBadRequest(req, "test message", "test_code")
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		errObj := parsed["error"].(map[string]any)
		if errObj["type"] != "invalid_request_error" {
			t.Errorf("type = %v, want invalid_request_error", errObj["type"])
		}
		if errObj["message"] != "test message" {
			t.Errorf("message = %v, want 'test message'", errObj["message"])
		}
		if errObj["code"] != "test_code" {
			t.Errorf("code = %v, want 'test_code'", errObj["code"])
		}
		if errObj["param"] != nil {
			t.Errorf("param = %v, want nil", errObj["param"])
		}
	})

	t.Run("anthropic_path", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		body := formatBadRequest(req, "test message", "ignored")
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if parsed["type"] != "error" {
			t.Errorf("outer type = %v, want 'error'", parsed["type"])
		}
		errObj := parsed["error"].(map[string]any)
		if errObj["type"] != "invalid_request_error" {
			t.Errorf("type = %v, want invalid_request_error", errObj["type"])
		}
		if errObj["message"] != "test message" {
			t.Errorf("message = %v, want 'test message'", errObj["message"])
		}
		// Anthropic 格式不应有 param/code 字段
		if _, ok := errObj["param"]; ok {
			t.Error("Anthropic format should not have 'param' field")
		}
		if _, ok := errObj["code"]; ok {
			t.Error("Anthropic format should not have 'code' field")
		}
	})

	t.Run("empty_code_serializes_null", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		body := formatBadRequest(req, "msg", "")
		var parsed map[string]any
		if err := json.Unmarshal(body, &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		errObj := parsed["error"].(map[string]any)
		if errObj["code"] != nil {
			t.Errorf("empty code should serialize as null, got %v", errObj["code"])
		}
	})
}

// TestModelRewrite 端到端验证响应 model 字段改写：四种响应组合 × 三模式。
func TestModelRewrite(t *testing.T) {
	rcDefault := RewriteConfig{
		Mode:    ModeDefault,
		Map:     map[string]string{"xopglm51": "glm-5.1", "xopglm52": "glm-5.2"},
		Default: "glm-default",
	}
	rcPassthrough := RewriteConfig{
		Mode: ModePassthrough,
		Map:  map[string]string{"xopglm51": "glm-5.1", "xopglm52": "glm-5.2"},
	}

	cases := []struct {
		name         string
		rc           RewriteConfig
		path         string
		upstreamCT   string
		upstreamBody string
		wantNotHas   string // 不应出现的子串（如真实模型名）
		wantHas      []string // 应出现的子串（对外名 / 透传内容）
	}{
		{
			// OpenAI 流式：每个 chunk 顶层 model，多帧均改写。
			name:         "openai_stream_default",
			rc:           rcDefault,
			path:         "/v1/chat/completions",
			upstreamCT:   "text/event-stream",
			upstreamBody: "data: {\"model\":\"xopglm52\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"model\":\"xopglm52\",\"choices\":[{\"delta\":{\"content\":\"there\"}}]}\n\ndata: [DONE]\n\n",
			wantNotHas:   "xopglm52",
			wantHas:      []string{"glm-5.2", "[DONE]"},
		},
		{
			// OpenAI 流式 reasoning_content（思考模式）：顶层 model 改写，reasoning 内容保留。
			name:         "openai_stream_reasoning",
			rc:           rcDefault,
			path:         "/v1/chat/completions",
			upstreamCT:   "text/event-stream",
			upstreamBody: "data: {\"model\":\"xopglm52\",\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"}}]}\n\n",
			wantNotHas:   "xopglm52",
			wantHas:      []string{"glm-5.2", `"reasoning_content":"thinking"`},
		},
		{
			// OpenAI 非流式：顶层 model。
			name:         "openai_nonstream_default",
			rc:           rcDefault,
			path:         "/v1/chat/completions",
			upstreamCT:   "application/json",
			upstreamBody: `{"id":"1","model":"xopglm51","choices":[{"message":{"content":"hi"}}]}`,
			wantNotHas:   "xopglm51",
			wantHas:      []string{"glm-5.1", `"content":"hi"`},
		},
		{
			// Anthropic 流式：仅 message_start 的 message.model 改写；content_block_delta 无 model 原样透传。
			name:         "anthropic_stream_default",
			rc:           rcDefault,
			path:         "/v1/messages",
			upstreamCT:   "text/event-stream",
			upstreamBody: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"xopglm51\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n",
			wantNotHas:   "xopglm51",
			wantHas:      []string{"glm-5.1", `"text":"hi"`, "content_block_delta"},
		},
		{
			// Anthropic 非流式：顶层 model，未命中 → default。
			name:         "anthropic_nonstream_default_miss",
			rc:           rcDefault,
			path:         "/v1/messages",
			upstreamCT:   "application/json",
			upstreamBody: `{"id":"1","model":"xopglmv47flash","content":[{"type":"text","text":"hi"}]}`,
			wantNotHas:   "xopglmv47flash",
			wantHas:      []string{"glm-default"},
		},
		{
			// passthrough 未命中：透传真名。
			name:         "openai_stream_passthrough_miss",
			rc:           rcPassthrough,
			path:         "/v1/chat/completions",
			upstreamCT:   "text/event-stream",
			upstreamBody: "data: {\"model\":\"xopglmv47flash\",\"choices\":[]}\n\n",
			wantHas:      []string{"xopglmv47flash"},
		},
		{
			// off 模式：不改写，保留真名。
			name:         "openai_nonstream_off",
			rc:           RewriteConfig{Mode: ModeOff},
			path:         "/v1/chat/completions",
			upstreamCT:   "application/json",
			upstreamBody: `{"model":"xopglm51"}`,
			wantHas:      []string{"xopglm51"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", c.upstreamCT)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, c.upstreamBody)
			}))
			defer upstream.Close()

			target, _ := url.Parse(upstream.URL)
			h := ProxyHandler(target, target, false, http.DefaultTransport, DefaultRules(5), slog.Default(), "", c.rc)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, c.path, strings.NewReader(`{}`))
			h.ServeHTTP(rec, req)

			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, body)
			}
			if c.wantNotHas != "" && strings.Contains(body, c.wantNotHas) {
				t.Errorf("body should not contain %q; got %q", c.wantNotHas, body)
			}
			for _, has := range c.wantHas {
				if !strings.Contains(body, has) {
					t.Errorf("body missing %q; got %q", has, body)
				}
			}
		})
	}
}

// TestModelRewriteDoesNotBreakErrorIntercept 验证启用改写时 SSE error 拦截仍正常工作。
// error 帧无 model 字段，改写与拦截字段正交、互不干扰。
func TestModelRewriteDoesNotBreakErrorIntercept(t *testing.T) {
	rc := RewriteConfig{Mode: ModeDefault, Map: map[string]string{"xopglm51": "glm-5.1"}, Default: "glm-default"}
	upstreamBody := `data: {"error":{"code":10012,"message":"Xunfei ... EngineInternalError:1105|{\"Code\":1105}"}}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	h := ProxyHandler(target, target, false, http.DefaultTransport, DefaultRules(5), slog.Default(), "", rc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (error intercept should still fire); body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "upstream engine busy") {
		t.Errorf("expected intercept body; got %q", rec.Body.String())
	}
}

// TestForwardStreamIncompleteEvent 验证流末尾无空行边界、直接 EOF 时 forwardStream
// 不死循环，且已读字节（含改写）正常写出。锁死 readEvent "EOF 优先于空行边界判断" 的修复
// （proxy.go:201 err 检查前移）——若顺序颠倒，EOF 被空行分支吞掉返回 (空,nil)，循环死等，
// 本测试会触发 -timeout 超时。
func TestForwardStreamIncompleteEvent(t *testing.T) {
	rc := RewriteConfig{Mode: ModeDefault, Map: map[string]string{"xopglm51": "glm-5.1"}, Default: "glm-default"}
	// 上游 body 只有一行 data，无结尾空行（\n\n）即结束 → readEvent 读一行后遇 EOF。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"model\":\"xopglm51\"}\n")
	}))
	defer upstream.Close()

	target, _ := url.Parse(upstream.URL)
	h := ProxyHandler(target, target, false, http.DefaultTransport, DefaultRules(5), slog.Default(), "", rc)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "glm-5.1") {
		t.Errorf("expected rewritten model glm-5.1 in body; got %q", body)
	}
	if strings.Contains(body, "xopglm51") {
		t.Errorf("real name leaked; got %q", body)
	}
}
