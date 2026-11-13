package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// checkinResult 单个账号的签到结果。
type checkinResult struct {
	Name    string `json:"name"`
	Status  string `json:"status"`  // ok / already / no_campaign / error
	Message string `json:"message"`
	Reward  string `json:"reward,omitempty"`
	Days    int    `json:"days,omitempty"`
	TS      int64  `json:"ts"`
}

var (
	checkinMu   sync.RWMutex
	lastCheckin = map[string]checkinResult{}
)

// doCheckin 调豆包日常签到接口。
//
// 接口：POST /alice/ug/campaign/landing/checkin
// 报文：{"scene": "<scene>", "biz_params": {}}   ← biz_params 必须是对象，传字符串会 400
func (s *Server) doCheckin(name, cookie, dev string) checkinResult {
	res := checkinResult{Name: name, TS: time.Now().Unix()}
	if cookie == "" {
		res.Status, res.Message = "error", "无 Cookie"
		return res
	}
	if dev == "" {
		dev = shortHash(cookie)
	}

	u, err := url.Parse("https://www.doubao.com/alice/ug/campaign/landing/checkin")
	if err != nil {
		res.Status, res.Message = "error", err.Error()
		return res
	}
	q := u.Query()
	for k, v := range s.cfg.Upstream.Query {
		q.Set(k, v)
	}
	q.Set("device_id", dev)
	u.RawQuery = q.Encode()

	scene := s.cfg.Checkin.Scene
	if scene == "" {
		scene = "pc_incentive_task_mvp_v3" // 实测：工作模式「每日签到」活动
	}
	body, _ := json.Marshal(map[string]any{"scene": scene})
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		res.Status, res.Message = "error", err.Error()
		return res
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://www.doubao.com/")
	req.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	req.Header.Set("Cookie", cookie)

	resp, err := s.http.Do(req)
	if err != nil {
		res.Status, res.Message = "error", err.Error()
		return res
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Code    int    `json:"code"`
		Msg     string `json:"msg"`
		Message string `json:"message"`
		Data    struct {
			CheckedIn      bool   `json:"checked_in"`
			CheckinDate    string `json:"checkin_date"`
			ContinuousDays int    `json:"continuous_days"`
			Idempotent     bool   `json:"idempotent"`
			RewardAmount   int    `json:"reward_amount"`
			RewardType     string `json:"reward_type"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &payload)

	if payload.Code != 0 {
		logf("checkin_raw acct=%s http=%d body=%s", name, resp.StatusCode, truncate(string(raw), 300))
	}
	switch {
	case resp.StatusCode == http.StatusOK && payload.Code == 0 && payload.Data.Idempotent:
		res.Status = "already"
		res.Message = "今日已签到（幂等命中）"
		res.Reward = formatReward(payload.Data.RewardAmount)
		res.Days = payload.Data.ContinuousDays
	case resp.StatusCode == http.StatusOK && payload.Code == 0 && payload.Data.CheckedIn:
		res.Status = "ok"
		res.Message = "签到成功"
		res.Reward = formatReward(payload.Data.RewardAmount)
		res.Days = payload.Data.ContinuousDays
	case strings.Contains(payload.Message, "already") || strings.Contains(payload.Msg, "已签到"):
		res.Status, res.Message = "already", "今日已签到"
	case payload.Code == 710010702:
		// 没有可用活动（账号未命中活动/无签到任务）
		res.Status, res.Message = "no_campaign", "当前无可用签到活动"
	default:
		res.Status = "error"
		res.Message = truncate(strings.TrimSpace(payload.Message+payload.Msg), 120)
		if res.Message == "" {
			res.Message = truncate(string(raw), 120)
		}
	}
	return res
}

// extractReward 尽量从返回里挖出奖励数额。
func extractReward(data json.RawMessage) string {
	if len(data) == 0 || string(data) == "null" {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	for _, k := range []string{"reward_amount", "amount", "quota", "credit"} {
		if v, ok := m[k]; ok {
			if f := numOf(v); f > 0 {
				return itoaSimple(int(f)) + " 积分"
			}
		}
	}
	return ""
}

// runCheckinAll 对所有账号执行签到并记录。
func (s *Server) runCheckinAll(only string) []checkinResult {
	type acct struct{ name, cookie, dev string }
	var list []acct
	s.mu.Lock()
	for n, a := range s.accs {
		if only != "" && n != only {
			continue
		}
		list = append(list, acct{n, a.Cookie, a.DeviceID})
	}
	s.mu.Unlock()

	out := make([]checkinResult, 0, len(list))
	for _, a := range list {
		r := s.doCheckin(a.name, a.cookie, a.dev)
		checkinMu.Lock()
		lastCheckin[a.name] = r
		checkinMu.Unlock()
		s.appendCheckinLog(r)
		logf("checkin acct=%s status=%s msg=%s", r.Name, r.Status, r.Message)
		out = append(out, r)
	}
	return out
}

// appendCheckinLog 追加到 checkin.jsonl。
func (s *Server) appendCheckinLog(r checkinResult) {
	line, _ := json.Marshal(r)
	f, err := os.OpenFile(strings.TrimSuffix(s.usagePath(), ".jsonl")+"_checkin.jsonl",
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		_, _ = f.Write(append(line, '\n'))
		f.Close()
	}
}

// checkinLoop 每天在配置的整点执行一次签到。
func (s *Server) checkinLoop() {
	if !s.cfg.Checkin.Enabled {
		return
	}
	hour := s.cfg.Checkin.Hour
	if hour <= 0 || hour > 23 {
		hour = 9 // 默认每天 9 点
	}
	for {
		now := time.Now()
		next := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
		if !next.After(now) {
			next = next.Add(24 * time.Hour)
		}
		time.Sleep(time.Until(next))
		res := s.runCheckinAll("")
		logf("checkin_daily done accounts=%d", len(res))
	}
}

// adminCheckin 手动触发签到：?name= 指定单个账号，缺省全部。
func (s *Server) adminCheckin(w http.ResponseWriter, r *http.Request) {
	res := s.runCheckinAll(r.URL.Query().Get("name"))
	writeJSON(w, http.StatusOK, map[string]any{"results": res, "total": len(res)})
}

// adminCheckinStatus 查看最近一次签到结果。
func (s *Server) adminCheckinStatus(w http.ResponseWriter, r *http.Request) {
	checkinMu.RLock()
	out := make([]checkinResult, 0, len(lastCheckin))
	for _, v := range lastCheckin {
		out = append(out, v)
	}
	checkinMu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"results":     out,
		"enabled":     s.cfg.Checkin.Enabled,
		"hour":        s.cfg.Checkin.Hour,
		"scene":       s.cfg.Checkin.Scene,
		"server_time": time.Now().Format("2006-01-02 15:04:05"),
	})
}

// formatReward 把积分数量格式化成文本。
func formatReward(n int) string {
	if n <= 0 {
		return ""
	}
	return itoaSimple(n) + " 积分"
}
