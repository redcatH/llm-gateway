package routing

import (
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestSelectTarget(t *testing.T) {
	openai := &url.URL{Host: "openai.example.com"}
	anthropic := &url.URL{Host: "anthropic.example.com"}

	cases := []struct {
		path string
		want string // 期望命中的上游 Host
	}{
		{"/v1/messages", "anthropic.example.com"},
		{"/v1/messages/count_tokens", "anthropic.example.com"},
		{"/antigravity/v1/messages", "anthropic.example.com"},
		{"/v1/chat/completions", "openai.example.com"},
		{"/v1/responses", "openai.example.com"},
		{"/v1beta/models/gemini:generateContent", "openai.example.com"},
		{"/anything-else", "openai.example.com"},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			req := httptest.NewRequest("POST", c.path, nil)
			got := SelectTarget(req, openai, anthropic)
			if got == nil || got.Host != c.want {
				t.Errorf("SelectTarget(%q) host = %v, want %s", c.path, got, c.want)
			}
		})
	}
}
