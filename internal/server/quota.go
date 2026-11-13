package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// quotaGroup 一个额度窗口（如「活动赠送额度」）。
type quotaGroup struct {
	Name    string  `json:"name"`
	Total   float64 `json:"total"`
	Used    float64 `json:"used"`
	Percent float64 `json:"percent"`
	EndTime int64   `json:"end_time"`
}

// quotaResult 剩余额度汇总。
type quotaResult struct {
	Account   string       `json:"account"`
	Total     float64      `json:"total"`
	Used      float64      `json:"used"`
	Remaining float64      `json:"remaining"`
	Percent   float64      `json:"percent"`
	Groups    []quotaGroup `json:"groups"`
	Error     string       `json:"error,omitempty"`
}

// adminQuota 查询账号额度：POST 豆包 commerce 的 quota/summary。
func (s *Server) adminQuota(w http.ResponseWriter, r *http.Request) {
	wantName := r.URL.Query().Get("name")

	var name, cookie, dev string
	s.mu.Lock()
	if wantName != "" {
		if a, ok := s.accs[wantName]; ok {
			name, cookie, dev = a.Name, a.Cookie, a.DeviceID
		}
	} else {
		for n, a := range s.accs {
			name, cookie, dev = n, a.Cookie, a.DeviceID
			break
		}
	}
	s.mu.Unlock()

	if cookie == "" {
		writeJSON(w, http.StatusOK, quotaResult{Error: "没有可用账号"})
		return
	}
	if dev == "" {
		dev = shortHash(cookie)
	}

	u, err := url.Parse("https://www.doubao.com/alice/commerce/sale/subscription/quota/summary/")
	if err != nil {
		writeJSON(w, http.StatusOK, quotaResult{Account: name, Error: err.Error()})
		return
	}
	q := u.Query()
	for k, v := range s.cfg.Upstream.Query {
		q.Set(k, v)
	}
	q.Set("device_id", dev)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, u.String(), bytes.NewReader([]byte("{}")))
	if err != nil {
		writeJSON(w, http.StatusOK, quotaResult{Account: name, Error: err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Referer", "https://www.doubao.com/")
	req.Header.Set("User-Agent", s.cfg.Upstream.UserAgent)
	req.Header.Set("Cookie", cookie)

	resp, err := s.http.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, quotaResult{Account: name, Error: err.Error()})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusOK, quotaResult{Account: name,
			Error: "上游 " + resp.Status + ": " + truncate(string(raw), 120)})
		return
	}

	out := quotaResult{Account: name}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		out.Error = "解析失败: " + err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	if data, ok := payload["data"].(map[string]any); ok {
		collectQuota(data, &out)
	}
	out.Remaining = out.Total - out.Used
	if out.Remaining < 0 {
		out.Remaining = 0
	}
	if out.Total > 0 {
		out.Percent = float64(int(out.Used/out.Total*1000)) / 10
	}
	writeJSON(w, http.StatusOK, out)
}

// collectQuota 递归找出所有 window_limits 并累加。
func collectQuota(v any, out *quotaResult) {
	switch t := v.(type) {
	case map[string]any:
		if limits, ok := t["window_limits"].([]any); ok {
			gname, _ := t["feature_group_name"].(string)
			for _, l := range limits {
				lm, ok := l.(map[string]any)
				if !ok {
					continue
				}
				total := numOf(lm["total_amount"])
				used := numOf(lm["used_amount"])
				if total <= 0 && used <= 0 {
					continue
				}
				out.Total += total
				out.Used += used
				out.Groups = append(out.Groups, quotaGroup{
					Name: gname, Total: total, Used: used,
					Percent: numOf(lm["used_percent"]),
					EndTime: int64(numOf(lm["end_time"])),
				})
			}
		}
		for _, child := range t {
			collectQuota(child, out)
		}
	case []any:
		for _, child := range t {
			collectQuota(child, out)
		}
	}
}

// numOf 兼容 JSON 数字与数字字符串。
func numOf(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		clean := strings.NewReplacer(",", "", " ", "", "¥", "", "$", "").Replace(t)
		f, err := strconv.ParseFloat(strings.TrimSpace(clean), 64)
		if err == nil {
			return f
		}
	}
	return 0
}
