package tokencount

import "encoding/json"

// 每个非文本块(图片/音频)的保守 token 估算。
// 图片通常数百到一千多 token，1000 偏高防误判到 500k。
const nonTextBlockTokens = 1000

// Count 估算一个 LLM 请求体的输入 token 数。
// protocol: "openai" 或 "anthropic"。
// ok=false 表示走了字节长度兜底（解析失败或未识别出 messages），调用方可记日志。
// 兜底同样偏高：len(body)/3（UTF-8 中文 3 字节≈1 字符≈偏高；ASCII 也偏高）。
func Count(est Estimator, body []byte, protocol string) (tokens int, ok bool) {
	if len(body) == 0 {
		return 0, true
	}
	var text string
	var nonText int
	var found bool
	if protocol == "anthropic" {
		text, nonText, found = extractAnthropic(body)
	} else {
		text, nonText, found = extractOpenAI(body)
	}
	if !found && nonText == 0 {
		// 未识别出结构化文本 → 字节长度兜底（偏高，安全）
		return len(body) / 3, false
	}
	return est.Estimate(text) + nonText*nonTextBlockTokens, true
}

// Model 从请求体提取顶层 model 字段。两协议格式一致。
// 无 model 字段或解析失败返回 ""。
func Model(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return v.Model
}

// ── OpenAI 格式提取 ──

func extractOpenAI(body []byte) (text string, nonText int, found bool) {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
		Input    json.RawMessage    `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", 0, false
	}

	if len(req.Messages) > 0 {
		found = true
	}
	for _, raw := range req.Messages {
		var msg struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		t, n := extractOpenAIContent(msg.Content)
		text += t
		nonText += n
	}

	// /v1/responses 用 input 而非 messages
	if !found && len(req.Input) > 0 {
		found = true
		// input 可能是 string 或 array；string 直接当文本
		var s string
		if json.Unmarshal(req.Input, &s) == nil {
			text += s
		}
		// array 形式的 input 暂不深入，走字节兜底即可
	}

	return text, nonText, found
}

// extractOpenAIContent 从 OpenAI message 的 content 字段提取文本。
// content 可以是 string 或 []part（{type:"text",text} / {type:"image_url",...}）。
func extractOpenAIContent(raw json.RawMessage) (text string, nonText int) {
	// 尝试 string
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, 0
	}

	// 尝试 []part
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL any    `json:"image_url"` // ponytail: 不深入解析，仅判断存在
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", 0
	}
	for _, p := range parts {
		switch p.Type {
		case "text":
			text += p.Text
		default:
			nonText++
		}
	}
	return text, nonText
}

// ── Anthropic 格式提取 ──

func extractAnthropic(body []byte) (text string, nonText int, found bool) {
	var req struct {
		System   json.RawMessage   `json:"system"`
		Messages []json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "", 0, false
	}

	// 顶层 system
	if len(req.System) > 0 {
		found = true
		t, n := extractAnthropicSystem(req.System)
		text += t
		nonText += n
	}

	// messages
	if len(req.Messages) > 0 {
		found = true
	}
	for _, raw := range req.Messages {
		var msg struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &msg) != nil {
			continue
		}
		t, n := extractAnthropicContent(msg.Content)
		text += t
		nonText += n
	}

	return text, nonText, found
}

// extractAnthropicSystem 从 Anthropic 顶层 system 字段提取文本。
// system 可以是 string 或 []block。
func extractAnthropicSystem(raw json.RawMessage) (text string, nonText int) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, 0
	}
	return extractAnthropicBlocks(raw)
}

// extractAnthropicContent 从 Anthropic message 的 content 字段提取文本。
// content 是 []block。
func extractAnthropicContent(raw json.RawMessage) (text string, nonText int) {
	return extractAnthropicBlocks(raw)
}

// extractAnthropicBlocks 从 Anthropic block 数组提取文本。
// text → 取 text；tool_use → 序列化 input 为文本（偏高）；tool_result → 取 content 文本；其它 → nonText++。
func extractAnthropicBlocks(raw json.RawMessage) (text string, nonText int) {
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"` // tool_result 的 content（string 或 []block）
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return "", 0
	}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text += b.Text
		case "tool_use":
			// input 是上下文，序列化为文本计入（偏高，安全）
			if len(b.Input) > 0 {
				text += string(b.Input)
			}
		case "tool_result":
			t, n := extractToolResultContent(b.Content)
			text += t
			nonText += n
		default:
			nonText++
		}
	}
	return text, nonText
}

// extractToolResultContent 从 tool_result 的 content 提取文本。
// content 可以是 string 或 []block。
func extractToolResultContent(raw json.RawMessage) (text string, nonText int) {
	if len(raw) == 0 {
		return "", 0
	}
	// 尝试 string
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, 0
	}
	// 尝试 []block
	return extractAnthropicBlocks(raw)
}
