package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"doubao2api/internal/config"
	"doubao2api/internal/pool"
)

// qrSession 一次扫码登录会话。
type qrSession struct {
	ID        string
	Token     string
	Status    string // waiting | scanned | confirmed | expired | error
	Message   string
	Image     string // data:image/png;base64,...（若有）
	QRURL     string
	Account   string
	CreatedAt time.Time
}

var (
	qrMu   sync.Mutex
	qrSess = map[string]*qrSession{}
)

// qrStart 创建扫码会话：请求 passport 的 get_qrcode，返回二维码。
func (s *Server) qrStart(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.QR.Enabled || s.cfg.QR.GetURL == "" {
		writeErr(w, http.StatusNotImplemented,
			"扫码登录未启用：请在 config.json 配置 qr.get_url / qr.check_url（参数可从抓包得到）",
			"qr_disabled", "not_implemented")
		return
	}
	u, err := url.Parse(s.cfg.QR.GetURL)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "qr.get_url 非法", "qr_config", "api_error")
		return
	}
	q := u.Query()
	for k, v := range s.cfg.QR.Query {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	req.Header.Set("Referer", "https://www.doubao.com/")
	resp, err := s.http.Do(req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "请求二维码失败: "+err.Error(), "qr_upstream", "api_error")
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Message string `json:"message"`
		Data    struct {
			ErrorCode      int    `json:"error_code"`
			Description    string `json:"description"`
			Token          string `json:"token"`
			Qrcode         string `json:"qrcode"`
			Base64Image    string `json:"base64_image"`
			QrcodeIndexURL string `json:"qrcode_index_url"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &payload)
	if payload.Data.ErrorCode != 0 || (payload.Data.Token == "" && payload.Data.Qrcode == "") {
		msg := payload.Data.Description
		if msg == "" {
			msg = payload.Message
		}
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		writeErr(w, http.StatusBadGateway, "上游拒绝生成二维码: "+msg, "qr_rejected", "api_error")
		return
	}

	token := payload.Data.Token
	if token == "" {
		token = payload.Data.Qrcode
	}
	image := payload.Data.Base64Image
	if image == "" {
		image = payload.Data.Qrcode // 实测：qrcode 字段本身就是 data:image/png;base64,...
	}
	sess := &qrSession{
		ID:        randomHex(8),
		Token:     token,
		Status:    "waiting",
		Image:     image,
		QRURL:     payload.Data.QrcodeIndexURL,
		CreatedAt: time.Now(),
	}
	qrMu.Lock()
	qrSess[sess.ID] = sess
	// 清理 10 分钟前的会话
	for id, v := range qrSess {
		if time.Since(v.CreatedAt) > 10*time.Minute {
			delete(qrSess, id)
		}
	}
	qrMu.Unlock()

	logf("qr_start id=%s token_len=%d", sess.ID, len(token))
	writeJSON(w, http.StatusOK, map[string]any{
		"id": sess.ID, "status": sess.Status,
		"image": sess.Image, "qrcode_url": sess.QRURL,
	})
}

// qrPoll 轮询扫码状态；确认后把 Cookie 加入号池。
func (s *Server) qrPoll(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	qrMu.Lock()
	sess, ok := qrSess[id]
	qrMu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "会话不存在或已过期", "qr_not_found", "invalid_request_error")
		return
	}

	u, err := url.Parse(s.cfg.QR.CheckURL)
	if err != nil || s.cfg.QR.CheckURL == "" {
		writeErr(w, http.StatusInternalServerError, "qr.check_url 未配置", "qr_config", "api_error")
		return
	}
	q := u.Query()
	for k, v := range s.cfg.QR.Query {
		q.Set(k, v)
	}
	q.Set("token", sess.Token)
	u.RawQuery = q.Encode()

	req, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	req.Header.Set("Referer", "https://www.doubao.com/")
	resp, err := s.http.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": sess.Status, "message": "轮询失败: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Message string `json:"message"`
		Data    struct {
			Status      string `json:"status"`      // 实测为字符串：new / scanning / confirmed / expired
			Description string `json:"description"`
			RedirectURL string `json:"redirect_url"`
			ErrorCode   int    `json:"error_code"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &payload)

	status := mapQRStatus(payload.Data.Status)
	if status == "" {
		status = sess.Status
		if payload.Data.Description != "" {
			sess.Message = payload.Data.Description
		}
	}
	if payload.Data.RedirectURL != "" {
		sess.Message = payload.Data.RedirectURL
	}

	if status == "confirmed" {
		cookie := collectCookies(resp)
		if cookie != "" {
			name := "qr-" + randomHex(4)
			s.mu.Lock()
			s.accs[name] = config.Account{Name: name, Cookie: cookie, LocalAppID: "582478", Note: "扫码新增"}
			s.mu.Unlock()
			s.pool.Add(pool.Key{Name: name, Key: name})
			sess.Account = name
			logf("qr_confirmed id=%s account=%s", id, name)
		}
	}

	qrMu.Lock()
	sess.Status = status
	qrMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "status": status, "message": sess.Message, "account": sess.Account,
	})
}

// collectCookies 把上游 Set-Cookie 拼成请求头格式。
func collectCookies(resp *http.Response) string {
	parts := []string{}
	for _, c := range resp.Cookies() {
		if c.Name == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}
	return strings.Join(parts, "; ")
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// mapQRStatus 把 passport 返回的状态（字符串/数字字符串）归一化成内部状态。
func mapQRStatus(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "new", "1", "0", "waiting":
		return "waiting"
	case "scanning", "scanned", "2", "5":
		return "scanned"
	case "confirmed", "success", "3", "6":
		return "confirmed"
	case "expired", "4", "7":
		return "expired"
	case "canceled", "cancelled", "8":
		return "canceled"
	}
	return ""
}
