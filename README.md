# doubao2api

把**豆包客户端「工作模式」**的 `/chat/completion` 协议包装成 **OpenAI 兼容**接口，附带号池、密钥分发、额度与签到管理面板。

单二进制 Go 网关 + 单文件 Python 面板，网关零第三方 Go 依赖。

```
OpenAI SDK / DSH / Cherry Studio
        │  POST /v1/chat/completions
        ▼
   ┌─────────────┐
   │  doubao2api │  号池 / 会话粘性 / 多模型 / 密钥 / SSE 转换 / 心跳
   └─────────────┘
        │  POST https://www.doubao.com/chat/completion
        ▼
      豆包（agent_mode=1 工作模式，model_item_key 决定模型）
```

---

## 功能

### 网关

- `/v1/chat/completions`：流式 + 非流式，OpenAI 协议
- `/v1/models`：模型清单，带 `context_length` / `max_tokens`
- **10 个模型可切**：豆包 / DeepSeek / Kimi / GLM，逐个实测过（见下表）
- **按模型设置上下文**：不同模型窗口不同（256k / 300k），网关按请求模型发送对应 `memory_profile`
- 请求翻译：`messages[]` → `content_block[block_type=10000]`；`agent_mode=1`、`conversation_mode=2`、`mode_id`、`reasoning_effort`、`model_item_key`
- 响应翻译：`CHUNK_DELTA.text` → `delta.content`；`block_type=10040` → `delta.reasoning_content`；`SSE_REPLY_END.brief` 兜底
- 多轮：`SSE_ACK` 取 `conversation_id` / `section_id` 复用，首轮可压平历史
- **号池**：Cookie 账号池、会话粘性、冷却/停用、大请求预算，重启自动加载 `accounts.json`
- **多密钥分发**：`/v1/*` 校验密钥，按密钥统计 token 用量
- **每日自动签到**：号池账号定时签到领额度，结果落盘
- **SSE 心跳**：上游静默超过阈值下发 `: ping`，防中间层判超时断流
- **管理接口**：`/admin/*` 仅回环可访问（与面板同机）

### 面板（8 个页签）

| 页签 | 内容 |
|---|---|
| 概览 | 剩余额度、今日消耗、号池健康、请求数、平均延迟 |
| 账号 | 扫码加号、账号列表、启停、删除 |
| 号池 | 实时状态（在线/冷却/停用、失败次数）+ **每日签到记录** + 立即签到 |
| 密钥 | 生成 / 复制 / 删除，**分密钥 token 用量**（7 天 + 今日） |
| **模型** | 模型名、显示名、豆包 key、item_id、上下文、memory_profile、价格倍率、默认标记、**逐模型测试** |
| 请求日志 | 时间（本地时区）、模型、状态、耗时、字符、会话 |
| 测试台 | 模型下拉 + 思考强度 + 流式逐字渲染 + 可中断 + 实时统计 |
| 设置 | 网关地址、Key 状态、路径等只读配置 |

---

## 快速开始

### 1. 编译

```bash
git clone https://github.com/yyydsz/doubao2api
cd doubao2api
go build -trimpath -o doubao2api ./cmd/server
```

### 2. 配置

```bash
cp config.example.json config.json
```

最少填 `accounts[].cookie`；也可以留空，启动后在面板里扫码/导入。

```jsonc
{
  "listen": "0.0.0.0:8850",
  "api_keys": [],                         // 留空：面板「密钥」页生成写入 keys.json
  "upstream": {
    "chat_url": "https://www.doubao.com/chat/completion",
    "heartbeat_seconds": 15,
    "flatten_history": true,
    "query": { "pc_version": "2.32.6", "runtime_version": "3.42.0" }  // 活动/模型按版本放行
  },
  "accounts": [],                         // 留空：面板写入 accounts.json
  "default_model": "deepseek-v4-flash",
  "models": ["auto", "doubao-2.1-lite", "doubao-2.1-pro", "deepseek-v4-flash",
             "deepseek-v4-pro", "kimi-k3", "kimi-k3.1-pro", "kimi-k3.1-pro-fast",
             "glm-5.3", "glm-5.3-flash"],
  "model_aliases": {
    "deepseek-v4-flash": "person-deepseek-v4-1-flash-261017",
    "kimi-k3.1-pro": "person-kimi-k3-1-pro-261111"
    // 省略其余，见 config.example.json
  },
  "model_params": {                        // 按模型的上下文与元数据
    "deepseek-v4-flash": {
      "display": "DeepSeek V4.1 Flash", "item_id": "10001", "ratio": "限时 0.01x",
      "memory_profile": "300k_230", "total_window_size": "300000"
    },
    "doubao-2.1-pro": {
      "display": "豆包 2.1 Pro", "item_id": "5", "ratio": "0.65x",
      "memory_profile": "256k", "total_window_size": "256000"
    }
  },
  "work_mode": { "enabled": true, "bot_id": "7338286299411103781", "mode_id": "3", "reasoning_effort": "4" },
  "checkin": { "enabled": true, "hour": 9, "scene": "pc_incentive_task_mvp_v3" }
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

打开 `http://127.0.0.1:8860` → 「密钥」页生成 key → 「模型」页看清单 → 「测试台」直接对话。

### 5. 客户端配置

```bash
export OPENAI_BASE_URL=http://127.0.0.1:8850/v1
export OPENAI_API_KEY=sk-xxxxxxxx        # 面板「密钥」页生成
```

```python
from openai import OpenAI
c = OpenAI(base_url="http://127.0.0.1:8850/v1", api_key="sk-xxxxxxxx")
print(c.chat.completions.create(model="kimi-k3.1-pro", messages=[{"role":"user","content":"你好"}]).choices[0].message.content)
```

---

## 接口

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/v1/chat/completions` | OpenAI 兼容对话（流式 / 非流式） |
| GET | `/v1/models` | 模型清单（含 `context_length`） |
| GET | `/healthz` | 健康检查 |
| GET | `/admin/status` | 号池状态（回环可免密钥） |
| GET | `/admin/models` | 模型管理数据（key / item_id / 窗口 / 倍率） |
| GET | `/admin/accounts` | 号池账号列表 |
| POST | `/admin/reload` | 从 `accounts.json` 热加载 |
| POST | `/admin/cookie` | 单账号 Cookie 热更新 |
| DELETE | `/admin/accounts?name=` | 移出号池 |
| POST / GET | `/admin/accounts/qr` | 扫码会话创建 / 轮询 |
| GET | `/admin/quota` | 剩余额度 |
| GET / POST / DELETE | `/admin/keys` | 密钥管理 |
| GET | `/admin/usage?days=7` | 按密钥聚合用量 |
| POST / GET | `/admin/checkin` | 立即签到 / 签到状态 |

> - `/admin/*` 仅允许**回环地址**访问，面板与网关同机即可，无需额外令牌
> - `/v1/*` 必须携带有效密钥；未创建任何密钥时返回明确提示（不放行）
> - 把面板端口当网关端口调 `/v1/*` 时，面板会返回「请改用 8850」的提示

---

## 工作模式可用模型（逐个实测）

豆包工作模式：`agent_mode=1`、`mode_id=3`、`mode_name=工作任务`。

| 对外模型名 | 豆包 model_item_key | item_id | 上下文 | memory_profile | 倍率 | 实测 |
|---|---|---|---|---|---|---|
| `auto` | `9` | 9 | 256,000 | 256k | 免费 | ✅ |
| `doubao-2.1-lite` | `seed-lite-7b` | 10001 | 256,000 | 256k | 限时 0.01x | ✅ |
| `doubao-2.1-pro` | `5` | 5 | 256,000 | 256k | 0.65x | ✅ |
| `deepseek-v4-flash` | `person-deepseek-v4-1-flash-261017` | 10001 | 300,000 | 300k_230 | 限时 0.01x | ✅（默认） |
| `deepseek-v4-pro` | `person-deepseek-v4-pro-ga-260813` | 10001 | 300,000 | 300k_230 | 0.46x | ✅ |
| `kimi-k3` | `person-kimi-k3-261111` | 10001 | 300,000 | 300k_230 | 1.48x | ✅ |
| `kimi-k3.1-pro` | `person-kimi-k3-1-pro-261111` | 10001 | 300,000 | 300k_230 | 0.95x | ✅ |
| `kimi-k3.1-pro-fast` | `person-kimi-k3-1-pro-highspeed-261111` | 10001 | 300,000 | 300k_230 | 1.44x | ✅ |
| `glm-5.3` | `person-glm-5-3-261111` | 10001 | 300,000 | 300k_230 | 0.80x | ⚠️ 上游错误话术 |
| `glm-5.3-flash` | `person-glm-5-3-flash-261111` | 10001 | 300,000 | 300k_230 | 0.08x | ⚠️ 上游错误话术 |

- 兼容别名：`doubao`、`deepseek` → `person-deepseek-v4-1-flash-261017`
- **300k 是当前档位上限**，没有更大窗口可开（豆包 2.1 系列仅 256k）
- 清单来源：客户端缓存 `_DesktopActionBarBriefListRequestCacheDataV1`（`modeSelectData.menuConfV2.mode_list`），非接口下发；豆包调整模型时同步更新 `model_aliases` 即可
- GLM 两个模型返回「出了点问题，请稍后重试。」，疑似账号档位未开通

---

## 协议要点（实测）

### 工作模式字段

| 字段 | 值 |
|---|---|
| `option.agent_mode` | `1` |
| `option.conversation_mode` | `2` |
| `option.mode_id` | `"3"` |
| `option.model_config.model_item_key` / `aggregate_params.model_item_key` / `conversation_init_ext.model_item_key` | 三处需一致 |
| `option.sse_recv_event_options.support_chunk_delta` | `true`（开 `CHUNK_DELTA`） |
| `option.conversation_init_ext.reasoning_effort` | `"0"`–`"4"` |

### 响应事件顺序

```
SSE_HEARTBEAT     {}                                  心跳
SSE_ACK           conversation_id / section_id        建会话回执
FULL_MSG_NOTIFY   用户消息入库回执
STREAM_MSG_NOTIFY 首帧壳（agent_id / model_id）
STREAM_CHUNK      patch_op[]（10000 正文 / 10040 思考 / 111 tts）
CHUNK_DELTA       {"text": "..."}                     正文增量（优先）
SSE_REPLY_END     end_type=1 时 msg_finish_attr.brief 完整答案（兜底）
```

### 额度查询

```
POST https://www.doubao.com/alice/commerce/sale/subscription/quota/summary/
→ data.*.window_limits[].{total_amount, used_amount, used_percent}
```

注意 `total_amount` 是**带千分位的字符串**（如 `"1,000"`），解析前要剔除逗号。

### 每日签到

```
POST https://www.doubao.com/alice/ug/campaign/landing/checkin
body: {"scene": "pc_incentive_task_mvp_v3"}
→ {"data":{"checked_in":true,"continuous_days":2,"idempotent":false,
           "reward_amount":200,"reward_code":"quota_grant"}}
```

- `biz_params` 若携带必须是**对象**，传字符串会 400
- 活动按客户端版本放行，`pc_version` 需与当前客户端一致（实测 2.32.6 / runtime 3.42.0）
- `idempotent: true` 表示当天已签，奖励不会重复发放

---

## 已知限制

1. **扫码登录**：Passport `get_qrcode` 在客户端侧带 `sign` / `a_bogus` / `x-tt-passport-aid-sign` 三层签名；内置参数能出码，但 App 确认环节仍可能失败——**推荐「抓包 Cookie 导入」**。
2. **token 用量为估算**：豆包 SSE 不返回 usage，按输出字符 ÷1.7 折算。
3. 工作模式会触发本机 sandbox，headless 场景需 `work_mode.enable_sandbox=false`。
4. 仅支持文本消息，附件/图片未实现。
5. GLM 5.3 两个模型暂不可用（上游错误话术）。
6. 本项目为协议研究用途，账号与额度风险自负。

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
internal/config/     配置加载（账号 / 工作模式 / 模型参数 / 签到 / QR / 池参数）
internal/doubao/     请求体构造 + SSE 解析
internal/pool/       号池（粘性 / 冷却 / 停用 / 大请求预算）
internal/server/     HTTP 层：协议转换、调度、密钥、额度、签到、模型、扫码
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
