package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"doubao2api/internal/config"
)

// fakeDoubao 复刻实抓的 SSE 序列：SSE_ACK → CHUNK_DELTA×N → SSE_REPLY_END(end_type=1, brief)
type fakeDoubao struct {
	srv  *httptest.Server
	mu   sync.Mutex
	bods []string
}

func newFakeDoubao(t *testing.T) *fakeDoubao {
	f := &fakeDoubao{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bods = append(f.bods, string(body))
		f.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			"event: SSE_HEARTBEAT\ndata: {}\n\n",
			`event: SSE_ACK` + "\n" + `data: {"ack_client_meta":{"conversation_id":"38453576668627970","section_id":"38453576668628226"}}` + "\n\n",
			"event: CHUNK_DELTA\ndata: {\"text\":\"你好！\"}\n\n",
			"event: CHUNK_DELTA\ndata: {\"text\":\"我是豆包。\"}\n\n",
			"event: STREAM_CHUNK\ndata: {\"message_id\":\"1\",\"patch_op\":[{\"patch_object\":1,\"patch_value\":{\"content_block\":[{\"block_type\":10040,\"content\":{\"thinking_block\":{\"streaming_title\":\"正在思考\"}}}]}}]}\n\n",
			`event: SSE_REPLY_END` + "\n" + `data: {"end_type":1,"msg_finish_attr":{"brief":"你好！我是豆包。"}}` + "\n\n",
			"event: SSE_REPLY_END\ndata: {\"end_type\":2}\n\n",
		}
		for _, fr := range frames {
			_, _ = io.WriteString(w, fr)
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func newTestServer(t *testing.T, baseURL string) *Server {
	cfg := map[string]any{
		"listen":    "127.0.0.1:0",
		"api_keys":  []string{"sk-down"},
		"accounts":  []map[string]any{{"name": "a1", "cookie": "sessionid=test; ttwid=test"}, {"name": "a2", "cookie": "sessionid=test2"}},
		"default_model": "doubao",
		"model_aliases": map[string]string{"deepseek": "person-deepseek-v4-1-flash-261017"},
		"work_mode":     map[string]any{"enabled": true, "enable_sandbox": false},
		"upstream": map[string]any{
			"chat_url":          baseURL + "/chat/completion",
			"heartbeat_seconds": 30,
			"timeout_seconds":   30,
		},
		"pool": map[string]any{"deterministic": true, "sticky_ttl": "1m"},
	}
	raw, _ := json.Marshal(cfg)
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatalf("write cfg: %v", err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("load cfg: %v", err)
	}
	return New(c)
}

func post(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-down")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

// 非流式：把 CHUNK_DELTA 拼成完整回答。
func TestNonStream(t *testing.T) {
	f := newFakeDoubao(t)
	s := newTestServer(t, f.srv.URL)

	rec := post(t, s, `{"model":"deepseek","messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse: %v (%s)", err, rec.Body.String())
	}
	if got := out.Choices[0].Message.Content; got != "你好！我是豆包。" {
		t.Fatalf("content = %q", got)
	}
	// 上游必须收到 model_item_key 与工作模式参数
	f.mu.Lock()
	up := f.bods[0]
	f.mu.Unlock()
	for _, want := range []string{
		`"model_item_key":"person-deepseek-v4-1-flash-261017"`,
		`"agent_mode":1`, `"conversation_mode":2`,
		`"support_chunk_delta":true`,
	} {
		if !strings.Contains(up, want) {
			t.Fatalf("上游 body 缺少 %s：%s", want, up[:400])
		}
	}
}

// 流式：逐块输出并以 [DONE] 收尾，且会话 id 被记录。
func TestStreamAndConversationReuse(t *testing.T) {
	f := newFakeDoubao(t)
	s := newTestServer(t, f.srv.URL)

	rec := post(t, s, `{"model":"deepseek","stream":true,"metadata":{"conversation_id":"conv-test-1"},"messages":[{"role":"user","content":"你好"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"content":"你好！"`, `"content":"我是豆包。"`, `"reasoning_content":"正在思考"`, "data: [DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %s\n%s", want, body)
		}
	}

	// 第二轮：应带上服务端返回的 conversation_id
	_ = post(t, s, `{"model":"deepseek","metadata":{"conversation_id":"conv-test-1"},"messages":[{"role":"user","content":"第一条问题"},{"role":"assistant","content":"第一条回答"},{"role":"user","content":"第二条"}]}`)
	f.mu.Lock()
	last := f.bods[len(f.bods)-1]
	f.mu.Unlock()
	if !strings.Contains(last, `"conversation_id":"38453576668627970"`) {
		t.Fatalf("第二轮未复用 conversation_id: %s", last[:300])
	}
	if strings.Contains(last, `"need_create_conversation":true`) {
		t.Fatalf("第二轮不应再建会话")
	}
}

// 鉴权：错误下游密钥 401，且不打上游。
func TestAuth(t *testing.T) {
	f := newFakeDoubao(t)
	s := newTestServer(t, f.srv.URL)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"messages":[]}`))
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code=%d", rec.Code)
	}
	f.mu.Lock()
	n := len(f.bods)
	f.mu.Unlock()
	if n != 0 {
		t.Fatalf("未授权请求不应打上游: %d", n)
	}
}

// 401 换号重试。
func TestRetryOtherAccount(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"login required"}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: CHUNK_DELTA\ndata: {\"text\":\"ok\"}\n\nevent: SSE_REPLY_END\ndata: {\"end_type\":1}\n\n")
	}))
	defer srv.Close()
	s := newTestServer(t, srv.URL)

	rec := post(t, s, `{"model":"doubao","messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if calls < 2 {
		t.Fatalf("应换号重试，calls=%d", calls)
	}
}
