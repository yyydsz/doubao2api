// Package server 把豆包 /chat/completion 包装成 OpenAI 兼容接口。
package server

import (
	"bytes"
	"context"
	"net"
	"crypto/subtle"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"

	"doubao2api/internal/config"
	"doubao2api/internal/doubao"
	"doubao2api/internal/pool"
)

const maxBodyBytes = 32 << 20

type convState struct {
	ConversationID string
	SectionID      string
	at             time.Time
}

// Server 网关主体。
type Server struct {
	cfg  *config.Config
	pool *pool.Pool
	mux  *http.ServeMux
	acl  [][]byte
	http *http.Client
	accs map[string]config.Account

	mu    sync.Mutex
	convs map[string]convState
	upAt  time.Time
}

// New 构造服务。
func New(cfg *config.Config) *Server {
	s := &Server{
		cfg:  cfg,
		pool: pool.New(cfg.PoolOptions()),
		mux:  http.NewServeMux(),
		accs: map[string]config.Account{},
		convs: map[string]convState{},
		upAt: time.Now(),
		http: &http.Client{
			Timeout: time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second,
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        50,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
				ForceAttemptHTTP2:   true,
			},
		},
	}
	for _, a := range cfg.Accounts {
		s.accs[a.Name] = a
		s.pool.Add(pool.Key{Name: a.Name, Key: a.Name})
	}
	s.loadKeys()
	s.mux.HandleFunc("POST /v1/chat/completions", s.withAuth(s.chatCompletions))
	s.mux.HandleFunc("GET /v1/models", s.withAuth(s.models))
	s.loadAccountsFile() // 启动时加载 accounts.json（面板写入的账号）
	go s.checkinLoop()   // 每日自动签到
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /status", s.withAuth(s.status))
	// 面板用的账号管理接口
	s.mux.HandleFunc("GET /admin/accounts", s.withAuth(s.adminListAccounts))
	s.mux.HandleFunc("GET /admin/quota", s.withAuth(s.adminQuota))
	s.mux.HandleFunc("GET /admin/models", s.withAuth(s.adminModels))
	s.mux.HandleFunc("GET /admin/status", s.withAuth(s.status))
	s.mux.HandleFunc("POST /admin/checkin", s.withAuth(s.adminCheckin))
	s.mux.HandleFunc("GET /admin/checkin", s.withAuth(s.adminCheckinStatus))
	s.mux.HandleFunc("GET /admin/keys", s.withAuth(s.adminListKeys))
	s.mux.HandleFunc("POST /admin/keys", s.withAuth(s.adminCreateKey))
	s.mux.HandleFunc("DELETE /admin/keys", s.withAuth(s.adminDeleteKey))
	s.mux.HandleFunc("GET /admin/usage", s.withAuth(s.adminUsage))
	s.mux.HandleFunc("POST /admin/reload", s.withAuth(s.adminReload))
	s.mux.HandleFunc("POST /admin/cookie", s.withAuth(s.adminSetCookie))
	s.mux.HandleFunc("DELETE /admin/accounts", s.withAuth(s.adminDelete))
	s.mux.HandleFunc("POST /admin/accounts/qr", s.withAuth(s.qrStart))
	s.mux.HandleFunc("GET /admin/accounts/qr", s.withAuth(s.qrPoll))
	return s
}

// ServeHTTP 实现 http.Handler。
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// 1) 管理接口：面板与网关同机，只允许回环地址访问（人到面板有 Basic Auth）
		if strings.HasPrefix(r.URL.Path, "/admin/") {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err == nil {
				ip := net.ParseIP(host)
				if ip != nil && ip.IsLoopback() {
					next(w, r)
					return
				}
			}
			// 远程管理：允许显式配置的 admin token（可选）
			tok := bearerToken(r)
			if s.cfg.AdminToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.AdminToken)) == 1 {
				next(w, r)
				return
			}
			writeErr(w, http.StatusForbidden, "管理接口仅限本机访问", "admin_local_only", "permission_error")
			return
		}

		// 2) 业务接口：校验分发密钥
		tok := bearerToken(r)
		if k, ok := lookupKey(tok); ok {
			next(w, r.WithContext(context.WithValue(r.Context(), ctxKeyID, k.ID)))
			return
		}
		keyMu.RLock()
		empty := len(keysDB) == 0
		keyMu.RUnlock()
		if empty {
			writeErr(w, http.StatusUnauthorized, "尚未创建任何密钥，请在管理面板「密钥」页生成", "no_keys", "invalid_request_error")
			return
		}
		writeErr(w, http.StatusUnauthorized, "invalid api key", "invalid_api_key", "invalid_request_error")
	}
}

// bearerToken 从 Authorization / x-api-key 取令牌。
func bearerToken(r *http.Request) string {
	t := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if t == "" {
		t = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	return t
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": s.pool.Len(), "healthy": s.pool.Healthy(),
		"uptime": int(time.Since(s.upAt).Seconds()),
	})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": s.pool.Stats(), "healthy": s.pool.Healthy(), "total": s.pool.Len(),
	})
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	list := s.cfg.ModelList()
	data := make([]map[string]any, 0, len(list))
	for _, m := range list {
		item := map[string]any{"id": m, "object": "model", "created": s.upAt.Unix(), "owned_by": "doubao"}
		// 带上上下文窗口：优先 model_params，其次全局 work_mode
		win := s.cfg.WorkMode.TotalWindow
		if mp, ok := s.cfg.ModelParams[m]; ok && mp.TotalWindowSize != "" {
			win = mp.TotalWindowSize
		}
		if n, err := parseInt(win); err == nil && n > 0 {
			item["context_length"] = n
			item["max_tokens"] = n
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// chatCompletions 主入口。
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取请求体失败: "+err.Error(), "bad_body", "invalid_request_error")
		return
	}
	var req doubao.ChatRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体不是合法 JSON: "+err.Error(), "bad_json", "invalid_request_error")
		return
	}

	modelKey := s.cfg.ResolveModel(req.Model)
	effortOverride := strings.TrimSpace(req.ReasoningEffort)
	// 不同模型上下文窗口不同：优先用 model_params，其次全局 work_mode
	memProfile, winSize := s.cfg.WorkMode.MemoryProfile, s.cfg.WorkMode.TotalWindow
	if mp, ok := s.modelParamFor(req.Model, modelKey); ok {
		if mp.MemoryProfile != "" {
			memProfile = mp.MemoryProfile
		}
		if mp.TotalWindowSize != "" {
			winSize = mp.TotalWindowSize
		}
	}
	convKey := conversationKey(&req, raw)
	st := s.getConv(convKey)

	body, err := doubao.BuildBody(&req, modelKey, doubao.BuildOptions{
		BotID: s.cfg.WorkMode.BotID, ModeID: s.cfg.WorkMode.ModeID,
		ReasoningEffort: effortOr(effortOverride, s.cfg.WorkMode.ReasoningEffort),
		MemoryProfile:   memProfile,
		TotalWindow:     winSize,
		EnableSandbox:   s.cfg.WorkMode.EnableSandbox,
		Workspace:       s.cfg.WorkMode.Workspace,
		DeviceName:      hostname(),
		WorkMode:        s.cfg.WorkMode.Enabled,
		ConversationID:  st.ConversationID,
		Flatten:         s.cfg.Upstream.FlattenHistory,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error(), "bad_request", "invalid_request_error")
		return
	}

	large := s.cfg.Pool.LargeRequestBytes > 0 && len(body) >= s.cfg.Pool.LargeRequestBytes
	maxAttempts := s.pool.Len()
	if maxAttempts > 3 {
		maxAttempts = 3
	}
	tried := map[string]bool{}
	lastMsg := ""

	for i := 0; i < maxAttempts; i++ {
		h := s.pickKey(convKey, large, tried)
		if h == nil {
			break
		}
		name := h.Name()
		tried[name] = true
		if large {
			s.pool.NoteLarge(name)
		}
		acct, ok := s.accs[name]
		if !ok {
			s.pool.NoteAuthFailure(name, "account missing")
			continue
		}

		status, retry := s.forward(w, r, &req, body, acct, convKey, started)
		if status == 0 && retry {
			continue
		}
		if status == http.StatusOK {
			s.pool.NoteSuccess(name)
			s.pool.BindSticky(convKey, name)
			return
		}
		if retry {
			continue
		}
		return
	}

	if lastMsg == "" {
		lastMsg = "所有豆包账号均不可用"
	}
	logf("failed model=%s tried=%d msg=%s", modelKey, len(tried), lastMsg)
	writeErr(w, http.StatusServiceUnavailable, lastMsg, "no_available_account", "api_error")
}

// forward 单次转发；返回 (httpStatus, 是否可换号重试)。
func (s *Server) forward(w http.ResponseWriter, r *http.Request, req *doubao.ChatRequest,
	body []byte, acct config.Account, convKey string, started time.Time) (int, bool) {

	target, err := s.buildURL(acct)
	if err != nil {
		logf("build_url_error acct=%s err=%v", acct.Name, err)
		return 0, true
	}
	hreq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, true
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "*/*")
	hreq.Header.Set("agw-js-conv", "str, str")
	hreq.Header.Set("last-event-id", "undefined")
	hreq.Header.Set("x-flow-trace", "04-0000000000000000000000000000000-0000000000000000-01")
	hreq.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	hreq.Header.Set("Referer", "https://www.doubao.com/")
	hreq.Header.Set("Cookie", acct.Cookie)

	resp, err := s.http.Do(hreq)
	if err != nil {
		logf("net_error acct=%s err=%v", acct.Name, err)
		s.pool.NoteOverload(acct.Name)
		return 0, true
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(raw))
		logf("upstream_error acct=%s status=%d body=%s", acct.Name, resp.StatusCode, truncate(msg, 200))
		switch {
		case resp.StatusCode == 401 || resp.StatusCode == 403:
			s.pool.NoteAuthFailure(acct.Name, msg)
			return 0, true
		case resp.StatusCode == 429:
			s.pool.NoteRateLimit(acct.Name, parseRetryAfter(resp.Header.Get("Retry-After")))
			return 0, true
		case resp.StatusCode >= 500:
			s.pool.NoteOverload(acct.Name)
			return 0, true
		}
		writeErr(w, resp.StatusCode, msg, "upstream_error", "api_error")
		return resp.StatusCode, false
	}

	if req.Stream {
		return s.streamOut(w, r, resp.Body, convKey, acct.Name, req.Model, started)
	}
	return s.wholeOut(w, r, resp.Body, convKey, req, acct.Name, started)
}

// sink 把 SSE 事件转发到 channel。
type sink struct {
	ch     chan ev
	final  strings.Builder
	reason strings.Builder
}

type ev struct {
	kind    int
	text    string
	conv    string
	section string
	endType int
	brief   string
}

const (evText = iota + 1; evReason; evConv; evFinish)

func (s *sink) OnText(t string)  { s.final.WriteString(t); s.ch <- ev{kind: evText, text: t} }
func (s *sink) OnReasoning(t string) { s.reason.WriteString(t); s.ch <- ev{kind: evReason, text: t} }
func (s *sink) OnConversation(c, sec string) { s.ch <- ev{kind: evConv, conv: c, section: sec} }
func (s *sink) OnFinish(endType int, brief string) { s.ch <- ev{kind: evFinish, endType: endType, brief: brief} }

// streamOut 流式输出。
func (s *Server) streamOut(w http.ResponseWriter, r *http.Request, body io.Reader, convKey, acct, model string, started time.Time) (int, bool) {
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	w.WriteHeader(http.StatusOK)

	sk := &sink{ch: make(chan ev, 64)}
	go func() {
		defer close(sk.ch)
		if err := doubao.Pump(body, sk); err != nil && err != io.EOF {
			logf("pump_error acct=%s err=%v", acct, err)
		}
	}()

	id := "chatcmpl-" + shortHash(convKey)
	created := time.Now().Unix()
	hb := time.Duration(s.cfg.Upstream.HeartbeatSeconds) * time.Second
	ticker := time.NewTicker(hb)
	defer ticker.Stop()
	wrote := false
	finishReason := "stop"

	for {
		select {
		case <-r.Context().Done():
			logf("client_gone acct=%s elapsed=%s", acct, time.Since(started).Round(time.Millisecond))
			return 0, false
		case <-ticker.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return 0, false
			}
			if fl != nil {
				fl.Flush()
			}
		case e, ok := <-sk.ch:
			if !ok {
				// 收尾
				if !wrote && sk.final.Len() == 0 {
					writeChunk(w, fl, id, created, map[string]any{"content": "(空响应)"}, nil)
				}
				writeChunk(w, fl, id, created, map[string]any{}, &finishReason)
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				if fl != nil {
					fl.Flush()
				}
				logf("ok acct=%s stream=true latency=%s chars=%d", acct, time.Since(started).Round(time.Millisecond), sk.final.Len())
				s.recordUsage(apiKeyFrom(r.Context()), model, sk.final.Len(), http.StatusOK, int(time.Since(started)/time.Millisecond))
				return http.StatusOK, false
			}
			switch e.kind {
			case evText:
				wrote = true
				writeChunk(w, fl, id, created, map[string]any{"content": e.text}, nil)
			case evReason:
				writeChunk(w, fl, id, created, map[string]any{"reasoning_content": e.text}, nil)
			case evConv:
				s.setConv(convKey, e.conv, e.section)
			case evFinish:
				if e.endType != 1 && sk.final.Len() == 0 && e.brief != "" {
					wrote = true
					writeChunk(w, fl, id, created, map[string]any{"content": e.brief}, nil)
				}
				if e.endType != 1 && sk.final.Len() == 0 {
					finishReason = "stop"
				}
			}
		}
	}
}

// wholeOut 非流式输出。
func (s *Server) wholeOut(w http.ResponseWriter, r *http.Request, body io.Reader, convKey string, req *doubao.ChatRequest, acct string, started time.Time) (int, bool) {
	sk := &sink{ch: make(chan ev, 256)}
	done := make(chan struct{})
	go func() {
		for range sk.ch { /* 丢弃，只累积 */
		}
		close(done)
	}()
	err := doubao.Pump(body, sk)
	close(sk.ch)
	<-done
	if err != nil && err != io.EOF {
		writeErr(w, http.StatusBadGateway, "上游流读取失败: "+err.Error(), "upstream_stream_error", "api_error")
		return http.StatusBadGateway, false
	}
	text := sk.final.String()
	if text == "" {
		writeErr(w, http.StatusBadGateway, "上游没有返回内容", "empty_response", "api_error")
		return http.StatusBadGateway, false
	}
	resp := map[string]any{
		"id":      "chatcmpl-" + shortHash(convKey),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": text},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
		},
	}
	writeJSON(w, http.StatusOK, resp)
	logf("ok acct=%s stream=false latency=%s chars=%d", acct, time.Since(started).Round(time.Millisecond), len(text))
	s.recordUsage(apiKeyFrom(r.Context()), req.Model, len(text), http.StatusOK, int(time.Since(started)/time.Millisecond))
	return http.StatusOK, false
}

// buildURL 拼上游 URL（基础参数 + 账号参数 + 签名）。
func (s *Server) buildURL(acct config.Account) (string, error) {
	u, err := url.Parse(s.cfg.Upstream.ChatURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	for k, v := range s.cfg.Upstream.Query {
		if _, exists := q[k]; !exists {
			q.Set(k, v)
		}
	}
	dev := acct.DeviceID
	if dev == "" {
		dev = shortHash(acct.Cookie)
	}
	q.Set("device_id", dev)
	q.Set("fp", "verify_"+dev)
	q.Set("tea_uuid", dev)
	if acct.WebID != "" {
		q.Set("web_id", acct.WebID)
	}
	q.Set("web_tab_id", newUUID())
	u.RawQuery = q.Encode()

	if cmd := s.cfg.Upstream.SignCommand; cmd != "" {
		sig, err := runSign(cmd, u.String())
		if err != nil {
			logf("sign_error err=%v", err)
		} else if sig != "" {
			qq := u.Query()
			qq.Set("a_bogus", sig)
			u.RawQuery = qq.Encode()
		}
	} else if s.cfg.Upstream.ABogus != "" {
		qq := u.Query()
		qq.Set("a_bogus", s.cfg.Upstream.ABogus)
		u.RawQuery = qq.Encode()
	}
	return u.String(), nil
}

// pickKey 粘性优先 + 池策略。
func (s *Server) pickKey(conv string, large bool, tried map[string]bool) *pool.Handle {
	if name, ok := s.pool.ResolveSticky(conv); ok && !tried[name] {
		if h := s.pool.PickBySticky(name, large); h != nil {
			return h
		}
		s.pool.UnbindSticky(conv)
	}
	return s.pool.Pick(tried, large)
}

// 会话状态
func (s *Server) getConv(k string) convState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.convs[k]
}

func (s *Server) setConv(k, conv, section string) {
	if k == "" || conv == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.convs[k] = convState{ConversationID: conv, SectionID: section, at: time.Now()}
	if len(s.convs) > 5000 {
		for key, v := range s.convs {
			if time.Since(v.at) > time.Hour {
				delete(s.convs, key)
			}
		}
	}
}

// ---------- 输出辅助 ----------

func writeChunk(w http.ResponseWriter, fl http.Flusher, id string, created int64, delta map[string]any, finish *string) {
	chunk := map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	b, _ := json.Marshal(chunk)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
	if fl != nil {
		fl.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg, code, typ string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code}})
}

func logf(format string, args ...any) { log.Printf(format, args...) }

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:6])
}

func newUUID() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(time.Now().UnixNano() >> (i * 3))
	}
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func hostname() string {
	h, err := exec.LookPath("hostname")
	_ = h
	if err != nil {
		return "USER"
	}
	return "USER"
}

func runSign(cmd, target string) (string, error) {
	c := exec.Command("sh", "-c", cmd)
	c.Stdin = strings.NewReader(target)
	out, err := c.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if d, err := time.ParseDuration(v + "s"); err == nil {
		return d
	}
	return 0
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// conversationKey 会话键：metadata.conversation_id → user → 首条消息哈希。
func conversationKey(req *doubao.ChatRequest, raw []byte) string {
	if req.Metadata != nil {
		for _, k := range []string{"conversation_id", "conversationId", "session_id"} {
			if v, ok := req.Metadata[k].(string); ok && v != "" {
				return "c:" + v
			}
		}
	}
	if req.User != "" {
		return "u:" + req.User
	}
	for _, m := range req.Messages {
		if m.Role == "user" {
			sum := sha256.Sum256([]byte(m.Text()))
			return "h:" + hex.EncodeToString(sum[:12])
		}
	}
	sum := sha256.Sum256(raw)
	return "r:" + hex.EncodeToString(sum[:12])
}

// effortOr 请求级 reasoning_effort 优先，缺省用配置值。
func effortOr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// modelParamFor 查某模型的上下文参数（先按请求名，再按 model_item_key）。
func (s *Server) modelParamFor(modelName, modelKey string) (config.ModelParam, bool) {
	if s.cfg.ModelParams == nil {
		return config.ModelParam{}, false
	}
	if p, ok := s.cfg.ModelParams[modelName]; ok {
		return p, true
	}
	if p, ok := s.cfg.ModelParams[modelKey]; ok {
		return p, true
	}
	return config.ModelParam{}, false
}

// adminModels 模型清单（管理面板用）：含豆包 key、item_id、上下文、倍率。
func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	list := s.cfg.ModelList()
	out := make([]map[string]any, 0, len(list))
	for _, name := range list {
		p := s.cfg.ModelParams[name]
		win := s.cfg.WorkMode.TotalWindow
		if p.TotalWindowSize != "" {
			win = p.TotalWindowSize
		}
		enabled := true
		if p.Enabled != nil {
			enabled = *p.Enabled
		}
		item := map[string]any{
			"id":               name,
			"model_item_key":   s.cfg.ResolveModel(name),
			"item_id":          p.ItemID,
			"memory_profile":   p.MemoryProfile,
			"context_length":   atoiSafe(win),
			"ratio":            p.Ratio,
			"display":          firstNonEmpty(p.Display, name),
			"enabled":          enabled,
			"is_default":       name == s.cfg.DefaultModel,
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "total": len(out),
		"default_model": s.cfg.DefaultModel})
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func atoiSafe(s string) int {
	n, err := parseInt(s)
	if err != nil {
		return 0
	}
	return n
}
