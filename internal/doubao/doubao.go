// Package doubao 实现豆包 /chat/completion 的请求构造与 SSE 事件解析。
//
// 协议要点（来自实抓）：
//   - 正文增量优先取 CHUNK_DELTA 的 {"text": "..."}；
//   - 没有 CHUNK_DELTA 时退回 STREAM_CHUNK 里 block_type=10000 的 text_block.text；
//   - 思考过程是 block_type=10040 的 thinking_block（标题文案）；
//   - 会话 id 从 SSE_ACK 的 ack_client_meta 取；
//   - 收尾看 SSE_REPLY_END.end_type，end_type=1 且 msg_finish_attr.brief 是完整答案。
package doubao

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ---------- 对外请求 ----------

// Msg OpenAI 风格消息。
type Msg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// ChatRequest 客户端请求（只取本网关用得到的字段）。
type ChatRequest struct {
	Model       string          `json:"model"`
	Messages    []Msg           `json:"messages"`
	Stream      bool            `json:"stream"`
	MaxTokens   int             `json:"max_tokens"`
	Temperature *float64        `json:"temperature"`
	TopP        *float64        `json:"top_p"`
	User        string          `json:"user"`
	Metadata    map[string]any  `json:"metadata"`
	StreamOpts  *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// Text 提取 content 的纯文本。
func (m Msg) Text() string {
	if len(m.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(m.Content, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Text != "" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return ""
}

// BuildOptions 工作模式与会话参数。
type BuildOptions struct {
	BotID           string
	ModeID          string
	ReasoningEffort string
	MemoryProfile   string
	TotalWindow     string
	EnableSandbox   bool
	Workspace       string
	DeviceName      string
	LocalAppID      string
	LocalDeviceID   string
	WorkMode        bool

	ConversationID string // 已有会话则带上
	Flatten        bool   // 首轮把历史拼成一段
}

// BuildBody 构造 /chat/completion 的请求体。
func BuildBody(req *ChatRequest, modelKey string, o BuildOptions) ([]byte, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("messages 不能为空")
	}
	// 最后一条 user 文本
	lastUser := ""
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUser = req.Messages[i].Text()
			break
		}
	}
	if lastUser == "" {
		return nil, fmt.Errorf("缺少 user 消息")
	}

	text := lastUser
	if o.ConversationID == "" && o.Flatten {
		// 首轮：把历史压成一段，避免丢上下文
		lastIdx := len(req.Messages) - 1
		var b strings.Builder
		for i, m := range req.Messages {
			t := strings.TrimSpace(m.Text())
			if t == "" {
				continue
			}
			if i == lastIdx {
				break
			}
			switch m.Role {
			case "user":
				b.WriteString("用户：")
			case "assistant":
				b.WriteString("助手：")
			default:
				b.WriteString(m.Role + "：")
			}
			b.WriteString(t)
			b.WriteString("\n")
		}
		if b.Len() > 0 {
			text = b.String() + "用户：" + lastUser
		}
	}

	msgID := uuid()
	localConv := "local_" + randomDigits(16)
	nowMs := nowMillis()

	block := map[string]any{
		"block_type": 10000,
		"content": map[string]any{
			"text_block":     map[string]any{"text": text, "icon_url": "", "icon_url_dark": "", "summary": ""},
			"pc_event_block": "",
		},
		"block_id":      uuid(),
		"parent_id":     "",
		"meta_info":     []any{},
		"append_fields": []any{},
	}

	body := map[string]any{
		"client_meta": map[string]any{
			"local_conversation_id": localConv,
			"conversation_id":       o.ConversationID,
			"bot_id":                o.BotID,
			"last_section_id":       "",
			"last_message_index":    nil,
			"local_permissions": []any{
				map[string]any{"permission_name": "ACCESS_COARSE_LOCATION", "status": 1},
				map[string]any{"permission_name": "ACCESS_FINE_LOCATION", "status": 1},
				map[string]any{"permission_name": "ACCESS_BACKGROUND_LOCATION", "status": 1},
			},
		},
		"messages": []any{map[string]any{
			"local_message_id": msgID,
			"content_block":    []any{block},
			"message_status":   0,
		}},
	}

	opt := map[string]any{
		"create_time_ms":             nowMs,
		"answer_with_suggest":        false,
		"agent_mode":                 boolToInt(o.WorkMode),
		"need_deep_think":            10001,
		"unique_key":                 uuid(),
		"start_seq":                  0,
		"need_create_conversation":   o.ConversationID == "",
		"conversation_init_option":   map[string]any{"need_ack_conversation": true},
		"sse_recv_event_options":     map[string]any{"support_chunk_delta": true},
		"support_lazy_fetch_stream":  true,
		"is_old_user":                true,
		"message_storage_type":       0,
		"conversation_mode":          modeInt(o.WorkMode),
	}

	initExt := map[string]any{
		"model_item_key":   modelKey,
		"reasoning_effort": o.ReasoningEffort,
		"mode_id":          o.ModeID,
	}
	taskParam := map[string]any{
		"runtime_type":      2,
		"sandbox_auth_type": 1,
		"device_name":       o.DeviceName,
		"folder_name":       "",
		"local_app_id":      o.LocalAppID,
		"local_device_id":   o.LocalDeviceID,
		"workspace":         o.Workspace,
	}
	initExt["agent_task_param"] = mustJSONString(taskParam)
	opt["conversation_init_ext"] = initExt

	opt["model_config"] = map[string]any{
		"model_item_key": modelKey,
		"model_extra_params": map[string]any{
			"memory_profile":    o.MemoryProfile,
			"total_window_size": o.TotalWindow,
		},
		"reasoning_effort": atoiSafe(o.ReasoningEffort),
	}
	opt["aggregate_params"] = map[string]any{
		"mention_skill_list":  "[]",
		"mention_plugin_list": "[]",
		"mention_ext":         "[{}]",
		"conversation_mode":   itoa(modeInt(o.WorkMode)),
		"mode_id":             o.ModeID,
		"model_item_key":      modelKey,
		"agent_mode":          itoa(boolToInt(o.WorkMode)),
		"reasoning_effort":    o.ReasoningEffort,
		"provider_id":         "",
	}

	if o.WorkMode {
		shared := []string{}
		if o.Workspace != "" {
			shared = append(shared, o.Workspace)
		}
		clientOpt := map[string]any{
			"enable_sandbox":    o.EnableSandbox,
			"os":                "Windows",
			"shared_folder_path": shared,
			"agent_workspace": map[string]any{
				"agent_workspace":   o.Workspace,
				"local_skill_paths": []string{},
			},
			"client_env_id":     uuid(),
			"sandbox_id":        "sandbox-" + uuid(),
			"sandbox_auth_type": 1,
		}
		gtp := map[string]any{
			"action":                   0,
			"thread_local_message_id":  []string{msgID},
			"client_option":            clientOpt,
			"runtime_type":             2,
			"agent_task_param":         taskParam,
			"agent_task_param_change":  map[string]any{"runtime_changed": false, "device_changed": false, "sandbox_auth_type_changed": false},
			"need_modify_conversation": false,
			"task_input_json": mustJSONString(map[string]any{
				"agents_md":       map[string]any{"files": []any{}, "state": 2},
				"schema_version":  1,
				"home_dir":        homeDir(o.Workspace),
				"project_context": map[string]any{},
				"localConnectors": []any{},
			}),
		}
		opt["general_task_param"] = gtp
		opt["ext"] = map[string]any{
			"general_task_param":               mustJSONString(gtp),
			"use_deep_think":                   "10001",
			"agent_mode":                       "1",
			"is_finish":                        "1",
			"conversation_init_option":         `{"need_ack_conversation":true}`,
			"commerce_credit_config_enable":    "0",
		}
	}

	body["option"] = opt
	body["user_context"] = []any{}
	if _, ok := opt["ext"]; !ok {
		body["ext"] = map[string]any{"use_deep_think": "10001", "is_finish": "1"}
	}
	return json.Marshal(body)
}

// ---------- SSE 解析 ----------

// Sink 流事件回调。
type Sink interface {
	OnText(string)                  // 正文增量
	OnReasoning(string)             // 思考过程（标题级）
	OnConversation(conv, section string)
	OnFinish(endType int, brief string)
}

// Pump 解析 SSE 并回调。返回 error 表示连接/解析异常。
func Pump(r io.Reader, sink Sink) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var event string
	var data []string
	sawChunkDelta := false

	dispatch := func() error {
		if event == "" && len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		ev := event
		event, data = "", nil
		if payload == "" {
			return nil
		}
		switch ev {
		case "CHUNK_DELTA":
			var d struct {
				Text string `json:"text"`
			}
			if json.Unmarshal([]byte(payload), &d) == nil && d.Text != "" {
				sawChunkDelta = true
				sink.OnText(d.Text)
			}
		case "STREAM_CHUNK":
			var d struct {
				PatchOp []struct {
					PatchValue struct {
						ContentBlock []struct {
							BlockType int `json:"block_type"`
							Content   struct {
								TextBlock *struct {
									Text string `json:"text"`
								} `json:"text_block"`
								ThinkingBlock *struct {
									StreamingTitle string `json:"streaming_title"`
									FinishTitle    string `json:"finish_title"`
								} `json:"thinking_block"`
							} `json:"content"`
						} `json:"content_block"`
					} `json:"patch_value"`
				} `json:"patch_op"`
			}
			if json.Unmarshal([]byte(payload), &d) != nil {
				return nil
			}
			for _, op := range d.PatchOp {
				for _, b := range op.PatchValue.ContentBlock {
					switch b.BlockType {
					case 10000:
						if !sawChunkDelta && b.Content.TextBlock != nil && b.Content.TextBlock.Text != "" {
							sink.OnText(b.Content.TextBlock.Text)
						}
					case 10040:
						if b.Content.ThinkingBlock != nil {
							t := b.Content.ThinkingBlock.StreamingTitle
							if t == "" {
								t = b.Content.ThinkingBlock.FinishTitle
							}
							if t != "" {
								sink.OnReasoning(t)
							}
						}
					}
				}
			}
		case "SSE_ACK":
			var d struct {
				AckClientMeta struct {
					ConversationID string `json:"conversation_id"`
					SectionID      string `json:"section_id"`
				} `json:"ack_client_meta"`
			}
			if json.Unmarshal([]byte(payload), &d) == nil {
				sink.OnConversation(d.AckClientMeta.ConversationID, d.AckClientMeta.SectionID)
			}
		case "SSE_REPLY_END":
			var d struct {
				EndType       int `json:"end_type"`
				MsgFinishAttr struct {
					Brief string `json:"brief"`
				} `json:"msg_finish_attr"`
			}
			if json.Unmarshal([]byte(payload), &d) == nil {
				sink.OnFinish(d.EndType, d.MsgFinishAttr.Brief)
			}
		}
		return nil
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			if ferr := dispatch(); ferr != nil {
				return ferr
			}
			if err == io.EOF {
				return io.EOF
			}
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		switch {
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		case strings.HasPrefix(line, ":"):
			// 注释帧
		}
	}
}

// ---------- 小工具 ----------

func uuid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

func randomDigits(n int) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	s := ""
	for i := 0; i < n; i++ {
		s += string(rune('0' + int(b[i%8])%10))
	}
	return s
}

func lastMessage(req *ChatRequest) Msg {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			return req.Messages[i]
		}
	}
	return Msg{}
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func mustJSONString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func modeInt(workMode bool) int {
	if workMode {
		return 2
	}
	return 0
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func homeDir(workspace string) string {
	if workspace == "" {
		return ""
	}
	if i := strings.Index(workspace, `\Doubao`); i > 0 {
		return workspace[:i]
	}
	return ""
}
