package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"doubao2api/internal/config"
	"doubao2api/internal/pool"
)

// accountsFile 账号文件（面板写入后调用 /admin/reload 热加载）。
type accountsFile struct {
	Accounts []config.Account `json:"accounts"`
}

func (s *Server) accountsPath() string {
	if p := os.Getenv("DOUBAO2API_ACCOUNTS"); p != "" {
		return p
	}
	// 默认与 config.json 同目录
	exe, err := os.Executable()
	if err != nil {
		return "accounts.json"
	}
	return filepath.Join(filepath.Dir(exe), "accounts.json")
}

// adminListAccounts 返回账号名与可用状态（不下发 Cookie）。
func (s *Server) adminListAccounts(w http.ResponseWriter, r *http.Request) {
	stats := s.pool.Stats()
	out := make([]map[string]any, 0, len(stats))
	for _, st := range stats {
		item := map[string]any{
			"name":     st.Name,
			"cooling":  st.Cooling,
			"disabled": st.Disabled,
			"fails":    st.Fails,
			"reason":   st.Reason,
		}
		if a, ok := s.accs[st.Name]; ok {
			item["device_id"] = a.DeviceID
			item["note"] = a.Note
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out, "healthy": s.pool.Healthy(), "total": s.pool.Len()})
}

// adminReload 从 accounts.json 热加载账号（只增不减，避免误删在途请求）。
func (s *Server) adminReload(w http.ResponseWriter, r *http.Request) {
	path := s.accountsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "读取账号文件失败: "+err.Error(), "accounts_file_error", "invalid_request_error")
		return
	}
	var f accountsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		writeErr(w, http.StatusBadRequest, "账号文件格式错误: "+err.Error(), "accounts_parse_error", "invalid_request_error")
		return
	}

	added := []string{}
	s.mu.Lock()
	for i := range f.Accounts {
		a := f.Accounts[i]
		a.Name = strings.TrimSpace(a.Name)
		if a.Name == "" || a.Cookie == "" {
			continue
		}
		if a.LocalAppID == "" {
			a.LocalAppID = "582478"
		}
		if _, exists := s.accs[a.Name]; exists {
			continue // 已存在：不覆盖（Cookie 更新走重启或同名替换策略）
		}
		s.accs[a.Name] = a
		s.pool.Add(pool.Key{Name: a.Name, Key: a.Name})
		added = append(added, a.Name)
	}
	s.mu.Unlock()

	logf("admin_reload file=%s added=%d total=%d", path, len(added), s.pool.Len())
	writeJSON(w, http.StatusOK, map[string]any{
		"added": added, "total": s.pool.Len(), "healthy": s.pool.Healthy(),
	})
}

// adminSetCookie 更新某账号 Cookie（面板改 Cookie 后调用）。
func (s *Server) adminSetCookie(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Cookie string `json:"cookie"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "参数错误", "bad_json", "invalid_request_error")
		return
	}
	if body.Name == "" || body.Cookie == "" {
		writeErr(w, http.StatusBadRequest, "name/cookie 不能为空", "bad_request", "invalid_request_error")
		return
	}
	s.mu.Lock()
	a, ok := s.accs[body.Name]
	if ok {
		a.Cookie = body.Cookie
		s.accs[body.Name] = a
	} else {
		a = config.Account{Name: body.Name, Cookie: body.Cookie, LocalAppID: "582478"}
		s.accs[body.Name] = a
		s.pool.Add(pool.Key{Name: a.Name, Key: a.Name})
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": body.Name})
}

// adminDelete 停用某账号（从池中移除；Cookie 保留在文件里由面板管理）。
func (s *Server) adminDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少 name", "bad_request", "invalid_request_error")
		return
	}
	s.mu.Lock()
	delete(s.accs, name)
	s.mu.Unlock()
	s.pool.Remove(name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": name, "total": s.pool.Len()})
}

// loadAccountsFile 启动时把 accounts.json 里的账号装进池子（重启不丢号）。
func (s *Server) loadAccountsFile() {
	raw, err := os.ReadFile(s.accountsPath())
	if err != nil {
		return
	}
	var f accountsFile
	if json.Unmarshal(raw, &f) != nil {
		return
	}
	n := 0
	s.mu.Lock()
	for i := range f.Accounts {
		a := f.Accounts[i]
		if a.Name == "" || a.Cookie == "" {
			continue
		}
		if a.LocalAppID == "" {
			a.LocalAppID = "582478"
		}
		if _, exists := s.accs[a.Name]; exists {
			continue
		}
		s.accs[a.Name] = a
		s.pool.Add(pool.Key{Name: a.Name, Key: a.Name})
		n++
	}
	s.mu.Unlock()
	if n > 0 {
		logf("startup_accounts loaded=%d total=%d", n, s.pool.Len())
	}
}
