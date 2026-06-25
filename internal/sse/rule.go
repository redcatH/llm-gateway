// Package sse 实现对上游 SSE 响应的首帧 peek 与 error 拦截。
//
// 设计目标：在不破坏透明转发的前提下，识别上游"HTTP 200 + SSE 流内 error 帧"
// 的假成功，命中规则时拦截（如返回 503 让客户端重试），未命中则原样透传。
// 规则与处理逻辑解耦：Rule 只负责匹配，命中后交由 Handler 决策，便于逐步扩展。
package sse

import "strings"

// Match 描述从上游 SSE 首帧识别出的一个 error。
type Match struct {
	// Code 是 error.code（讯飞为数字，如 10012）。
	Code int
	// ErrorType 是 error.type（Anthropic 协议，如 "overloaded_error"）。
	// OpenAI 协议通常为空。
	ErrorType string
	// Message 是 error.message 原文。
	Message string
	// Raw 是该 data 帧的原始 JSON 字节，供日志/调试。
	Raw []byte
}

// Rule 定义一条拦截规则：匹配条件 + 处理 handler。
// 后续新增规则只需 append，不动 peek 核心逻辑。
type Rule struct {
	// Code 匹配 error.code；0 表示任意 code。
	Code int
	// ErrorType 匹配 error.type（Anthropic 协议）；空表示任意 type。
	ErrorType string
	// MsgContains 是子串匹配列表，AND 语义：error.message 必须同时包含全部子串才命中。
	// 空切片表示任意 message（仅按 Code 匹配）。配置多个子串可提高匹配准确性，避免误匹配。
	MsgContains []string
	// Handler 命中后调用的处理函数，返回决策。
	Handler Handler
}

// matches 判断一个 error 是否命中本规则。
// Code 需匹配（0 表示任意），ErrorType 需匹配（空表示任意），
// 且 MsgContains 中所有子串都必须出现在 message 中（AND 语义）。
func (r Rule) matches(m Match) bool {
	if r.Code != 0 && r.Code != m.Code {
		return false
	}
	if r.ErrorType != "" && r.ErrorType != m.ErrorType {
		return false
	}
	for _, sub := range r.MsgContains {
		if !strings.Contains(m.Message, sub) {
			return false
		}
	}
	return true
}
