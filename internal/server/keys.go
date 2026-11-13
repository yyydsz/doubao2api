package server

import (
	"bufio"
	"context"
	"crypto/subtle"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// APIKey 下游密钥（面板管理，网关只认这里的 key）。
type APIKey struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Key     string `json:"key"`
	Created int64  `json:"created"`
	Enabled bool   `json:"enabled"`
}

var (
	keyMu  sync.RWMutex
	keysDB []APIKey
	usageMu sync.Mutex
)

type ctxKeyType string

const ctxKeyID ctxKeyType = "apikey_id"

// keysPath 密钥文件路径。
func (s *Server) keysPath() string {
	if p := os.Getenv("DOUBAO2API_KEYS"); p != "" {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return "keys.json"
	}
	return filepath.Join(filepath.Dir(exe), "keys.json")
}

// usagePath 用量日志路径（JSONL，一行一条）。
func (s *Server) usagePath() string {
	if p := os.Getenv("DOUBAO2API_USAGE"); p != "" {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return "usage.jsonl"
	}
	return filepath.Join(filepath.Dir(exe), "usage.jsonl")
}

// loadKeys 启动时加载密钥；文件不存在则用 config.api_keys 初始化一个默认密钥。
func (s *Server) loadKeys() {
	raw, err := os.ReadFile(s.keysPath())
	if err == nil {
		var list []APIKey
		if json.Unmarshal(raw, &list) == nil && len(list) > 0 {
			keyMu.Lock()
			keysDB = list
			keyMu.Unlock()
			logf("keys loaded=%d", len(list))
			return
		}
	}
	// 兜底：config.api_keys → 默认密钥
	keyMu.Lock()
	keysDB = nil
	for i, k := range s.cfg.APIKeys {
		keysDB = append(keysDB, APIKey{
			ID: "k" + itoaSimple(i+1), Name: "default-" + itoaSimple(i+1),
			Key: k, Created: time.Now().Unix(), Enabled: true,
		})
	}
	keyMu.Unlock()
	if len(keysDB) > 0 {
		s.saveKeys()
		logf("keys seeded from config=%d", len(keysDB))
	}
}

// saveKeys 落盘。
func (s *Server) saveKeys() {
	keyMu.RLock()
	out, _ := json.MarshalIndent(keysDB, "", "  ")
	keyMu.RUnlock()
	_ = os.WriteFile(s.keysPath(), out, 0o600)
}

// lookupKey 用常量时间比较匹配密钥，返回命中的 key。
func lookupKey(token string) (APIKey, bool) {
	sum := sha256.Sum256([]byte(token))
	keyMu.RLock()
	defer keyMu.RUnlock()
	for _, k := range keysDB {
		if !k.Enabled || k.Key == "" {
			continue
		}
		ks := sha256.Sum256([]byte(k.Key))
		if subtle.ConstantTimeCompare(sum[:], ks[:]) == 1 {
			return k, true
		}
	}
	return APIKey{}, false
}

// apiKeyFrom 从请求上下文取当前密钥。
func apiKeyFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyID).(string); ok {
		return v
	}
	return ""
}

// recordUsage 追加一条用量记录（JSONL）。
func (s *Server) recordUsage(keyID, model string, chars int, status int, latencyMs int) {
	rec := map[string]any{
		"ts": time.Now().Unix(), "key": keyID, "model": model,
		"chars": chars, "tokens": int(float64(chars) / 1.7),
		"status": status, "latency_ms": latencyMs,
	}
	b, _ := json.Marshal(rec)
	usageMu.Lock()
	f, err := os.OpenFile(s.usagePath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.Write(append(b, '\n'))
		f.Close()
	}
	usageMu.Unlock()
}

// ---------- 管理接口 ----------

// adminListKeys 列出密钥（key 打码）。
func (s *Server) adminListKeys(w http.ResponseWriter, r *http.Request) {
	keyMu.RLock()
	out := make([]map[string]any, 0, len(keysDB))
	for _, k := range keysDB {
		out = append(out, map[string]any{
			"id": k.ID, "name": k.Name, "key_masked": maskKey(k.Key),
			"key": k.Key, "created": k.Created, "enabled": k.Enabled,
		})
	}
	keyMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{"keys": out, "total": len(out)})
}

// adminCreateKey 新建密钥；name 为空则自动命名。
func (s *Server) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.TrimSpace(body.Name) == "" {
		body.Name = "key-" + randomHex(3)
	}
	k := APIKey{
		ID: "k" + randomHex(4), Name: body.Name,
		Key: "sk-" + randomHex(16), Created: time.Now().Unix(), Enabled: true,
	}
	keyMu.Lock()
	keysDB = append(keysDB, k)
	keyMu.Unlock()
	s.saveKeys()
	logf("key_created id=%s name=%s", k.ID, k.Name)
	writeJSON(w, http.StatusOK, map[string]any{"key": k})
}

// adminDeleteKey 删除密钥。
func (s *Server) adminDeleteKey(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, "缺少 id", "bad_request", "invalid_request_error")
		return
	}
	keyMu.Lock()
	out := keysDB[:0]
	removed := false
	for _, k := range keysDB {
		if k.ID == id {
			removed = true
			continue
		}
		out = append(out, k)
	}
	keysDB = out
	keyMu.Unlock()
	s.saveKeys()
	writeJSON(w, http.StatusOK, map[string]any{"ok": removed, "id": id})
}

// adminUsage 按密钥聚合用量。
func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := parseInt(v); err == nil && n > 0 {
			days = n
		}
	}
	since := time.Now().AddDate(0, 0, -days).Unix()

	type agg struct {
		KeyID    string `json:"key_id"`
		Name     string `json:"name"`
		Requests int    `json:"requests"`
		Chars    int    `json:"chars"`
		Tokens   int    `json:"tokens"`
		Today    int    `json:"today_requests"`
		TodayTok int    `json:"today_tokens"`
		LastTS   int64  `json:"last_ts"`
	}
	byKey := map[string]*agg{}
	todayStart := time.Now().Truncate(24 * time.Hour).Unix()

	f, err := os.Open(s.usagePath())
	if err == nil {
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			var rec struct {
				TS     int64  `json:"ts"`
				Key    string `json:"key"`
				Chars  int    `json:"chars"`
				Tokens int    `json:"tokens"`
			}
			if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.TS < since {
				continue
			}
			a := byKey[rec.Key]
			if a == nil {
				a = &agg{KeyID: rec.Key}
				byKey[rec.Key] = a
			}
			a.Requests++
			a.Chars += rec.Chars
			a.Tokens += rec.Tokens
			if rec.TS >= todayStart {
				a.Today++
				a.TodayTok += rec.Tokens
			}
			if rec.TS > a.LastTS {
				a.LastTS = rec.TS
			}
		}
		f.Close()
	}

	keyMu.RLock()
	for _, k := range keysDB {
		if a, ok := byKey[k.ID]; ok {
			a.Name = k.Name
		}
	}
	keyMu.RUnlock()

	list := make([]*agg, 0, len(byKey))
	for _, a := range byKey {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Tokens > list[j].Tokens })
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "usage": list})
}

func maskKey(k string) string {
	if len(k) <= 10 {
		return "****"
	}
	return k[:6] + "…" + k[len(k)-4:]
}


func itoaSimple(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func parseInt(s string) (int, error) {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, os.ErrInvalid
		}
		n = n*10 + int(c-'0')
	}
	return n, nil
}
