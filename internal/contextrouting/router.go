// Package contextrouting 按请求路径 + 估算输入 token 在 500k 默认上游与
// 1M 上下文上游之间选择目标，并决定是否替换出站鉴权头。
package contextrouting

import (
	"net/url"

	"llm-gateway/internal/routing"
	"llm-gateway/internal/tokencount"
)

// Router 持有路由决策所需的全部配置。
type Router struct {
	OpenAI500k    *url.URL // 500k 默认(xopglm52)，必非空
	Anthropic500k *url.URL // 500k 默认，必非空
	OpenAI1M      *url.URL // 1M(glm-5.2)，nil=该协议不支持升级
	Anthropic1M   *url.URL // 同上
	Token1M       string   // 1M 固定 token；空=无法升级
	Threshold     int      // 触发升级的输入 token 阈值
	Enabled       bool     // 全局开关
	RoutingModel500k string // 触发判定的 500k model 名（默认 xopglm52）
	Estimator     tokencount.Estimator
}

// Decision 是单次路由决策结果。
type Decision struct {
	Target *url.URL // 选中的上游(500k 或 1M)
	Is1M   bool     // true=走 1M，调用方需替换 Authorization
	Tokens int      // 估算输入 token(日志用)
	Reason string   // upgraded/below_threshold/disabled/no_1m_target/no_token/not_target_model/parse_fallback
}

// RouteLabel 返回路由标签（日志用）。
func (d Decision) RouteLabel() string {
	if d.Is1M {
		return "1m"
	}
	return "500k"
}

// Decide 按路径与请求体选择目标。
func (r *Router) Decide(reqPath string, reqBody []byte) Decision {
	isAnthro := routing.IsAnthropicPath(reqPath)
	base500k := r.OpenAI500k
	oneM := r.OpenAI1M
	protocol := "openai"
	if isAnthro {
		base500k = r.Anthropic500k
		oneM = r.Anthropic1M
		protocol = "anthropic"
	}

	// 1) 全局开关关闭 → 直接走 500k
	if !r.Enabled {
		return Decision{Target: base500k, Reason: "disabled"}
	}

	// 2) 前置 model 匹配：仅目标 500k model 才介入
	model := tokencount.Model(reqBody)
	if model != r.RoutingModel500k {
		return Decision{Target: base500k, Reason: "not_target_model"}
	}

	// 3) 估算 token
	tokens, ok := tokencount.Count(r.Estimator, reqBody, protocol)
	reason := "below_threshold"
	if !ok {
		reason = "parse_fallback"
	}

	// 4) 低于阈值 → 500k
	if tokens < r.Threshold {
		return Decision{Target: base500k, Tokens: tokens, Reason: reason}
	}

	// 5) 达到/超过阈值 → 尝试升级
	if oneM == nil {
		return Decision{Target: base500k, Tokens: tokens, Reason: "no_1m_target"}
	}
	if r.Token1M == "" {
		return Decision{Target: base500k, Tokens: tokens, Reason: "no_token"}
	}
	return Decision{Target: oneM, Is1M: true, Tokens: tokens, Reason: "upgraded"}
}
