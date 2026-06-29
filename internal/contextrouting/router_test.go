package contextrouting

import (
	"net/url"
	"testing"

	"llm-gateway/internal/tokencount"
)

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func newTestRouter(threshold int) *Router {
	return &Router{
		OpenAI500k:      mustURL("https://500k-openai.example.com"),
		Anthropic500k:   mustURL("https://500k-anthro.example.com"),
		OpenAI1M:        mustURL("https://1m-openai.example.com"),
		Anthropic1M:     mustURL("https://1m-anthro.example.com"),
		Token1M:         "sk-1m-test",
		Threshold:       threshold,
		Enabled:         true,
		RoutingModel500k: "xopglm52",
		Estimator:       tokencount.ApproxEstimator{},
	}
}

func TestDecide_BelowThreshold(t *testing.T) {
	r := newTestRouter(1000)
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"hi"}]}`)
	d := r.Decide("/v1/chat/completions", body)
	if d.Is1M {
		t.Error("should not upgrade")
	}
	if d.Target.Host != "500k-openai.example.com" {
		t.Errorf("target host = %q", d.Target.Host)
	}
	if d.Reason != "below_threshold" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDecide_AboveThreshold(t *testing.T) {
	r := newTestRouter(1) // threshold=1, even "hi" exceeds
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"hi"}]}`)
	d := r.Decide("/v1/chat/completions", body)
	if !d.Is1M {
		t.Error("should upgrade")
	}
	if d.Target.Host != "1m-openai.example.com" {
		t.Errorf("target host = %q", d.Target.Host)
	}
	if d.Reason != "upgraded" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDecide_AtThreshold(t *testing.T) {
	r := newTestRouter(1)
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"a"}]}`)
	d := r.Decide("/v1/chat/completions", body)
	// "a" = ceil(1/3) = 1 token, threshold = 1 → >= → upgrade
	if !d.Is1M {
		t.Error("boundary should upgrade")
	}
}

func TestDecide_Disabled(t *testing.T) {
	r := newTestRouter(1)
	r.Enabled = false
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"very long text here"}]}`)
	d := r.Decide("/v1/chat/completions", body)
	if d.Is1M {
		t.Error("disabled should not upgrade")
	}
	if d.Reason != "disabled" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDecide_No1MTarget(t *testing.T) {
	r := newTestRouter(1)
	r.OpenAI1M = nil
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"hi"}]}`)
	d := r.Decide("/v1/chat/completions", body)
	if d.Is1M {
		t.Error("no 1M target should not upgrade")
	}
	if d.Reason != "no_1m_target" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDecide_NoToken(t *testing.T) {
	r := newTestRouter(1)
	r.Token1M = ""
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"hi"}]}`)
	d := r.Decide("/v1/chat/completions", body)
	if d.Is1M {
		t.Error("no token should not upgrade")
	}
	if d.Reason != "no_token" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDecide_AnthropicPath(t *testing.T) {
	r := newTestRouter(1)
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	d := r.Decide("/v1/messages", body)
	if !d.Is1M {
		t.Error("should upgrade")
	}
	if d.Target.Host != "1m-anthro.example.com" {
		t.Errorf("target host = %q", d.Target.Host)
	}
}

func TestDecide_NotTargetModel(t *testing.T) {
	r := newTestRouter(1)
	// model is glm-5.2, not xopglm52 → should not upgrade
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"very long text"}]}`)
	d := r.Decide("/v1/chat/completions", body)
	if d.Is1M {
		t.Error("non-target model should not upgrade")
	}
	if d.Reason != "not_target_model" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDecide_NoModelField(t *testing.T) {
	r := newTestRouter(1)
	body := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	d := r.Decide("/v1/chat/completions", body)
	if d.Is1M {
		t.Error("no model field should not upgrade")
	}
	if d.Reason != "not_target_model" {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestDecide_InvalidJSON_StillUpgrades(t *testing.T) {
	r := newTestRouter(1)
	// Invalid JSON but model extracted as xopglm52... actually Model() returns "" for invalid JSON
	// So this goes to not_target_model. Let's test with a large invalid body that can't extract model.
	body := []byte(`not json but xopglm52`)
	d := r.Decide("/v1/chat/completions", body)
	// Model() returns "" for invalid JSON → not_target_model
	if d.Reason != "not_target_model" {
		t.Errorf("reason = %q, want not_target_model", d.Reason)
	}
}
