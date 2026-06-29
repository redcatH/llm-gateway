// Package tokencount 提供请求体输入 token 的估算。
// 刻意零依赖：默认实现用字符系数近似(CJK/ASCII 区分)，可经 Estimator 接口
// 替换为 tiktoken 等精确实现，调用方(路由层)无需改动。
package tokencount

// Estimator 估算一段文本的 token 数。可插拔。
type Estimator interface {
	Estimate(text string) int
}

// ApproxEstimator 是默认的近似估算器：CJK 与 ASCII 分别按系数计算，偏高。
// 偏高是有意为之——宁可误升级到 1M，也不要漏判导致 500k 上游 context_window_exceeded。
type ApproxEstimator struct{}

const (
	cjkTokensPerChar   = 1 // CJK：1 字符 ≈ 1 token（真实约 0.7，偏高留余量）
	asciiCharsPerToken = 3 // ASCII：约 3 字符/token（经典 4 字符/token，偏高）
)

func (ApproxEstimator) Estimate(text string) int {
	var cjk, ascii int
	for _, r := range text {
		if isCJK(r) {
			cjk++
		} else {
			ascii++
		}
	}
	return cjk + ceilDiv(ascii, asciiCharsPerToken)
}

func ceilDiv(a, b int) int { return (a + b - 1) / b }

// isCJK 判断 rune 是否属于 CJK 相关 Unicode 区段。
// 这些区段的字符在主流分词器中接近 1 字符 ≈ 1 token，偏高估算。
func isCJK(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF:  return true // CJK Unified Ideographs（常用汉字）
	case r >= 0x3400 && r <= 0x4DBF:  return true // CJK Extension A
	case r >= 0x20000 && r <= 0x2A6DF: return true // CJK Extension B
	case r >= 0x3040 && r <= 0x30FF:  return true // 平假名 + 片假名
	case r >= 0xAC00 && r <= 0xD7AF:  return true // 韩文音节
	case r >= 0x3000 && r <= 0x303F:  return true // CJK 标点（，。！等）
	case r >= 0xFF00 && r <= 0xFFEF:  return true // 全角/半角形式
	default:
		return false
	}
}
