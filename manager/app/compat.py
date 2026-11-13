"""workbuddy 面板兼容层：复用它的前端构建产物（webapp/），后端换成豆包数据源。

设计：
  - 前端原样使用 workbuddy 的 web/out（React 静态构建），不改一行；
  - 后端实现它调用的接口，能落到豆包数据的就真接，接不了的返回空结构（不让页面崩）；
  - 账号 = doubao2api 的 Cookie 账号；日志/统计 = 面板侧 SQLite。
"""
import json
import os
import time

from fastapi import FastAPI, Query, Request
from fastapi.responses import FileResponse, JSONResponse, StreamingResponse
from fastapi.staticfiles import StaticFiles

from . import core

HERE = os.path.dirname(os.path.abspath(__file__))
_cand = [os.path.join(os.path.dirname(HERE), "webapp"), os.path.join(HERE, "webapp")]
WEBAPP = next((d for d in _cand if os.path.isdir(d)), _cand[0])

app = FastAPI(title="doubao-manager (workbuddy UI)")
PANEL_AUTH = os.environ.get("PANEL_AUTH", "")


# ---------------- 通用响应 ----------------
def ok(**kw):
    d = {"ok": True, "code": 0, "success": True}
    d.update(kw)
    return d


def empty_list(**kw):
    d = {"items": [], "list": [], "total": 0, "accounts": [], "logs": [], "data": []}
    d.update(kw)
    return d


# ---------------- 鉴权（对齐面板：单用户） ----------------
@app.get("/api/me")
def me():
    return ok(user={"username": "admin", "role": "admin", "is_admin": True},
              username="admin", role="admin", is_admin=True)


@app.post("/api/login")
async def login(req: Request):
    return ok(message="已登录")


@app.post("/api/logout")
def logout():
    return ok()


# ---------------- 账号 ----------------
@app.get("/api/accounts")
def accounts():
    conn = core.db()
    rows = conn.execute("SELECT name,device_id,web_id,note,enabled,created_at FROM accounts").fetchall()
    conn.close()
    pool = core.gw_json("GET", "/admin/accounts")
    state = {a["name"]: a for a in (pool.get("accounts") or [])}
    out = []
    for r in rows:
        st = state.get(r["name"], {})
        out.append({
            "uid": r["name"], "nickname": r["name"], "remark": r["note"] or "",
            "realm": "cn", "status": "ok" if r["enabled"] and not st.get("disabled") else "disabled",
            "status_text": "在线" if r["enabled"] and not st.get("disabled") else "已停用",
            "enabled": bool(r["enabled"]), "credits": 0, "in_flight": st.get("fails", 0),
            "cooling": bool(st.get("cooling")), "device_id": r["device_id"],
            "created_at": r["created_at"], "source": "panel",
        })
    for name, st in state.items():
        if any(x["uid"] == name for x in out):
            continue
        out.append({"uid": name, "nickname": name, "remark": "扫码/外部导入",
                    "realm": "cn", "status": "ok" if not st.get("disabled") else "disabled",
                    "status_text": "在线" if not st.get("disabled") else "已停用",
                    "enabled": not st.get("disabled"), "credits": 0,
                    "in_flight": st.get("fails", 0), "cooling": bool(st.get("cooling")),
                    "device_id": st.get("device_id", ""), "created_at": "", "source": "gateway"})
    return ok(accounts=out, items=out, total=len(out),
              counts={"total": len(out), "healthy": pool.get("healthy", 0)})


@app.post("/api/accounts")
async def add_account(req: Request):
    body = await req.json()
    name = (body.get("uid") or body.get("name") or "").strip()
    cookie = (body.get("cookie") or "").strip()
    if not name or not cookie:
        return JSONResponse({"ok": False, "error": "uid/cookie 必填"}, 400)
    conn = core.db()
    conn.execute(
        "INSERT INTO accounts(name,cookie,device_id,web_id,note,enabled) VALUES(?,?,?,?,?,1) "
        "ON CONFLICT(name) DO UPDATE SET cookie=excluded.cookie,device_id=excluded.device_id,"
        "web_id=excluded.web_id,note=excluded.note,enabled=1",
        (name, cookie, body.get("device_id", ""), body.get("web_id", ""), body.get("remark", "")),
    )
    conn.commit(); conn.close()
    core.sync_accounts()
    core.gw_json("POST", "/admin/cookie", {"name": name, "cookie": cookie})
    return ok(uid=name)


@app.delete("/api/accounts/{uid}")
def del_account(uid: str):
    conn = core.db(); conn.execute("DELETE FROM accounts WHERE name=?", (uid,)); conn.commit(); conn.close()
    core.gw_json("DELETE", "/admin/accounts?name=" + uid)
    core.sync_accounts()
    return ok()


@app.get("/api/account-risk")
def account_risk():
    return ok(items=[], total=0)


# ---------------- 模型 ----------------
@app.get("/api/models")
def models():
    d = core.gw_json("GET", "/v1/models") if core.GW_KEY else {}
    items = [{"id": m.get("id"), "name": m.get("id"), "model": m.get("id"),
              "context": 300000, "credits": 1} for m in (d.get("data") or [])]
    return ok(items=items, models=items, total=len(items))


@app.get("/api/model-catalog")
def model_catalog():
    return models()


# ---------------- 日志 ----------------
@app.get("/api/logs")
def logs(page: int = Query(1), size: int = Query(20)):
    conn = core.db()
    total = conn.execute("SELECT COUNT(*) c FROM req_logs").fetchone()["c"]
    rows = conn.execute(
        "SELECT ts,model,status,latency_ms,chars,conv,account,error FROM req_logs "
        "ORDER BY id DESC LIMIT ? OFFSET ?", (size, (page - 1) * size)).fetchall()
    conn.close()
    items = [{"id": i, "time": r["ts"], "ts": r["ts"], "model": r["model"], "status": r["status"],
              "status_code": r["status"], "latency_ms": r["latency_ms"], "duration_ms": r["latency_ms"],
              "tokens": r["chars"], "completion_tokens": r["chars"], "prompt_tokens": 0,
              "account": r["account"], "conversation_id": r["conv"], "error": r["error"],
              "key_name": "default", "ip": "-"} for i, r in enumerate(rows)]
    return ok(items=items, logs=items, total=total, page=page, size=size)


@app.post("/api/logs/clear")
def logs_clear():
    conn = core.db(); conn.execute("DELETE FROM req_logs"); conn.commit(); conn.close()
    return ok()


@app.get("/api/checkin-logs")
def checkin_logs():
    return empty_list()


@app.get("/api/audit-logs")
def audit_logs():
    return empty_list()


# ---------------- 统计 ----------------
@app.get("/api/stats")
@app.get("/api/stats/summary")
def stats():
    conn = core.db()
    today = conn.execute(
        "SELECT COUNT(*) c, COALESCE(AVG(latency_ms),0) a FROM req_logs WHERE date(ts)=date('now')"
    ).fetchone()
    allt = conn.execute("SELECT COUNT(*) c FROM req_logs").fetchone()
    conn.close()
    pool = core.gw_json("GET", "/status")
    summary = {"requests_today": today["c"], "requests_total": allt["c"],
               "avg_latency_ms": int(today["a"] or 0),
               "accounts_total": pool.get("total", 0), "accounts_healthy": pool.get("healthy", 0),
               "tokens_today": 0, "credits_today": 0}
    return ok(summary=summary, **summary, series=[], items=[])


# ---------------- 密钥 / 代理 / 其它（占位，保证页面不崩） ----------------
@app.get("/api/keys")
def keys():
    return empty_list()


@app.get("/api/proxies")
def proxies():
    return empty_list()


@app.get("/api/proxy-routes")
def proxy_routes():
    return empty_list()


@app.get("/api/accounts/admin-api-status")
def api_status():
    return ok(status="ok", upstream=core.GW_BASE)


@app.get("/api/accounts/task-scheduler")
def task_scheduler():
    return ok(items=[], running=False)


# ---------------- 测试台 ----------------
@app.post("/api/playground/chat")
async def playground(req: Request):
    body = await req.json()
    payload = {
        "model": body.get("model") or "doubao",
        "stream": bool(body.get("stream", True)),
        "metadata": {"conversation_id": body.get("conversation_id") or "playground"},
        "messages": body.get("messages") or [{"role": "user", "content": body.get("message") or "你好"}],
    }
    started = time.time()
    try:
        resp = core.gw("POST", "/v1/chat/completions", payload, timeout=600)
    except Exception as e:  # noqa: BLE001
        core.log_request(model=payload["model"], status=0, error=str(e)[:200])
        return JSONResponse({"ok": False, "error": str(e)}, 502)

    if not payload["stream"]:
        data = json.loads(resp.read().decode("utf-8", "ignore"))
        text = (data.get("choices") or [{}])[0].get("message", {}).get("content", "")
        core.log_request(model=payload["model"], status=200, chars=len(text or ""),
                         latency_ms=int((time.time() - started) * 1000))
        return ok(content=text, data=data)

    def gen():
        n = 0
        try:
            for line in resp:
                s = line.decode("utf-8", "ignore")
                if s.startswith("data: ") and s.strip() != "data: [DONE]":
                    try:
                        d = json.loads(s[6:])
                        n += len(((d.get("choices") or [{}])[0].get("delta") or {}).get("content") or "")
                    except Exception:  # noqa: BLE001
                        pass
                yield s
        finally:
            core.log_request(model=payload["model"], status=200, chars=n,
                             latency_ms=int((time.time() - started) * 1000))

    return StreamingResponse(gen(), media_type="text/event-stream")


# ---------------- 静态前端（最后挂载，兜住所有页面路由） ----------------
@app.get("/healthz")
def healthz():
    return {"ok": True}


if os.path.isdir(WEBAPP):
    app.mount("/", StaticFiles(directory=WEBAPP, html=True), name="web")
