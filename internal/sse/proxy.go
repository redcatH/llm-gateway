package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/contextrouting"
	"llm-gateway/internal/routing"
)

// codeInMessage 匹配 message 字符串里的 "code: NNNN"（讯飞把真实错误码嵌在文案里）。
var codeInMessage = regexp.MustCompile(`code:\s*(\d+)`)

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

// pickTarget 决定本次请求的上游、是否走 1M 鉴权替换、以及 1M token。
type pickTarget func(req *http.Request, reqBody []byte) (target *url.URL, is1M bool, token1M string)

// ProxyHandler 返回一个处理所有请求的 http.Handler（无上下文路由，纯按协议选上游）。
//
// 对上游响应为 200 + SSE 的请求做首帧 peek：
//   - 首帧命中拦截规则 → 按 Handler 决策响应（如 503），丢弃原流；
//   - 否则（非命中 / 非 error / 非 SSE / 非 200）→ 原样透传，字节级一致。
//
// 分流依据是上游响应（resp.StatusCode==200 且 Content-Type 含 text/event-stream），
// 而非请求的 Accept 头，更可靠。
func ProxyHandler(openAITarget, anthropicTarget *url.URL, preserveHost bool, transport http.RoundTripper, rules []Rule, logger *slog.Logger, logDir string) http.Handler {
	pick := func(req *http.Request, _ []byte) (*url.URL, bool, string) {
		return routing.SelectTarget(req, openAITarget, anthropicTarget), false, ""
	}
	return serveProxy(pick, preserveHost, transport, rules, logger, logDir)
}

// ProxyHandlerWithRouting 返回带上下文路由的 http.Handler。
// 仅当请求 model 为 500k 模型且估算 input token >= 阈值时，自动切换到 1M 上游
// 并用配置的固定 token 替换出站 Authorization。其余请求走 500k 默认透传。
func ProxyHandlerWithRouting(router *contextrouting.Router, preserveHost bool, transport http.RoundTripper, rules []Rule, logger *slog.Logger, logDir string) http.Handler {
	pick := func(req *http.Request, reqBody []byte) (*url.URL, bool, string) {
		d := router.Decide(req.URL.Path, reqBody)
		logger.Debug("context routing decision",
			"path", req.URL.Path,
			"route", d.RouteLabel(),
			"est_tokens", d.Tokens,
			"threshold", router.Threshold,
			"reason", d.Reason,
		)
		if d.Is1M {
			logger.Info("routed to 1m upstream",
				"path", req.URL.Path, "est_tokens", d.Tokens, "threshold", router.Threshold,
			)
		}
		if d.Reason == "no_1m_target" || d.Reason == "no_token" {
			logger.Warn("token routing cannot upgrade",
				"path", req.URL.Path, "reason", d.Reason, "est_tokens", d.Tokens,
			)
		}
		return d.Target, d.Is1M, router.Token1M
	}
	return serveProxy(pick, preserveHost, transport, rules, logger, logDir)
}

// serveProxy 是 ProxyHandler / ProxyHandlerWithRouting 的共享实现。
// pick 在读 body 后调用，决定上游目标、是否 1M、1M token。
func serveProxy(pick pickTarget, preserveHost bool, transport http.RoundTripper, rules []Rule, logger *slog.Logger, logDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		// 缓存请求体用于 error 诊断 + token 估算；重置 req.Body 为可重读 reader，保证转发不受影响。
		// 必须在 RoundTrip 前：req.Clone() 浅拷贝 Body，RoundTrip 会消费掉原始 reader。
		var reqBody []byte
		if req.Body != nil {
			reqBody, _ = io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(reqBody))
		}

		// 按路径 + 估算 token 选上游（500k 默认 / 1M 升级）。
		target, is1M, token1M := pick(req, reqBody)

		logger.Debug("incoming request",
			"method", req.Method,
			"path", req.URL.Path,
			"target_host", target.Host,
			"route", func() string {
				if is1M {
					return "1m"
				}
				return "500k"
			}(),
			"accept", req.Header.Get("Accept"),
		)

		outReq := buildUpstreamRequest(req, target, preserveHost)

		// 仅 1M 路由：用配置的固定 token 覆盖出站鉴权头。
		// 500k 路由完全不碰 header → 透明透传不变。
		// ⚠️ 这是透明透传原则的有意例外，仅限 1M 分支（见 README）。
		if is1M {
			apply1MAuth(outReq, token1M)
		}

		resp, err := transport.RoundTrip(outReq)
		if err != nil {
			logger.Error("upstream roundtrip error", "err", err.Error(), "path", req.URL.Path)
			writeJSONError(w, http.StatusBadGateway, "upstream unreachable")
			return
		}
		defer resp.Body.Close()

		logger.Debug("upstream response",
			"path", req.URL.Path,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
		)

		// 仅对 200 + SSE 响应做 peek 拦截；其余原样透传。
		if resp.StatusCode == http.StatusOK && isSSEResponse(resp) {
			handleSSEResponse(w, resp, rules, logger, req, reqBody, logDir)
			return
		}
		copyResponse(w, resp)
	}
}

// apply1MAuth 用 1M 固定 token 覆盖出站请求的鉴权头。
// 讯飞 1M(glm-5.2) 上游两协议端点统一用 Authorization: Bearer <token>。
func apply1MAuth(outReq *http.Request, token string) {
	outReq.Header.Set("Authorization", "Bearer "+token)
}

// handleSSEResponse 处理 200 + SSE 响应：peek 首帧，命中规则则拦截，否则放行续传。
func handleSSEResponse(w http.ResponseWriter, resp *http.Response, rules []Rule, logger *slog.Logger, req *http.Request, reqBody []byte, logDir string) {
	br := bufio.NewReader(resp.Body)
	peeked, err := readFirstEvent(br)

	m, isErr := parseErrorEvent(peeked)
	logger.Debug("sse first event peeked",
		"path", req.URL.Path,
		"bytes", len(peeked),
		"is_error_frame", isErr,
		"code", m.Code,
	)

	// 解析首帧是 error 帧时尝试匹配规则。
	if isErr {
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
				"request_id", req.Header.Get("X-Request-Id"),
			)
			// 客服反馈的"孤立 tool 消息"类错误（10012 EngineInternalError:Bad Request）：
			// 转储完整请求体到独立文件，便于事后排查 messages 异常。结果仍照常透传。
			if is10012BadRequest(m) {
				dumpRequestBody(logDir, dumpFileName(req, time.Now()), reqBody, logger)
			}
		}
	}

	// 读取异常时仅记日志，仍尽力放行已读字节（保守不吞流）。
	if err != nil && err != io.EOF {
		logger.Warn("peek first sse event ended with error, passing through", "err", err.Error(), "path", req.URL.Path)
	}
	forwardStream(w, resp, peeked, br)
}

// buildUpstreamRequest 构造转发到上游的请求：
// 复制入站 header/body，URL 用 target 的 scheme/host + 重写后的 path + 原始 query，清空 RequestURI。
func buildUpstreamRequest(req *http.Request, target *url.URL, preserveHost bool) *http.Request {
	outReq := req.Clone(req.Context())
	outReq.URL = &url.URL{
		Scheme:   target.Scheme,
		Host:     target.Host,
		Path:     routing.RewritePath(target, req.URL.Path),
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
// 同时支持两种协议格式：
//
//	OpenAI:    data: {"error":{"code":10012,"message":"..."}}
//	Anthropic: event: error \n data: {"type":"error","error":{"type":"overloaded_error","message":"..."}}
//
// 返回 (Match, true) 若是 error 帧；否则 (Match{}, false)。
func parseErrorEvent(event []byte) (Match, bool) {
	var eventType string
	for _, raw := range bytes.Split(event, []byte("\n")) {
		line := bytes.TrimSpace(raw)
		// 提取 event: 行（Anthropic 协议用 event: error 标识错误帧）。
		if bytes.HasPrefix(line, []byte("event:")) {
			eventType = strings.TrimSpace(string(bytes.TrimPrefix(line, []byte("event:"))))
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		m, ok := parseErrorPayload(payload, eventType)
		if ok {
			return m, true
		}
	}
	return Match{}, false
}

// parseErrorPayload 从 data 行的 JSON payload 中解析 error。
// eventType 为 SSE event: 行的值（Anthropic 协议为 "error"，OpenAI 协议为空）。
func parseErrorPayload(payload []byte, eventType string) (Match, bool) {
	// Anthropic 格式：{"type":"error","error":{"type":"overloaded_error","message":"..."}}
	var anthropic struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &anthropic); err == nil {
		if anthropic.Type == "error" && (anthropic.Error.Type != "" || anthropic.Error.Message != "") {
			m := Match{
				ErrorType: anthropic.Error.Type,
				Message:   anthropic.Error.Message,
				Raw:       payload,
			}
			// error.code 可能是数字或字符串。
			switch v := anthropic.Error.Code.(type) {
			case float64:
				m.Code = int(v)
			case string:
				// 字符串 code 不填 Match.Code，但 ErrorType 已有值可匹配。
			}
			// 讯飞 Anthropic 路径常把真实 code 藏在 message 字符串里
			// （如 "...code: 10012, msg: ..."），error 对象无结构化 code 字段。
			// 此时从 message 回填 code，使现有按 code 匹配的规则能命中。
			if m.Code == 0 {
				m.Code = extractCodeFromMessage(m.Message)
			}
			return m, true
		}
	}

	// OpenAI 格式：{"error":{"code":10012,"message":"..."}}
	var openai struct {
		Error struct {
			Code    json.RawMessage `json:"code"`
			Message string          `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(payload, &openai); err != nil {
		return Match{}, false
	}
	if len(openai.Error.Code) == 0 && openai.Error.Message == "" {
		return Match{}, false
	}
	m := Match{Message: openai.Error.Message, Raw: payload}
	var codeInt int
	if json.Unmarshal(openai.Error.Code, &codeInt) == nil {
		m.Code = codeInt
	}
	return m, true
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

// forwardStream 放行 SSE 流：写响应头 + 已 peek 字节 + 剩余 body，逐帧 flush。
// 已 peek 的字节先写出再续传剩余，保证透传字节级完整。
//
// 关键：剩余 body 必须逐块读 + 每次立即 flush，不能用 io.Copy——
// io.Copy 用 32KB 缓冲且不 flush，< 32KB 的响应会缓冲到 EOF 才下发，
// 破坏 SSE 逐帧流式（下游要等所有内容到达才收到）。
func forwardStream(w http.ResponseWriter, resp *http.Response, peeked []byte, br *bufio.Reader) {
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	if len(peeked) > 0 {
		_, _ = w.Write(peeked)
		flush()
	}
	// 逐块读取剩余 body，每块立即 flush，保证 SSE 逐帧下发。
	buf := make([]byte, 4096)
	for {
		n, err := br.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			flush()
		}
		if err != nil {
			break
		}
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

// extractCodeFromMessage 从 message 字符串里提取 "code: NNNN" 的数字部分。
// 讯飞 Anthropic 路径把真实错误码嵌在文案里（error 对象无结构化 code 字段），
// 此处回填以便按 code 匹配的规则能命中。未匹配返回 0。
func extractCodeFromMessage(msg string) int {
	m := codeInMessage.FindStringSubmatch(msg)
	if len(m) < 2 {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return n
}

// is10012BadRequest 判断是否为客服反馈的"孤立 tool 消息"类错误：
// code==10012 且 message 同时含 "EngineInternalError" 与 "Bad Request"。
// （1105 子类型已被 DefaultRules 拦截不进 !matched；此处精确匹配 Bad Request 子类型。）
func is10012BadRequest(m Match) bool {
	return m.Code == 10012 &&
		strings.Contains(m.Message, "EngineInternalError") &&
		strings.Contains(m.Message, "Bad Request")
}

// dumpFileName 决定转储文件名（不含扩展名）：
// 优先用客户端传入的 X-Request-Id（filepath.Base 防路径穿越）；否则用毫秒精度时间戳。
// 不修改下游请求、不注入任何 header——仅读取已存在的 X-Request-Id。
func dumpFileName(req *http.Request, now time.Time) string {
	if id := strings.TrimSpace(req.Header.Get("X-Request-Id")); id != "" {
		return filepath.Base(id) // 不可信输入 → 仅取文件名部分，防穿越
	}
	return now.UTC().Format("20060102T150405.000Z") // 毫秒精度，文件系统安全，字典序=时间序
}

// dumpRequestBody 把请求体完整写入 LOG_DIR/requests/<name>.json，不截断。
// logDir 为空（未配置日志目录）则跳过；失败仅记日志，不影响透传。
func dumpRequestBody(logDir, name string, body []byte, logger *slog.Logger) {
	if logDir == "" || name == "" || len(body) == 0 {
		return
	}
	dir := filepath.Join(logDir, "requests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logger.Warn("dump request body: mkdir failed", "err", err.Error())
		return
	}
	path := filepath.Join(dir, name+".json")
	// 防覆盖：文件已存在时追加 -N（N 从 2 递增）直到找到不存在的文件名。
	// ponytail: stat+write 间存在 TOCTOU 竞态，诊断文件并发概率极低，可接受。
	for i := 2; ; i++ {
		if _, err := os.Stat(path); err == nil {
			path = filepath.Join(dir, name+"-"+strconv.Itoa(i)+".json")
			continue
		}
		break
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		logger.Warn("dump request body: write failed", "err", err.Error(), "file", path)
		return
	}
	logger.Info("dumped request body for unmatched 10012", "file", path, "bytes", len(body))
}
