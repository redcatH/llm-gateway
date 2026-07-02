package sse

import (
	"bytes"
	"encoding/json"
)

// 响应 model 字段改写模式。值为字符串以便与 config 层（环境变量解析结果）直接对接。
const (
	ModeOff         = "off"         // 关闭改写，纯透传
	ModePassthrough = "passthrough" // 命中改写，未命中透传真名
	ModeDefault     = "default"     // 命中改写，未命中用 Default
)

// RewriteConfig 封装响应 model 改写所需的全部配置。
// 零值（Mode 为空）等价于 ModeOff，改写关闭——未配置时现有调用方安全。
type RewriteConfig struct {
	Mode    string            // off / passthrough / default
	Map     map[string]string // 真名→对外名
	Default string            // default 模式的兜底名
}

// enabled 报告改写是否启用。仅 passthrough/default 启用；
// 空值与 off 均视为关闭（零值 RewriteConfig 安全）。
func (rc RewriteConfig) enabled() bool {
	return rc.Mode == ModePassthrough || rc.Mode == ModeDefault
}

// mapModel 将真实模型名映射为对外名。
// 命中 Map 返回对外名；未命中按 mode 处理：
//   - default：返回 Default（Load 已校验非空）
//   - passthrough 或其他：返回原值
func mapModel(real string, rc RewriteConfig) string {
	if mapped, ok := rc.Map[real]; ok {
		return mapped
	}
	if rc.Mode == ModeDefault {
		return rc.Default
	}
	return real
}

// rewriteModelJSON 解析 JSON，递归改写所有 string 类型的 model 字段，重序列化返回。
// 解析失败 / 无 model 字段 / 值未变 / 改写关闭时原样返回 data（保守透传，不吞流）。
//
// 递归安全：LLM 响应中 model 字段名唯一，content/tool_calls/usage 等结构不含
// model 字段，递归只会命中真正的 model 字段（OpenAI 顶层、Anthropic message.model）。
// 重序列化会改变字段顺序与空格，但语义等价，LLM SDK 不依赖字节顺序。
func rewriteModelJSON(data []byte, rc RewriteConfig) []byte {
	if !rc.enabled() {
		return data
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return data // 解析失败，原样透传
	}
	if !rewriteModelInPlace(v, rc) {
		return data // 无 model 字段或值未变
	}
	out, err := json.Marshal(v)
	if err != nil {
		return data
	}
	return out
}

// rewriteModelInPlace 递归遍历 v，将所有 string 类型的 model 字段按 rc 改写。
// 返回是否有字段被实际修改（用于决定是否重序列化）。
func rewriteModelInPlace(v any, rc RewriteConfig) bool {
	switch val := v.(type) {
	case map[string]any:
		changed := false
		for k, item := range val {
			if k == "model" {
				if s, ok := item.(string); ok {
					if mapped := mapModel(s, rc); mapped != s {
						val[k] = mapped
						changed = true
					}
				}
				continue // model 值不会是含 model 子对象的 map，无需再递归
			}
			if rewriteModelInPlace(item, rc) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, item := range val {
			if rewriteModelInPlace(item, rc) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

// rewriteSSEEvent 改写一个 SSE 事件（可能含多行）中的 model 字段。
// 逐行处理：对每个 "data:" 行的 JSON payload 调 rewriteModelJSON；
// 非 data 行（event:/id:/retry:/注释/空行/[DONE]）原样保留。
// 用 bytes.Contains(event,"model") 短路：不含 model 的事件零解析直接返回原字节。
//
// 行尾统一输出为 \n（输入 \r\n 或 \n 均归一化），SSE 解析器两种都支持。
func rewriteSSEEvent(event []byte, rc RewriteConfig) []byte {
	if !rc.enabled() || !bytes.Contains(event, []byte("model")) {
		return event
	}
	lines := bytes.Split(event, []byte("\n"))
	var out bytes.Buffer
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n') // 恢复 Split 去掉的行分隔
		}
		trimmed := bytes.TrimSpace(line)
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if len(payload) > 0 && !bytes.Equal(payload, []byte("[DONE]")) {
				payload = rewriteModelJSON(payload, rc)
			}
			out.WriteString("data: ")
			out.Write(payload)
			continue
		}
		out.Write(line) // 非 data 行原样（含可能的 \r）
	}
	return out.Bytes()
}
