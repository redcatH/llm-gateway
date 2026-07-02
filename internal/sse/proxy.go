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

// ProxyHandler 返回一个处理所有请求的 http.Handler。
//
// 对上游响应为 200 + SSE 的请求做首帧 peek：
//   - 首帧命中拦截规则 → 按 Handler 决策响应（如 503），丢弃原流；
//   - 否则（非命中 / 非 error / 非 SSE / 非 200）→ 原样透传，字节级一致。
//
// 分流依据是上游响应（resp.StatusCode==200 且 Content-Type 含 text/event-stream），
// 而非请求的 Accept 头，更可靠。
func ProxyHandler(openAITarget, anthropicTarget *url.URL, preserveHost bool, transport http.RoundTripper, rules []Rule, logger *slog.Logger, logDir string, rc RewriteConfig) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// 按请求协议（路径）选上游：/v1/messages → Anthropic，其余 → OpenAI。
		target := routing.SelectTarget(req, openAITarget, anthropicTarget)
		logger.Debug("incoming request",
			"method", req.Method,
			"path", req.URL.Path,
			"target_host", target.Host,
			"accept", req.Header.Get("Accept"),
		)

		// 缓存请求体用于 error 诊断；重置 req.Body 为可重读 reader，保证转发不受影响。
		// 必须在 RoundTrip 前：req.Clone() 浅拷贝 Body，RoundTrip 会消费掉原始 reader。
		var reqBody []byte
		if req.Body != nil {
			reqBody, _ = io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(reqBody))
		}

		outReq := buildUpstreamRequest(req, target, preserveHost)

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
			handleSSEResponse(w, resp, rules, logger, req, reqBody, logDir, rc)
			return
		}
		// 非流式 + 5xx → 记录响应体便于事后分析上游异常。
		// 4xx 属客户端错误（如 401/404），不算上游异常，不记。
		// 读全量 body 后重置 resp.Body，不影响后续透传。
		if resp.StatusCode >= 500 {
			logNonSuccessResponse(logger, req, resp)
		}
		// 非流式 2xx + 启用改写 + JSON → 读全量改写 model；否则原样透传。
		// 错误响应（4xx/5xx）与非 JSON body 不改写。
		if rc.enabled() && resp.StatusCode == http.StatusOK && isJSONResponse(resp) {
			copyResponseRewritten(w, resp, rc)
			return
		}
		copyResponse(w, resp)
	})
}

// handleSSEResponse 处理 200 + SSE 响应：peek 首帧，命中规则则拦截，否则放行续传。
func handleSSEResponse(w http.ResponseWriter, resp *http.Response, rules []Rule, logger *slog.Logger, req *http.Request, reqBody []byte, logDir string, rc RewriteConfig) {
	br := bufio.NewReader(resp.Body)
	peeked, err := readEvent(br)

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
	forwardStream(w, resp, peeked, br, rc)
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

// readEvent 从 bufio.Reader 读取一个完整的 SSE 事件（以空行分隔）。
// 返回该事件的原始字节（含事件边界空行）。遇到 EOF 时返回已读字节与 io.EOF。
// 首帧与后续帧共用：handleSSEResponse peek 首帧，forwardStream 逐帧续读。
func readEvent(br *bufio.Reader) ([]byte, error) {
	var buf bytes.Buffer
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			buf.Write(line)
		}
		// EOF/错误必须优先返回，否则空 line 会命中下方"空行=边界"分支，
		// 吞掉 EOF 导致 forwardStream 循环死等下一个事件。
		if err != nil {
			return buf.Bytes(), err
		}
		// 空行（去掉行尾 \r\n 后为空）= 事件边界。
		if len(bytes.TrimRight(line, "\r\n")) == 0 {
			return buf.Bytes(), nil
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

// isJSONResponse 判断上游响应是否为 JSON（非流式 model 改写的准入条件）。
func isJSONResponse(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json")
}

// copyResponseRewritten 读全量 body，改写 model 后写出（非流式 2xx JSON 路径）。
// 改写后 body 长度可能变化，删除 Content-Length 让 Go 自动定界（chunked 或重算）。
// 解析失败 / 无需改时 rewriteModelJSON 原样返回 body，等价透传。
func copyResponseRewritten(w http.ResponseWriter, resp *http.Response, rc RewriteConfig) {
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	out := rewriteModelJSON(body, rc)
	copyHeader(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// logNonSuccessResponse 记录上游 5xx 响应的 body 内容，便于事后分析上游异常。
// 读全量 body 后用 NopCloser 重置 resp.Body，保证后续 copyResponse 透传不受影响。
// body 超过 maxLogBodyBytes 时截断并标注，防止超大响应撑爆日志。
//
// 遵循 logging-guidelines "What NOT to Log" 的 5xx 例外：
// 仅 status>=500 触发、截断上限 8KB、不记认证头。详见 .trellis/spec/backend/logging-guidelines.md。
func logNonSuccessResponse(logger *slog.Logger, req *http.Request, resp *http.Response) {
	const maxLogBodyBytes = 8 * 1024 // 8KB 上限，足够覆盖上游错误体
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	// err 时 body 为已读部分，透传不中断；正常时为完整 body。
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		logger.Error("upstream non-success response (body read failed)",
			"path", req.URL.Path,
			"status", resp.StatusCode,
			"content_type", resp.Header.Get("Content-Type"),
			"read_err", err.Error(),
		)
		return
	}

	logBody := body
	truncated := len(body) > maxLogBodyBytes
	if truncated {
		logBody = body[:maxLogBodyBytes]
	}
	logger.Error("upstream non-success response",
		"path", req.URL.Path,
		"status", resp.StatusCode,
		"content_type", resp.Header.Get("Content-Type"),
		"body", string(logBody),
		"body_bytes", len(body),
		"truncated", truncated,
	)
}

// copyResponse 将上游响应原样透传给下游（status + header + body）。
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	copyHeader(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	copyBodyAndFlush(w, resp.Body)
}

// forwardStream 放行 SSE 流：写响应头 + 已 peek 字节 + 剩余 body，逐帧 flush。
// 启用改写时（rc.enabled），对每个 SSE 事件按需改写 model 后再写出；
// 未启用或事件不含 model 时 rewriteSSEEvent 字节级原样返回。
//
// 关键：剩余 body 必须逐事件读 + 每次立即 flush，不能用 io.Copy——
// io.Copy 用 32KB 缓冲且不 flush，< 32KB 的响应会缓冲到 EOF 才下发，
// 破坏 SSE 逐帧流式（下游要等所有内容到达才收到）。
func forwardStream(w http.ResponseWriter, resp *http.Response, peeked []byte, br *bufio.Reader, rc RewriteConfig) {
	copyHeader(w.Header(), resp.Header)
	// 改写可能改变字节数；流式本无 Content-Length，Del 统一处理（无此头时为 no-op）。
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	flush := func() {
		if flusher != nil {
			flusher.Flush()
		}
	}
	// 首帧（已 peek）：含 model 则改写后写出，否则原样。
	if len(peeked) > 0 {
		_, _ = w.Write(rewriteSSEEvent(peeked, rc))
		flush()
	}
	// 逐事件读取剩余 body，每个事件按需改写后立即 flush，保证 SSE 逐帧下发。
	for {
		event, err := readEvent(br)
		if len(event) > 0 {
			_, _ = w.Write(rewriteSSEEvent(event, rc))
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
