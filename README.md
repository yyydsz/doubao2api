# doubao2api

把**豆包客户端「工作模式」**的 `/chat/completion` 协议包装成 **OpenAI 兼容**接口，附带号池管理与密钥分发面板。

单二进制 Go 网关 + 单文件 Python 面板，网关零第三方 Go 依赖。

```
OpenAI SDK / DSH / Cherry Studio
        │  POST /v1/chat/completions
        ▼
   ┌─────────────┐
   │  doubao2api │  号池 / 会话粘性 / 错误策略 / SSE 转换 / 心跳
   └─────────────┘
        │  POST https://www.doubao.com/chat/completion
        ▼
      豆包（agent_mode=1 工作模式，模型由 model_item_key 决定）
```

---

## 功能

### 网关

- `/v1/chat/completions`：流式 + 非流式，OpenAI 协议
- `/v1/models`：模型清单（支持别名）
- 请求翻译：`messages[]` → 豆包 `content_block[block_type=10000]`；`option.agent_mode=1`、`conversation_mode=2`、`mode_id`、`reasoning_effort`、`model_item_key`
- 响应翻译：`CHUNK_DELTA.text` → `delta.content`；`block_type=10040` → `delta.reasoning_content`；`SSE_REPLY_END.brief` 兜底
- 多轮：`SSE_ACK` 取 `conversation_id` / `section_id` 复用，首轮可压平历史
- **号池**：Cookie 账号池、会话粘性、冷却/停用、大请求预算
- **SSE 心跳**：上游静默超过阈值下发 `: ping`，防中间层判超时断流
- **多密钥分发**：`/v1/*` 校验密钥，按密钥统计 token 用量
- **管理接口**：`/admin/*` 仅回环可访问（与面板同机）

### 面板

- 概览：剩余额度、今日消耗、号池健康、请求数、平均延迟
- 账号：扫码加号、账号列表、启停、删除
- 号池：实时状态（在线 / 冷却 / 停用、失败次数）
- 密钥：生成 / 复制 / 删除，**分密钥 token 用量**（7 天 + 今日）
- 测试台：模型下拉 + 思考强度 + 流式逐字渲染 + 可中断
- 请求日志：时间、模型、状态、耗时、字符、会话

---

## 快速开始

### 1. 编译网关

```bash
git clone https://github.com/yyydsz/doubao2api
cd doubao2api
go build -trimpath -o doubao2api ./cmd/server
```

### 2. 配置

```bash
cp config.example.json config.json
```

最少需要填 `accounts[].cookie`（豆包登录态 Cookie）；也可以留空，启动后在面板里添加。

```jsonc
{
  "listen": "127.0.0.1:8850",
  "api_keys": [],                        // 留空：由面板生成写入 keys.json
  "upstream": {
    "chat_url": "https://www.doubao.com/chat/completion",
    "heartbeat_seconds": 15,
    "flatten_history": true
  },
  "accounts": [],                        // 留空：由面板写入 accounts.json
  "default_model": "doubao",
  "model_aliases": { "deepseek": "person-deepseek-v4-1-flash-261017" },
  "work_mode": {
    "enabled": true,
    "bot_id": "7338286299411103781",
    "mode_id": "3",
    "reasoning_effort": "4"
  }
}
```

### 3. 启动网关

```bash
./doubao2api -config config.json
```

### 4. 启动面板

```bash
cd manager
python3 -m venv venv && ./venv/bin/pip install fastapi "uvicorn[standard]"
GW_BASE=http://127.0.0.1:8850 PANEL_AUTH=admin:yourpass \
  ./venv/bin/uvicorn app.main:app --host 0.0.0.0 --port 8860
```

浏览器打开 `http://127.0.0.1:8860` → 「密钥」页生成 key → 「测试台」直接对话。

### 5. 客户端配置

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8850/v1
export OPENAI_API_KEY=sk-xxxxxxxx        # 面板「密钥」页生成
```

---

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | OpenAI 兼容对话（流式 / 非流式） |
| GET | `/v1/models` | 模型清单 |
| GET | `/healthz` | 健康检查 |
| GET | `/admin/accounts` | 号池状态 |
| POST | `/admin/reload` | 从 `accounts.json` 热加载 |
| POST | `/admin/cookie` | 单账号 Cookie 热更新 |
| DELETE | `/admin/accounts?name=` | 移出号池 |
| POST / GET | `/admin/accounts/qr` | 扫码会话创建 / 轮询 |
| GET | `/admin/quota` | 剩余额度（豆包 commerce 接口） |
| GET / POST / DELETE | `/admin/keys` | 密钥管理 |
| GET | `/admin/usage?days=7` | 按密钥聚合用量 |

> `/admin/*` 仅允许回环地址；`/v1/*` 必须携带有效密钥；未创建密钥时返回明确提示而非放行。

---

## 协议要点（实测）

工作模式与普通对话共用同一端点，靠参数区分：

| 字段 | 值 | 含义 |
|---|---|---|
| `option.agent_mode` | `1` | 工作模式 |
| `option.conversation_mode` | `2` | 工作模式会话 |
| `option.mode_id` | `"3"` | 模式 |
| `option.model_config.model_item_key` | 如 `person-deepseek-v4-1-flash-261017` | **选模型** |
| `option.aggregate_params.model_item_key` | 同上 | 三处需一致 |
| `option.sse_recv_event_options.support_chunk_delta` | `true` | 开启 `CHUNK_DELTA` 文本增量 |
| `option.conversation_init_ext.reasoning_effort` | `"0"`..`"4"` | 思考强度 |

响应事件顺序（实测）：

```
SSE_HEARTBEAT     {}                                  心跳
SSE_ACK           conversation_id / section_id        建会话回执
FULL_MSG_NOTIFY   用户消息入库回执
STREAM_MSG_NOTIFY 首帧壳（agent_id / model_id）
STREAM_CHUNK      patch_op[]（10000 正文 / 10040 思考 / 111 tts）
CHUNK_DELTA       {"text": "..."}                     正文增量（优先）
SSE_REPLY_END     end_type=1 时 msg_finish_attr.brief 完整答案（兜底）
```

额度查询（面板「剩余额度」）：

```
POST https://www.doubao.com/alice/commerce/sale/subscription/quota/summary/
→ data.*.window_limits[].{total_amount, used_amount, used_percent}
```

---

## 已知限制

1. **扫码登录**：Passport `get_qrcode` 在客户端侧带 `sign` / `a_bogus` / `x-tt-passport-aid-sign` 三层签名，纯 HTTP 复刻不稳定。内置参数可生成二维码，但 App 侧确认环节仍可能失败——**推荐「抓包 Cookie 导入」**。
2. **token 用量为估算**：豆包 SSE 不返回 usage，按输出字符 ÷1.7 折算。
3. 工作模式会触发本机 sandbox，headless 场景需 `work_mode.enable_sandbox=false`。
4. 仅支持文本消息，附件/图片未实现。
5. 本项目为协议研究用途，账号与额度风险自负。

---

## 从抓包导入账号

豆包客户端登录后，其请求头里的 Cookie 即完整登录态：

```bash
cat > /opt/doubao2api/accounts.json <<'EOF'
{"accounts":[{"name":"main","cookie":"sessionid=...; sid_tt=...; ttwid=...; s_v_web_id=...; odin_tt=...; passport_csrf_token=...","device_id":"<设备ID>","web_id":"<web_id>","local_app_id":"582478"}]}
EOF
sudo systemctl restart doubao2api     # 启动时自动加载 accounts.json
```

---

## 目录结构

```
cmd/server/          启动入口
internal/config/     配置加载（账号 / 工作模式 / QR / 池参数）
internal/doubao/     请求体构造 + SSE 解析
internal/pool/       号池（粘性 / 冷却 / 停用 / 大请求预算）
internal/server/     HTTP 层：协议转换、调度、密钥、额度、扫码
manager/app/         面板（FastAPI + 单页前端）
```

---

## 鸣谢

本项目的设计大量借鉴了以下两个项目，特此致谢：

- **[ithtelab/workbuddy-manager](https://github.com/ithtelab/workbuddy-manager)** —— 号池 + 管理面板的整体架构、会话粘性与大请求预算的设计、面板的交互形态，均参考自该项目。
- **workbuddy2api** —— 上游协议适配层（OpenAI 门面 / SSE 转换 / 错误分级 / 粘性调度）的实现范式源自它，本项目的网关结构与之同构。

没有这两个项目的前期探索，就没有这套「协议逆向 → OpenAI 兼容 → 可运营」的落地路径。

---

## License

MIT

---

## 工作模式可用模型

豆包工作模式（`agent_mode=1` / `mode_id=3`）实测可用的 10 个模型，已预置在 `config.example.json` 的 `models` + `model_aliases` 中：

| 对外模型名 | 豆包 model_item_key | 实测 |
|---|---|---|
| `auto` | `9` | ✅ 可用 |
| `doubao-2.1-lite` | `seed-lite-7b` | ✅ 可用 |
| `doubao-2.1-pro` | `5` | ✅ 可用 |
| `deepseek-v4-flash` | `person-deepseek-v4-1-flash-261017` | ✅ 可用 |
| `deepseek-v4-pro` | `person-deepseek-v4-pro-ga-260813` | ✅ 可用 |
| `kimi-k3` | `person-kimi-k3-261111` | ✅ 可用 |
| `kimi-k3.1-pro` | `person-kimi-k3-1-pro-261111` | ✅ 可用 |
| `kimi-k3.1-pro-fast` | `person-kimi-k3-1-pro-highspeed-261111` | ✅ 可用 |
| `glm-5.3` | `person-glm-5-3-261111` | ⚠️ 上游返回通用错误（可能受账号档位限制） |
| `glm-5.3-flash` | `person-glm-5-3-flash-261111` | ⚠️ 同上 |

兼容别名：`doubao`、`deepseek` 均指向 `person-deepseek-v4-1-flash-261017`。

> 清单来源：客户端 `_DesktopActionBarBriefListRequestCacheDataV1` 缓存（`modeSelectData.menuConfV2.mode_list`），非接口下发。豆包若调整模型，同步更新 `model_aliases` 即可。

### 上下文窗口（实测自客户端缓存）

`/v1/models` 现在会返回 `context_length`，网关也按模型发送对应的 `memory_profile` / `total_window_size`：

| 模型 | 上下文 | memory_profile |
|---|---|---|
| `auto` / `doubao-2.1-lite` / `doubao-2.1-pro` | **256,000** | `256k`（Pro/Auto 未声明 profile） |
| `deepseek-v4-flash` / `deepseek-v4-pro` | **300,000** | `300k_230` |
| `kimi-k3` / `kimi-k3.1-pro` / `kimi-k3.1-pro-fast` | **300,000** | `300k_230` |
| `glm-5.3` / `glm-5.3-flash` | **300,000** | `300k_230` |

结论：**300k 就是当前档位能拿到的上限**，不存在更大的窗口可开（豆包 2.1 系列反而是 256k）。
