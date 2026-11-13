"""doubao-manager：豆包号池管理面板（FastAPI 版）。

架构对齐 workbuddy 面板：
  main.py        ← 应用装配（等价 server/main.py）
  core.py        ← 数据库 / 网关客户端 / 同步逻辑（等价 services/）
  web/index.html ← 前端静态页（等价 web/out）
"""
import base64
import os

from fastapi import FastAPI, HTTPException, Query
from fastapi.responses import FileResponse, JSONResponse, StreamingResponse
from pydantic import BaseModel

from . import core

app = FastAPI(title="doubao-manager", version="1.0")
PANEL_AUTH = os.environ.get("PANEL_AUTH", "")  # "user:pass"，空=不鉴权


@app.middleware("http")
async def basic_auth(request, call_next):
    if PANEL_AUTH:
        h = request.headers.get("authorization", "")
        ok = False
        if h.startswith("Basic "):
            try:
                ok = base64.b64decode(h[6:]).decode() == PANEL_AUTH
            except Exception:
                ok = False
        if not ok:
            from fastapi.responses import Response
            return Response(status_code=401, headers={"WWW-Authenticate": 'Basic realm="doubao-manager"'})
    return await call_next(request)
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
    st = core.gw_json("GET", "/status")
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
    # 账号数以网关号池为准（含抓包/扫码导入、不在面板库里的号）
    gw_total = 0 if "error" in st else st.get("total", 0)
    return {
        "gateway": st,
        "accounts_enabled": max(n_acc, gw_total),
        "quota": quota,
        "today_chars": tokens_today,
        "today_tokens_est": int((tokens_today or 0) / 1.7),
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
