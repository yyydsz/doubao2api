"""doubao-manager：豆包号池管理面板（FastAPI 版）。

架构对齐 workbuddy 面板：
  main.py        ← 应用装配（等价 server/main.py）
  core.py        ← 数据库 / 网关客户端 / 同步逻辑（等价 services/）
  web/index.html ← 前端静态页（等价 web/out）
"""
import base64
import hashlib
import hmac
import json
import os
import time
import urllib.parse

from fastapi import FastAPI, HTTPException, Query, Request
from fastapi.responses import FileResponse, HTMLResponse, JSONResponse, RedirectResponse, StreamingResponse
from pydantic import BaseModel

from . import core

app = FastAPI(title="doubao-manager", version="1.0")
PANEL_AUTH = os.environ.get("PANEL_AUTH", "")  # "user:pass"，空=不鉴权


SESSION_COOKIE = "ds_panel_sess"
SESSION_SECRET = (os.environ.get("PANEL_SECRET") or PANEL_AUTH or "doubao-panel").encode()


def _sign(val: str) -> str:
    """对 "exp|user" 做 HMAC 签名，返回 cookie 值。"""
    msg = val.encode()
    mac = hmac.new(SESSION_SECRET, msg, hashlib.sha256).hexdigest()[:32]
    return base64.urlsafe_b64encode(msg).decode() + "." + mac


def _verify(raw: str) -> str | None:
    """校验 cookie，返回用户名；失败返回 None。"""
    try:
        b64, mac = raw.split(".", 1)
        msg = base64.urlsafe_b64decode(b64.encode()).decode()
        want = hmac.new(SESSION_SECRET, msg.encode(), hashlib.sha256).hexdigest()[:32]
        if not hmac.compare_digest(mac, want):
            return None
        exp_s, user = msg.split("|", 1)
        if int(exp_s) < int(time.time()):
            return None
        return user
    except Exception:
        return None


def _current_user(request) -> str | None:
    raw = request.cookies.get(SESSION_COOKIE, "")
    return _verify(raw) if raw else None


LOGIN_HTML = """<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>登录 · 豆包管理器</title>
<script>
(function(){try{var t=localStorage.getItem('doubao_theme');if(!t)t=matchMedia('(prefers-color-scheme:dark)').matches?'dark':'light';document.documentElement.setAttribute('data-theme',t);}catch(e){}})();
</script>
<style>
:root{--bg:#f6f7f9;--card:#ffffff;--border:#e4e7ec;--border2:#d0d5dd;--fg:#101828;--fg2:#667085;--accent:#10b981;--accent-h:#059669;--ring:rgba(16,185,129,.22);--bad:#dc2626;--bad-bg:rgba(220,38,38,.09);--shadow:0 12px 36px rgba(16,24,40,.08)}
[data-theme="dark"]{--bg:#0a0d12;--card:#11161f;--border:#222b38;--border2:#313e52;--fg:#f0f4f8;--fg2:#8b98ac;--accent:#10b981;--accent-h:#34d399;--ring:rgba(16,185,129,.28);--bad:#f87171;--bad-bg:rgba(248,113,113,.14);--shadow:0 16px 48px rgba(0,0,0,.48)}
*{box-sizing:border-box}body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:20px;
background:var(--bg);color:var(--fg);font:14px/1.6 -apple-system,BlinkMacSystemFont,"Inter","PingFang SC","Segoe UI",sans-serif;-webkit-font-smoothing:antialiased}
.theme-btn{position:fixed;top:18px;right:18px;width:36px;height:36px;border-radius:10px;border:1px solid var(--border);background:var(--card);color:var(--fg2);cursor:pointer;display:grid;place-items:center}
.theme-btn:hover{color:var(--fg);border-color:var(--border2)}
.box{width:100%;max-width:368px;background:var(--card);border:1px solid var(--border);border-radius:16px;padding:30px 28px;box-shadow:var(--shadow)}
.head{display:flex;align-items:center;gap:12px;margin-bottom:22px}
.logo{width:38px;height:38px;border-radius:10px;background:linear-gradient(135deg,#10b981,#059669);color:#fff;display:grid;place-items:center;box-shadow:0 4px 12px rgba(16,185,129,.3);flex:none}
h1{margin:0;font-size:17px;font-weight:700;letter-spacing:-.01em}p.sub{margin:2px 0 0;color:var(--fg2);font-size:12.5px}
label{display:block;font-size:12.5px;font-weight:500;color:var(--fg2);margin:14px 0 6px}
.pw-wrap{position:relative}
input{width:100%;background:var(--bg);border:1px solid var(--border2);color:var(--fg);border-radius:9px;padding:10px 12px;font:inherit;transition:border-color .15s,box-shadow .15s}
.pw-wrap input{padding-right:40px}
input:focus{outline:none;border-color:var(--accent);box-shadow:0 0 0 3px var(--ring)}
.pw-eye{position:absolute;right:8px;top:50%;transform:translateY(-50%);border:0;background:transparent;color:var(--fg2);cursor:pointer;padding:4px;font-size:12px}
button.submit{width:100%;margin-top:20px;background:var(--accent);color:#fff;border:0;border-radius:9px;
padding:11px;font-weight:600;font-size:14px;cursor:pointer;transition:background .15s}button.submit:hover{background:var(--accent-h)}
.err{margin-top:14px;font-size:12.5px;color:var(--bad);background:var(--bad-bg);border-radius:8px;padding:8px 11px}
.err:empty{display:none}
</style></head><body>
<button class="theme-btn" type="button" onclick="toggleTheme()" title="切换主题" aria-label="切换主题">◐</button>
<form class="box" method="post" action="/login">
  <div class="head">
    <div class="logo"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2L2 7l10 5 10-5-10-5z"/><path d="M2 17l10 5 10-5"/><path d="M2 12l10 5 10-5"/></svg></div>
    <div><h1>豆包管理器</h1><p class="sub">doubao2api 号池控制台</p></div>
  </div>
  <label>用户名</label><input name="username" autocomplete="username" placeholder="请输入用户名" autofocus required>
  <label>密码</label><div class="pw-wrap"><input id="pw" name="password" type="password" autocomplete="current-password" placeholder="请输入密码" required><button class="pw-eye" type="button" onclick="var i=document.getElementById('pw');i.type=i.type==='password'?'text':'password';this.textContent=i.type==='password'?'显示':'隐藏'">显示</button></div>
  <button class="submit" type="submit">登 录</button>
  <div class="err">__ERR__</div>
</form>
<script>function toggleTheme(){var n=document.documentElement.getAttribute('data-theme')==='dark'?'light':'dark';document.documentElement.setAttribute('data-theme',n);try{localStorage.setItem('doubao_theme',n)}catch(e){}}</script>
</body></html>"""


@app.middleware("http")
async def session_auth(request, call_next):
    path = request.url.path
    # 放行：登录页/登出/健康检查/网关接口
    if not PANEL_AUTH or path in ("/login", "/logout", "/healthz") or path.startswith("/v1/"):
        return await call_next(request)
    if _current_user(request):
        return await call_next(request)
    # 未登录：接口返回 401 JSON，页面跳登录
    if path.startswith("/api/"):
        return JSONResponse({"error": {"code": "unauthorized", "message": "请先登录"}}, status_code=401)
    return RedirectResponse("/login", status_code=302)


@app.get("/login")
def login_page(request: Request):
    if _current_user(request):
        return RedirectResponse("/", status_code=302)
    return HTMLResponse(LOGIN_HTML.replace("__ERR__", ""))


@app.post("/login")
async def login_submit(request: Request):
    raw = (await request.body()).decode("utf-8", "ignore")
    form = dict(urllib.parse.parse_qsl(raw, keep_blank_values=True))
    username = (form.get("username") or "").strip()
    password = form.get("password") or ""
    want_user, _, want_pass = PANEL_AUTH.partition(":")
    if username == want_user and hmac.compare_digest(password, want_pass):
        exp = int(time.time()) + 7 * 24 * 3600
        resp = RedirectResponse("/", status_code=302)
        resp.set_cookie(SESSION_COOKIE, _sign(f"{exp}|{username}"), max_age=7 * 24 * 3600,
                        httponly=True, samesite="lax")
        return resp
    return HTMLResponse(LOGIN_HTML.replace("__ERR__", "用户名或密码不正确"), status_code=401)


@app.get("/logout")
def logout():
    resp = RedirectResponse("/login", status_code=302)
    resp.delete_cookie(SESSION_COOKIE)
    return resp


WEB_DIR = os.path.join(os.path.dirname(__file__), "web")


class AccountIn(BaseModel):
    name: str
    cookie: str
    device_id: str = ""
    web_id: str = ""
    note: str = ""


class TestIn(BaseModel):
    model: str = "doubao"
    message: str = "你好"
    conversation_id: str = "panel-test"
    reasoning_effort: str = ""


# ---------------- 概览 ----------------
@app.get("/api/overview")
def overview():
    st = core.gw_json("GET", "/admin/status")
    conn = core.db()
    n_acc = conn.execute("SELECT COUNT(*) c FROM accounts WHERE enabled=1").fetchone()["c"]
    today = conn.execute(
        "SELECT COUNT(*) c, COALESCE(AVG(latency_ms),0) a FROM req_logs WHERE date(ts)=date('now')"
    ).fetchone()
    tokens_today = conn.execute(
        "SELECT COALESCE(SUM(chars),0) c FROM req_logs WHERE date(ts)=date('now')"
    ).fetchone()["c"]
    conn.close()
    quota = core.gw_json("GET", "/admin/quota")
    gw_usage = core.gw_json("GET", "/admin/usage?days=7")
    u_today_req = sum(u.get("today_requests", 0) for u in (gw_usage.get("usage") or []))
    u_today_tok = sum(u.get("today_tokens", 0) for u in (gw_usage.get("usage") or []))
    u_tok = sum(u.get("tokens", 0) for u in (gw_usage.get("usage") or []))
    # 账号数以网关号池为准（含抓包/扫码导入、不在面板库里的号）
    gw_total = 0 if "error" in st else st.get("total", 0)
    return {
        "gateway": st,
        "accounts_enabled": max(n_acc, gw_total),
        "quota": quota,
        "today_chars": tokens_today,
        "today_tokens_est": u_today_tok,
        "usage_today_requests": u_today_req,
        "usage_tokens_7d": u_tok,
        "requests_today": today["c"],
        "avg_latency_ms": int(today["a"] or 0),
    }


# ---------------- 账号 ----------------
@app.get("/api/accounts")
def list_accounts():
    conn = core.db()
    rows = conn.execute(
        "SELECT name,device_id,web_id,note,enabled,created_at FROM accounts ORDER BY created_at"
    ).fetchall()
    conn.close()
    items = [dict(r) for r in rows]
    known = {i["name"] for i in items}
    # 合并网关里由扫码等方式直接加入、但面板库还没有的账号
    pool = core.gw_json("GET", "/admin/accounts")
    for a in (pool.get("accounts") or []):
        if a.get("name") and a["name"] not in known:
            items.append({
                "name": a["name"], "device_id": a.get("device_id", ""), "web_id": "",
                "note": "扫码加入", "enabled": 0 if a.get("disabled") else 1,
                "created_at": "", "source": "gateway",
            })
    return {"accounts": items}


@app.post("/api/accounts")
def upsert_account(a: AccountIn):
    if not a.name.strip() or not a.cookie.strip():
        raise HTTPException(400, "name/cookie 必填")
    conn = core.db()
    conn.execute(
        "INSERT INTO accounts(name,cookie,device_id,web_id,note,enabled) VALUES(?,?,?,?,?,1) "
        "ON CONFLICT(name) DO UPDATE SET cookie=excluded.cookie,device_id=excluded.device_id,"
        "web_id=excluded.web_id,note=excluded.note,enabled=1",
        (a.name.strip(), a.cookie.strip(), a.device_id, a.web_id, a.note),
    )
    conn.commit()
    conn.close()
    reload_res = core.sync_accounts()
    core.gw_json("POST", "/admin/cookie", {"name": a.name, "cookie": a.cookie})
    return {"ok": True, "reload": reload_res}


@app.delete("/api/accounts/{name}")
def delete_account(name: str):
    conn = core.db()
    conn.execute("DELETE FROM accounts WHERE name=?", (name,))
    conn.commit()
    conn.close()
    core.gw_json("DELETE", "/admin/accounts?name=" + name)
    return {"ok": True, "reload": core.sync_accounts()}


@app.post("/api/accounts/{name}/toggle")
def toggle_account(name: str):
    conn = core.db()
    row = conn.execute("SELECT enabled FROM accounts WHERE name=?", (name,)).fetchone()
    if not row:
        conn.close()
        raise HTTPException(404, "账号不存在")
    new = 0 if row["enabled"] else 1
    conn.execute("UPDATE accounts SET enabled=? WHERE name=?", (new, name))
    conn.commit()
    conn.close()
    return {"ok": True, "enabled": new, "reload": core.sync_accounts()}


# ---------------- 模型 ----------------
@app.get("/api/models")
def models():
    d = core.gw_json("GET", "/v1/models")
    items = [{"id": m.get("id")} for m in (d.get("data") or [])]
    return {"models": items, "items": items}


# ---------------- 密钥管理 ----------------
class KeyIn(BaseModel):
    name: str = ""


@app.get("/api/keys")
def list_keys():
    return core.gw_json("GET", "/admin/keys")


@app.post("/api/keys")
def create_key(k: KeyIn):
    return core.gw_json("POST", "/admin/keys", {"name": k.name})


@app.delete("/api/keys/{kid}")
def delete_key(kid: str):
    return core.gw_json("DELETE", "/admin/keys?id=" + kid)


@app.get("/api/usage")
def usage(days: int = 7):
    return core.gw_json("GET", f"/admin/usage?days={days}")


# ---------------- 模型管理 ----------------
@app.get("/api/models/admin")
def models_admin():
    return core.gw_json("GET", "/admin/models")


class ModelTestIn(BaseModel):
    model: str = ""


@app.post("/api/models/test")
def models_test(t: ModelTestIn):
    started = time.time()
    payload = {"model": t.model, "metadata": {"conversation_id": "model-test-" + t.model},
               "messages": [{"role": "user", "content": "只回一个字：好"}]}
    try:
        resp = core.gw("POST", "/v1/chat/completions", payload, timeout=120)
        data = json.loads(resp.read().decode("utf-8", "ignore"))
        text = ((data.get("choices") or [{}])[0].get("message") or {}).get("content", "")
        # 上游偶发用正常 content 返回错误话术，这里识别成失败
        bad = any(k in (text or "") for k in ("出了点问题", "稍后重试", "系统繁忙"))
        return {"ok": not bad, "reply": text, "latency_ms": int((time.time() - started) * 1000),
                "error": "上游返回错误话术" if bad else ""}
    except Exception as e:  # noqa: BLE001
        return {"ok": False, "error": str(e)[:200], "latency_ms": int((time.time() - started) * 1000)}


# ---------------- 每日签到 ----------------
@app.post("/api/checkin")
def run_checkin(name: str = ""):
    return core.gw_json("POST", "/admin/checkin" + (f"?name={name}" if name else ""))


@app.get("/api/checkin")
def checkin_status():
    return core.gw_json("GET", "/admin/checkin")


# ---------------- 号池 / 日志 / 设置 ----------------
@app.get("/api/pool")
def pool():
    return core.gw_json("GET", "/admin/accounts")


@app.get("/api/logs")
def logs(limit: int = Query(50, ge=1, le=500)):
    conn = core.db()
    rows = conn.execute(
        "SELECT ts,model,status,latency_ms,chars,conv,account,error FROM req_logs ORDER BY id DESC LIMIT ?",
        (limit,),
    ).fetchall()
    conn.close()
    return {"logs": [dict(r) for r in rows]}


@app.get("/api/settings")
def settings():
    return {
        "gw_base": core.GW_BASE,
        "gw_key_set": bool(core.GW_KEY),
        "accounts_file": core.ACCOUNTS_FILE,
        "db_path": core.DB_PATH,
    }


# ---------------- 扫码 ----------------
@app.post("/api/qr")
def qr_start():
    return core.gw_json("POST", "/admin/accounts/qr")


@app.get("/api/qr/{qid}")
def qr_poll(qid: str):
    return core.gw_json("GET", f"/admin/accounts/qr?id={qid}")


# ---------------- 聊天测试（SSE 透传 + 落日志）----------------
@app.post("/api/test")
def test_chat(t: TestIn):
    import json
    import time

    payload = {
        "model": t.model,
        "stream": True,
        "metadata": {"conversation_id": t.conversation_id},
        "messages": [{"role": "user", "content": t.message}],
    }
    if t.reasoning_effort:
        payload["reasoning_effort"] = t.reasoning_effort
    started = time.time()
    chars = {"n": 0}

    def gen():
        try:
            resp = core.gw("POST", "/v1/chat/completions", payload, timeout=600)
        except Exception as e:  # noqa: BLE001
            core.log_request(model=t.model, status=0, conv=t.conversation_id, error=str(e)[:200])
            yield f"data: {{\"error\":\"{e}\"}}\n\n"
            return
        try:
            for line in resp:
                s = line.decode("utf-8", "ignore")
                if s.startswith("data: ") and '"content"' in s:
                    try:
                        d = json.loads(s[6:])
                        chars["n"] += len(d["choices"][0]["delta"].get("content") or "")
                    except Exception:  # noqa: BLE001
                        pass
                yield s
        finally:
            core.log_request(
                model=t.model,
                status=200,
                latency_ms=int((time.time() - started) * 1000),
                chars=chars["n"],
                conv=t.conversation_id,
            )

    return StreamingResponse(gen(), media_type="text/event-stream")


# ---------------- 前端 ----------------
@app.get("/")
def index():
    return FileResponse(os.path.join(WEB_DIR, "index.html"))


@app.get("/healthz")
def healthz():
    return JSONResponse({"ok": True})


@app.api_route("/v1/{path:path}", methods=["GET", "POST", "OPTIONS"])
def v1_on_panel(path: str):
    """常见误用：把面板端口当网关端口。给出明确指引而不是 401/404。"""
    gw = core.GW_BASE.replace("127.0.0.1", "").rstrip("/")
    return JSONResponse(
        {
            "error": {
                "message": "这里是管理面板端口（8860），模型接口在网关端口 8850",
                "type": "wrong_port",
                "code": "use_gateway_port",
                "panel_port": 8860,
                "gateway_base": "http://<host>:8850/v1",
                "hint": "把 OPENAI_BASE_URL 改成 http://<host>:8850/v1，密钥用面板「密钥」页生成的那把",
            }
        },
        status_code=404,
    )
