# doubao2api

按 **workbuddy2api** 的架构，把**豆包客户端「工作模式」**的 `/chat/completion` 包装成 OpenAI 兼容接口。

```
OpenAI SDK ──/v1/chat/completions──▶ doubao2api ──/chat/completion──▶ www.doubao.com
                                        │
                          账号池(Cookie) / 会话粘性 / 错误策略 / SSE 转换 / 心跳
```

## 与 workbuddy2api 的对应关系

| workbuddy2api | doubao2api |
|---|---|
| 账号池（token 文件） | **账号池（Cookie 串）** |
| `/v1/chat/completions` 上游 | `/chat/completion`（patch/SSE） |
| 会话粘性保前缀缓存 | 会话粘性 + **conversation_id 复用**（多轮） |
| 大请求预算 | 同（复用同一实现） |
| 上游错误分类（ErrKind） | HTTP 401/403/429/5xx + `end_type≠1` |
| SSE 帧序修正 / 截断判定 | `CHUNK_DELTA` / `brief` 兜底 |

## 已实现

- OpenAI `/v1/chat/completions`（流式 + 非流式）、`/v1/models`、`/healthz`、`/status`
- 请求翻译：`messages[]` → `content_block[block_type=10000]`；工作模式参数（`agent_mode=1`、`conversation_mode=2`、`mode_id`、`reasoning_effort`、`model_item_key`）
- 响应翻译：`CHUNK_DELTA.text` → `delta.content`；`block_type=10040` → `delta.reasoning_content`；`SSE_REPLY_END.brief` 兜底；`[DONE]` 收尾
- 多轮：`SSE_ACK` 取 `conversation_id`/`section_id`，按会话键复用；首轮可把历史压平成一段（`flatten_history`）
- 账号池：401/403 停用、429 冷却、5xx 短冷却、粘性绑定、大请求预算
- SSE 心跳（`heartbeat_seconds`）：上游静默时下发 `: ping`
- 模型别名：`doubao` / `deepseek` → `person-deepseek-v4-1-flash-261017`（可改）
- 签名扩展位：`upstream.a_bogus`（固定值调试）与 `upstream.sign_command`（外部命令现算）

## 配置要点

| 键 | 说明 |
|---|---|
| `accounts[].cookie` | 豆包登录态 Cookie（`sessionid`/`sid_tt`/`ttwid`/`s_v_web_id`/`odin_tt`…） |
| `accounts[].device_id` / `web_id` | 与 Cookie 同源的设备标识；缺省由 Cookie 派生 |
| `work_mode.enabled` | 是否按工作模式（`agent_mode=1`）发请求；关闭则为普通对话 |
| `work_mode.enable_sandbox` | 工作模式会在本机起沙箱；纯聊天建议 `false` |
| `upstream.flatten_history` | 首轮把历史拼成一段（否则只发最后一条） |
| `upstream.a_bogus` / `sign_command` | 签名：固定值或外部命令（stdin=完整 URL，stdout=a_bogus） |

## 运行

```bash
cp config.example.json config.json    # 填 accounts[].cookie / device_id / web_id
go build -trimpath -o doubao2api ./cmd/server
./doubao2api -config config.json
```

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8850/v1
export OPENAI_API_KEY=sk-downstream-change-me

curl -N $OPENAI_BASE_URL/chat/completions \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek","stream":true,"metadata":{"conversation_id":"demo-1"},
       "messages":[{"role":"user","content":"你好"}]}'
```

## 测试

```bash
go test ./...
```

覆盖：请求体关键字段（`model_item_key`/`agent_mode`/`conversation_mode`/`support_chunk_delta`）、
非流式拼接、流式增量与 `reasoning_content`、`[DONE]`、**conversation_id 复用**、下游鉴权、401 换号重试。

单元测试用内置假上游（复刻实抓 SSE 序列），不需要真实 Cookie。

## 已知限制 / 待办

1. **`a_bogus` 签名未验证**：实抓请求带 `msToken`+`a_bogus`；本版本默认不带，需实测服务端是否放行。
   若拒绝，用 `sign_command` 挂 Node 签名脚本（BDMS）。
2. 工作模式会触发本机 sandbox；headless 场景需 `enable_sandbox=false` 并把 `workspace` 指到真实目录。
3. `usage` 未实现（豆包 SSE 未返回 token 计数），目前返回 0。
4. 未实现附件/图片消息（只有文本块）。

## 管理面板（manager/）

`manager/app.py` 是配套的号池管理面板，**纯 Python 标准库**，零依赖、无构建：

```bash
cd manager
GW_BASE=http://127.0.0.1:8850 GW_KEY=<下游密钥> python3 app.py
# 打开 http://127.0.0.1:8860
```

能力：账号增删改（写 `accounts.json` → 调网关 `/admin/reload` 热加载）、号池状态、聊天测试（SSE 透传）、扫码加号入口。

### 网关管理接口

| 接口 | 说明 |
|---|---|
| `GET /admin/accounts` | 账号 + 冷却/停用状态（不下发 Cookie） |
| `POST /admin/reload` | 从 `accounts.json` 热加载（只增不减） |
| `POST /admin/cookie` | 单账号 Cookie 热更新 |
| `DELETE /admin/accounts?name=` | 移出号池 |
| `POST /admin/accounts/qr` | 创建扫码会话 |
| `GET /admin/accounts/qr?id=` | 轮询扫码状态，确认后自动入池 |
