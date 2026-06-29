package tokencount

import "testing"

func TestApproxEstimator_ASCII(t *testing.T) {
	est := ApproxEstimator{}
	// 30 ASCII chars → ceil(30/3) = 10
	got := est.Estimate("Hello world, this is a test!!")
	if got != 10 {
		t.Errorf("ASCII 30 chars: got %d, want 10", got)
	}
}

func TestApproxEstimator_CJK(t *testing.T) {
	est := ApproxEstimator{}
	// 5 CJK chars → 5 tokens
	got := est.Estimate("你好世界吗")
	if got != 5 {
		t.Errorf("CJK 5 chars: got %d, want 5", got)
	}
}

func TestApproxEstimator_Mixed(t *testing.T) {
	est := ApproxEstimator{}
	// 3 CJK + 6 ASCII → 3 + ceil(6/3) = 5
	got := est.Estimate("你好abc世界")
	if got != 5 {
		t.Errorf("Mixed: got %d, want 5", got)
	}
}

func TestApproxEstimator_CJKPunctuation(t *testing.T) {
	est := ApproxEstimator{}
	// ，。！ are in CJK punctuation range (0x3000-0x303F)
	// 你好(2) + ，(1) + 世界(2) + ！(1) = 6
	got := est.Estimate("你好，世界！")
	if got != 6 {
		t.Errorf("CJK punctuation: got %d, want 6", got)
	}
}

func TestApproxEstimator_Fullwidth(t *testing.T) {
	est := ApproxEstimator{}
	// ＡＢ are fullwidth (0xFF00-0xFFEF)
	got := est.Estimate("ＡＢ")
	if got != 2 {
		t.Errorf("Fullwidth: got %d, want 2", got)
	}
}

func TestApproxEstimator_Japanese(t *testing.T) {
	est := ApproxEstimator{}
	// ひらがな + カタカナ in 0x3040-0x30FF
	got := est.Estimate("ひらがなカタカナ")
	if got != 8 {
		t.Errorf("Japanese: got %d, want 8", got)
	}
}

func TestApproxEstimator_Empty(t *testing.T) {
	est := ApproxEstimator{}
	if est.Estimate("") != 0 {
		t.Error("empty should be 0")
	}
}

// ── Count / extract ──

func TestCount_OpenAI_StringContent(t *testing.T) {
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"hello"}]}`)
	tokens, ok := Count(ApproxEstimator{}, body, "openai")
	if !ok {
		t.Error("should be ok")
	}
	if tokens == 0 {
		t.Error("tokens should be > 0")
	}
}

func TestCount_OpenAI_ArrayContent(t *testing.T) {
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:..."}}]}]}`)
	tokens, ok := Count(ApproxEstimator{}, body, "openai")
	if !ok {
		t.Error("should be ok")
	}
	// "hello" = ceil(5/3)=2 + 1 nonText block = 1002
	if tokens != 1002 {
		t.Errorf("got %d, want 1002", tokens)
	}
}

func TestCount_OpenAI_SystemMessage(t *testing.T) {
	body := []byte(`{"model":"xopglm52","messages":[{"role":"system","content":"You are helpful"},{"role":"user","content":"hi"}]}`)
	tokens, ok := Count(ApproxEstimator{}, body, "openai")
	if !ok {
		t.Error("should be ok")
	}
	if tokens == 0 {
		t.Error("tokens should be > 0")
	}
}

func TestCount_Anthropic_SystemString(t *testing.T) {
	body := []byte(`{"model":"xopglm52","system":"You are helpful","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	tokens, ok := Count(ApproxEstimator{}, body, "anthropic")
	if !ok {
		t.Error("should be ok")
	}
	if tokens == 0 {
		t.Error("tokens should be > 0")
	}
}

func TestCount_Anthropic_SystemArray(t *testing.T) {
	body := []byte(`{"model":"xopglm52","system":[{"type":"text","text":"Be helpful"}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	tokens, ok := Count(ApproxEstimator{}, body, "anthropic")
	if !ok {
		t.Error("should be ok")
	}
	if tokens == 0 {
		t.Error("tokens should be > 0")
	}
}

func TestCount_Anthropic_ToolUse(t *testing.T) {
	body := []byte(`{"model":"xopglm52","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"get_weather","input":{"city":"Beijing"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"Sunny 25C"}]}]}`)
	tokens, ok := Count(ApproxEstimator{}, body, "anthropic")
	if !ok {
		t.Error("should be ok")
	}
	if tokens == 0 {
		t.Error("tokens should be > 0")
	}
}

func TestCount_InvalidJSON(t *testing.T) {
	body := []byte(`not json at all but somewhat long enough to matter`)
	tokens, ok := Count(ApproxEstimator{}, body, "openai")
	if ok {
		t.Error("invalid JSON should return ok=false")
	}
	if tokens == 0 {
		t.Error("fallback should be > 0")
	}
}

func TestCount_EmptyBody(t *testing.T) {
	tokens, ok := Count(ApproxEstimator{}, nil, "openai")
	if !ok {
		t.Error("empty body should be ok")
	}
	if tokens != 0 {
		t.Error("empty body should be 0 tokens")
	}
}

func TestCount_PureNonText(t *testing.T) {
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:..."}}]}]}`)
	tokens, ok := Count(ApproxEstimator{}, body, "openai")
	if !ok {
		t.Error("should be ok")
	}
	if tokens != nonTextBlockTokens {
		t.Errorf("got %d, want %d", tokens, nonTextBlockTokens)
	}
}

// ── Model ──

func TestModel_Present(t *testing.T) {
	got := Model([]byte(`{"model":"xopglm52","messages":[]}`))
	if got != "xopglm52" {
		t.Errorf("got %q, want %q", got, "xopglm52")
	}
}

func TestModel_Absent(t *testing.T) {
	got := Model([]byte(`{"messages":[]}`))
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestModel_InvalidJSON(t *testing.T) {
	got := Model([]byte(`not json`))
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// ── Pluggable Estimator ──

type doubleEstimator struct{}

func (doubleEstimator) Estimate(text string) int { return len(text) * 2 }

func TestCount_CustomEstimator(t *testing.T) {
	body := []byte(`{"model":"xopglm52","messages":[{"role":"user","content":"abc"}]}`)
	tokens, ok := Count(doubleEstimator{}, body, "openai")
	if !ok {
		t.Error("should be ok")
	}
	// "abc" → doubleEstimator gives 6
	if tokens != 6 {
		t.Errorf("got %d, want 6", tokens)
	}
}
