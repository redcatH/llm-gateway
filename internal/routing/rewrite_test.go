package routing

import (
	"net/url"
	"testing"
)

func TestRewritePath(t *testing.T) {
	cases := []struct {
		target  string
		reqPath string
		want    string
	}{
		{"https://host/v2/", "/v1/messages", "/v2/messages"},
		{"https://host/v2/", "/v1/chat/completions", "/v2/chat/completions"},
		{"https://host/", "/v1/messages", "/messages"},
		{"https://host", "/v1/messages", "/v1/messages"},
		{"https://host/v2", "/v1/messages", "/v1/messages"},
		{"https://host/api/v3/", "/v1/chat/completions", "/api/v3/chat/completions"},
		{"https://host/v2/", "/v1/responses", "/v2/responses"},
	}
	for _, c := range cases {
		tgt, _ := url.Parse(c.target)
		got := RewritePath(tgt, c.reqPath)
		if got != c.want {
			t.Errorf("target=%s req=%s: got %q, want %q", c.target, c.reqPath, got, c.want)
		}
	}
}
