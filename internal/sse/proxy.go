package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"xunfei-gateway/internal/routing"
)

// hopByHopHeaders 是 RFC 7230 规定的逐跳头，转发响应时必须剥离
// （与 httputil.ReverseProxy 行为一致，属协议强制，非内容变更）。
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailers",
	"Transfer-Encoding",
	"Upgrade",
}

// ProxyHandler 返回一个处理所有请求的 http.Handler。
//
// 对上游响应为 200 + SSE 的请求做首帧 peek：
//   - 首帧命中拦截规则 → 按 Handler 决策响应（如 503），丢弃原流；
//   - 否则（非命中 / 非 error / 非 SSE / 非 200）→ 原样透传，字节级一致。
//
// 分流依据是上游响应（resp.StatusCode==200 且 Content-Type 含 text/event-stream），
// 而非请求的 Accept 头，更可靠。
func ProxyHandler(openAITarget, anthropicTarget *url.URL, preserveHost bool, transport http.RoundTripper, rules []Rule, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 按请求协议（路径）选上游：/v1/messages → Anthropic，其余 → OpenAI。
		target := routing.SelectTarget(req, openAITarget, anthropicTarget)
		outReq := buildUpstreamRequest(req, target, preserveHost)

		resp, err := transport.RoundTrip(outReq)
		if err != nil {
			logger.Error("upstream roundtrip error", "err", err.Error(), "path", req.URL.Path)
			writeJSONError(w, http.StatusBadGateway, "upstream unreachable")
			return
		}
		defer resp.Body.Close()

		// 仅对 200 + SSE 响应做 peek 拦截；其余原样透传。
		if resp.StatusCode == http.StatusOK && isSSEResponse(resp) {
			handleSSEResponse(w, resp, rules, logger, req)
			return
		}
		copyResponse(w, resp)
	})
}

// handleSSEResponse 处理 200 + SSE 响应：peek 首帧，命中规则则拦截，否则放行续传。
func handleSSEResponse(w http.ResponseWriter, resp *http.Response, rules []Rule, logger *slog.Logger, req *http.Request) {
	br := bufio.NewReader(resp.Body)
	peeked, err := readFirstEvent(br)

	// 解析首帧是否 error 帧。
	if m, ok := parseErrorEvent(peeked); ok {
		matched := false
		for _, rule := range rules {
			if rule.matches(m) {
				matched = true
				d := rule.Handler(req, m)
				if d.Intercept {
					logger.Info("sse error intercepted",
						"code", m.Code,
						"msg", m.Message,
						"raw", string(m.Raw),
						"rule_msg_contains", rule.MsgContains,
						"path", req.URL.Path,
					)
					writeDecision(w, d)
					return
				}
				// Handler 决定放行（Intercept=false）→ 记录后跳出匹配，继续透传。
				logger.Info("sse error matched but passed through",
					"code", m.Code,
					"msg", m.Message,
					"raw", string(m.Raw),
					"rule_msg_contains", rule.MsgContains,
					"path", req.URL.Path,
				)
				break
			}
		}
		if !matched {
			logger.Warn("sse error not matched by any rule",
				"code", m.Code,
				"msg", m.Message,
				"raw", string(m.Raw),
				"path", req.URL.Path,
			)
		}
	}

	// 读取异常时仅记日志，仍尽力放行已读字节（保守不吞流）。
	if err != nil && err != io.EOF {
		logger.Warn("peek first sse event ended with error, passing through", "err", err.Error())
	}
	forwardStream(w, resp, peeked, br)
}

// buildUpstreamRequest 构造转发到上游的请求：
// 复制入站 header/body，URL 用 target 的 scheme/host + 原始 path/query，清空 RequestURI。
func buildUpstreamRequest(req *http.Request, target *url.URL, preserveHost bool) *http.Request {
	outReq := req.Clone(req.Context())
	outReq.URL = &url.URL{
		Scheme:   target.Scheme,
		Host:     target.Host,
		Path:     req.URL.Path,
		RawPath:  req.URL.RawPath,
		RawQuery: req.URL.RawQuery,
		Fragment: req.URL.Fragment,
	}
	if target.User != nil {
		outReq.URL.User = target.User
	}
	if preserveHost {
		outReq.Host = req.Host
	} else {
		outReq.Host = target.Host
	}
	outReq.RequestURI = "" // 出站请求必须清空
	return outReq
}

// readFirstEvent 从 bufio.Reader 读取第一个完整的 SSE 事件（以空行分隔）。
// 返回该事件的原始字节（含事件边界空行）。遇到 EOF 时返回已读字节与 io.EOF。
func readFirstEvent(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			buf.Write(line)
		}
		// 空行（去掉行尾 \r\n 后为空）= 事件边界。
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			return buf.Bytes(), nil
		}
		if err != nil {
			return buf.Bytes(), err
		}
	}
}

// parseErrorEvent 从一个 SSE 事件字节中解析 error 帧。
// 识别 data: 行，提取 JSON 中的 error.code 与 error.message。
// 返回 (Match, true) 若是 error 帧；否则 (Match{}, false)。
//
// 注意：code 用 json.RawMessage 接收以兼容数字（讯飞 10012）与字符串两种形态，
// 数字形态会解析到 Match.Code；字符串形态时 Code 为 0（仍可按 message 匹配）。
func parseErrorEvent(event []byte) (Match, bool) {
	for _, raw := range bytes.Split(event, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var obj struct {
			Error struct {
				Code    json.RawMessage `json:"code"`
				Message string          `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(payload, &obj); err != nil {
			continue
		}
		// 存在 error.code 或 error.message 即视为 error 帧。
		if len(obj.Error.Code) == 0 && obj.Error.Message == "" {
			continue
		}
		m := Match{Message: obj.Error.Message, Raw: payload}
		var codeInt int
		if json.Unmarshal(obj.Error.Code, &codeInt) == nil {
			m.Code = codeInt
		}
		return m, true
	}
	return Match{}, false
}

// isSSEResponse 判断上游响应是否为 SSE 流。
func isSSEResponse(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

// copyResponse 将上游响应原样透传给下游（status + header + body）。
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	copyBodyAndFlush(w, resp.Body)
}

// forwardStream 放行 SSE 流：写响应头 + 已 peek 字节 + 剩余 body，逐段 flush。
// 已 peek 的字节先写出再续传剩余，保证透传字节级完整。
func forwardStream(w http.ResponseWriter, resp *http.Response, peeked []byte, br *bufio.Reader) {
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	if len(peeked) > 0 {
		_, _ = w.Write(peeked)
		if flusher != nil {
			flusher.Flush()
		}
	}
	// br 已消费首事件，继续读剩余 body。
	_, _ = io.Copy(w, br)
	if flusher != nil {
		flusher.Flush()
	}
}

// copyHeader 复制响应头，剥离 hop-by-hop 头。
func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		if isHopByHop(k) {
			continue
		}
		dst[k] = vs
	}
}

func isHopByHop(k string) bool {
	for _, h := range hopByHopHeaders {
		if strings.EqualFold(k, h) {
			return true
		}
	}
	return false
}

// copyBodyAndFlush 把 reader 内容复制到 writer 并 flush（用于非流式透传）。
func copyBodyAndFlush(w io.Writer, r io.Reader) {
	_, _ = io.Copy(w, r)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// writeDecision 按 Handler 的决策写响应。
func writeDecision(w http.ResponseWriter, d Decision) {
	for k, vs := range d.Headers {
		for _, v := range vs {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(d.Status)
	if len(d.Body) > 0 {
		_, _ = w.Write(d.Body)
	}
}

// writeJSONError 写一个简单的 JSON 错误响应（不泄漏内部细节）。
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]string{"error": msg})
	_, _ = w.Write(body)
}
