// Package server 把豆包 /chat/completion 包装成 OpenAI 兼容接口。
package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
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
	for _, k := range cfg.APIKeys {
		sum := sha256.Sum256([]byte(k))
		s.acl = append(s.acl, sum[:])
	}
	s.mux.HandleFunc("POST /v1/chat/completions", s.withAuth(s.chatCompletions))
	s.mux.HandleFunc("GET /v1/models", s.withAuth(s.models))
	s.mux.HandleFunc("GET /healthz", s.healthz)
	s.mux.HandleFunc("GET /status", s.withAuth(s.status))
	// 面板用的账号管理接口
	s.mux.HandleFunc("GET /admin/accounts", s.withAuth(s.adminListAccounts))
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
		if len(s.acl) == 0 {
			next(w, r)
			return
		}
		token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		sum := sha256.Sum256([]byte(token))
		for _, want := range s.acl {
			if subtle.ConstantTimeCompare(sum[:], want) == 1 {
				next(w, r)
				return
			}
		}
		writeErr(w, http.StatusUnauthorized, "invalid api key", "invalid_api_key", "invalid_request_error")
	}
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
		data = append(data, map[string]any{"id": m, "object": "model", "created": s.upAt.Unix(), "owned_by": "doubao"})
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
	convKey := conversationKey(&req, raw)
	st := s.getConv(convKey)

	body, err := doubao.BuildBody(&req, modelKey, doubao.BuildOptions{
		BotID: s.cfg.WorkMode.BotID, ModeID: s.cfg.WorkMode.ModeID,
		ReasoningEffort: s.cfg.WorkMode.ReasoningEffort,
		MemoryProfile:   s.cfg.WorkMode.MemoryProfile,
		TotalWindow:     s.cfg.WorkMode.TotalWindow,
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
		return s.streamOut(w, r, resp.Body, convKey, acct.Name, started)
	}
	return s.wholeOut(w, resp.Body, convKey, req, acct.Name, started)
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
func (s *Server) streamOut(w http.ResponseWriter, r *http.Request, body io.Reader, convKey, acct string, started time.Time) (int, bool) {
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
func (s *Server) wholeOut(w http.ResponseWriter, body io.Reader, convKey string, req *doubao.ChatRequest, acct string, started time.Time) (int, bool) {
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
