package sse

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestMapModel(t *testing.T) {
	rc := RewriteConfig{
		Mode:    ModeDefault,
		Map:     map[string]string{"xopglm51": "glm-5.1"},
		Default: "glm-default",
	}
	cases := []struct {
		name string
		real string
		want string
	}{
		{"hit", "xopglm51", "glm-5.1"},
		{"miss_default", "xopglmv47flash", "glm-default"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := mapModel(c.real, rc); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	// passthrough 模式未命中返回原值。
	rcPass := RewriteConfig{Mode: ModePassthrough, Map: map[string]string{"xopglm51": "glm-5.1"}}
	if got := mapModel("xopglmv47flash", rcPass); got != "xopglmv47flash" {
		t.Errorf("passthrough miss: got %q, want xopglmv47flash", got)
	}
}

func TestRewriteModelJSON(t *testing.T) {
	rc := RewriteConfig{
		Mode:    ModeDefault,
		Map:     map[string]string{"xopglm51": "glm-5.1", "xopglm52": "glm-5.2"},
		Default: "glm-default",
	}

	cases := []struct {
		name      string
		input     string
		wantModel string // 改写后顶层或 message.model 的期望值；空表示断言原样返回
	}{
		{
			name:      "openai_top_level",
			input:     `{"id":"1","model":"xopglm52","choices":[]}`,
			wantModel: "glm-5.2",
		},
		{
			name:      "anthropic_message_model",
			input:     `{"type":"message_start","message":{"id":"1","model":"xopglm51"}}`,
			wantModel: "glm-5.1",
		},
		{
			name:      "miss_default_uses_default",
			input:     `{"model":"xopglmv47flash"}`,
			wantModel: "glm-default",
		},
		{
			name:  "no_model_field_passthrough",
			input: `{"choices":[{"delta":{"content":"hi"}}]}`,
			// 无 model 字段 → 原样返回
		},
		{
			name:  "invalid_json_passthrough",
			input: `{not json`,
			// 解析失败 → 原样返回
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := rewriteModelJSON([]byte(c.input), rc)
			if c.wantModel == "" {
				// 断言原样返回
				if !bytes.Equal(out, []byte(c.input)) {
					t.Errorf("expected byte-exact passthrough\ngot:  %q\nwant: %q", out, c.input)
				}
				return
			}
			// 解析输出，提取 model（顶层或 message.model）。
			var parsed map[string]any
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("output not valid JSON: %v; out=%q", err, out)
			}
			gotModel, ok := parsed["model"].(string)
			if !ok {
				// 尝试 message.model（Anthropic）。
				if msg, ok := parsed["message"].(map[string]any); ok {
					gotModel, _ = msg["model"].(string)
				}
			}
			if gotModel != c.wantModel {
				t.Errorf("model = %q, want %q; out=%s", gotModel, c.wantModel, out)
			}
		})
	}

	// off 模式：原样返回。
	t.Run("off_mode_passthrough", func(t *testing.T) {
		in := []byte(`{"model":"xopglm51"}`)
		out := rewriteModelJSON(in, RewriteConfig{Mode: ModeOff})
		if !bytes.Equal(out, in) {
			t.Errorf("off mode should pass through\ngot:  %q\nwant: %q", out, in)
		}
	})

	// passthrough 模式未命中：保留真名。
	t.Run("passthrough_miss_keeps_real", func(t *testing.T) {
		rcPass := RewriteConfig{Mode: ModePassthrough, Map: map[string]string{"xopglm51": "glm-5.1"}}
		out := rewriteModelJSON([]byte(`{"model":"xopglmv47flash"}`), rcPass)
		var parsed map[string]any
		_ = json.Unmarshal(out, &parsed)
		if parsed["model"] != "xopglmv47flash" {
			t.Errorf("passthrough miss should keep real; got %v", parsed["model"])
		}
	})

	// 其余字段保持不变（usage/content 等）。
	t.Run("other_fields_preserved", func(t *testing.T) {
		in := `{"model":"xopglm51","usage":{"prompt_tokens":10},"choices":[{"delta":{"content":"hi"}}]}`
		out := rewriteModelJSON([]byte(in), rc)
		var parsed map[string]any
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if parsed["model"] != "glm-5.1" {
			t.Errorf("model = %v, want glm-5.1", parsed["model"])
		}
		usage, _ := parsed["usage"].(map[string]any)
		if usage["prompt_tokens"] != float64(10) {
			t.Errorf("usage.prompt_tokens changed: %v", usage["prompt_tokens"])
		}
	})
}

func TestRewriteSSEEvent(t *testing.T) {
	rc := RewriteConfig{
		Mode:    ModePassthrough,
		Map:     map[string]string{"xopglm51": "glm-5.1"},
		Default: "glm-default",
	}

	t.Run("rewrites_data_payload", func(t *testing.T) {
		in := []byte("data: {\"model\":\"xopglm51\"}\n\n")
		out := rewriteSSEEvent(in, rc)
		if !bytes.Contains(out, []byte("glm-5.1")) {
			t.Errorf("expected glm-5.1 in output; got %q", out)
		}
		if bytes.Contains(out, []byte("xopglm51")) {
			t.Errorf("real name leaked; got %q", out)
		}
		// 事件边界空行保留。
		if !bytes.HasSuffix(out, []byte("\n\n")) {
			t.Errorf("event boundary lost; got %q", out)
		}
	})

	t.Run("preserves_event_line", func(t *testing.T) {
		in := []byte("event: message_start\ndata: {\"message\":{\"model\":\"xopglm51\"}}\n\n")
		out := rewriteSSEEvent(in, rc)
		if !bytes.Contains(out, []byte("event: message_start")) {
			t.Errorf("event line lost; got %q", out)
		}
		if !bytes.Contains(out, []byte("glm-5.1")) {
			t.Errorf("model not rewritten; got %q", out)
		}
	})

	t.Run("no_model_byte_exact", func(t *testing.T) {
		in := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		out := rewriteSSEEvent(in, rc)
		if !bytes.Equal(out, in) {
			t.Errorf("event without model should be byte-exact\ngot:  %q\nwant: %q", out, in)
		}
	})

	t.Run("done_marker_preserved", func(t *testing.T) {
		in := []byte("data: [DONE]\n\n")
		out := rewriteSSEEvent(in, rc)
		if !bytes.Equal(out, in) {
			t.Errorf("[DONE] should be byte-exact\ngot:  %q\nwant: %q", out, in)
		}
	})

	t.Run("off_mode_byte_exact", func(t *testing.T) {
		in := []byte("data: {\"model\":\"xopglm51\"}\n\n")
		out := rewriteSSEEvent(in, RewriteConfig{Mode: ModeOff})
		if !bytes.Equal(out, in) {
			t.Errorf("off mode should be byte-exact\ngot:  %q\nwant: %q", out, in)
		}
	})

	t.Run("crlf_input_handled", func(t *testing.T) {
		in := []byte("data: {\"model\":\"xopglm51\"}\r\n\r\n")
		out := rewriteSSEEvent(in, rc)
		if !bytes.Contains(out, []byte("glm-5.1")) {
			t.Errorf("model not rewritten for CRLF input; got %q", out)
		}
	})
}
