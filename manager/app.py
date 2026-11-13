#!/usr/bin/env python3
"""doubao-manager：豆包号池管理面板（标准库实现，零依赖）。

对接 doubao2api：
  - 账号 CRUD  → 写 accounts.json → 调网关 /admin/reload 热加载
  - 号池状态   → 网关 /admin/accounts、/status
  - 扫码加号   → 网关 /admin/accounts/qr（需网关侧配置 qr.get_url/check_url）
  - 聊天测试   → 网关 /v1/chat/completions（SSE 透传）

环境变量：
  GW_BASE       网关地址，默认 http://127.0.0.1:8850
  GW_KEY        网关下游密钥（api_keys 之一）
  ACCOUNTS_FILE 账号文件路径，默认 ../doubao2api/accounts.json
  LISTEN        监听地址，默认 127.0.0.1:8860
  DB_PATH       面板数据库，默认 ./panel.db
"""
import json
import os
import sqlite3
import sys
import urllib.error
import urllib.parse
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

GW_BASE = os.environ.get("GW_BASE", "http://127.0.0.1:8850").rstrip("/")
GW_KEY = os.environ.get("GW_KEY", "")
ACCOUNTS_FILE = os.environ.get(
    "ACCOUNTS_FILE",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "doubao2api", "accounts.json"),
)
LISTEN = os.environ.get("LISTEN", "127.0.0.1:8860")
DB_PATH = os.environ.get("DB_PATH", os.path.join(os.path.dirname(os.path.abspath(__file__)), "panel.db"))


# ---------------- 数据库 ----------------
def db():
    conn = sqlite3.connect(DB_PATH)
    conn.row_factory = sqlite3.Row
    conn.execute(
        """CREATE TABLE IF NOT EXISTS accounts(
            name TEXT PRIMARY KEY,
            cookie TEXT NOT NULL,
            device_id TEXT DEFAULT '',
            web_id TEXT DEFAULT '',
            note TEXT DEFAULT '',
            enabled INTEGER DEFAULT 1,
            created_at TEXT DEFAULT CURRENT_TIMESTAMP
        )"""
    )
    conn.commit()
    return conn


# ---------------- 网关调用 ----------------
def gw(method, path, body=None, stream=False, timeout=120):
    url = GW_BASE + path
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if GW_KEY:
        req.add_header("Authorization", "Bearer " + GW_KEY)
    return urllib.request.urlopen(req, timeout=timeout)


def gw_json(method, path, body=None):
    try:
        with gw(method, path, body) as r:
            return json.loads(r.read().decode("utf-8", "ignore"))
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read().decode("utf-8", "ignore"))
        except Exception:
            return {"error": {"message": f"网关 {e.code}"}}
    except Exception as e:  # noqa: BLE001
        return {"error": {"message": f"网关不可达: {e}"}}


def sync_accounts():
    """把面板库里的启用账号写成 accounts.json，并让网关热加载。"""
    conn = db()
    rows = conn.execute("SELECT name,cookie,device_id,web_id,note FROM accounts WHERE enabled=1").fetchall()
    conn.close()
    payload = {"accounts": [
        {"name": r["name"], "cookie": r["cookie"], "device_id": r["device_id"],
         "web_id": r["web_id"], "note": r["note"], "local_app_id": "582478"}
        for r in rows
    ]}
    os.makedirs(os.path.dirname(os.path.abspath(ACCOUNTS_FILE)), exist_ok=True)
    with open(ACCOUNTS_FILE, "w", encoding="utf-8") as f:
        json.dump(payload, f, ensure_ascii=False, indent=2)
    return gw_json("POST", "/admin/reload")


# ---------------- HTTP 服务 ----------------
INDEX = r"""<!doctype html><html lang="zh"><head><meta charset="utf-8">
<title>豆包号池管理</title>
<style>
:root{--bg:#0e1116;--card:#161b22;--line:#232b36;--fg:#e6edf3;--dim:#8b949e;--accent:#3fb950;--warn:#d29922;--bad:#f85149}
*{box-sizing:border-box}body{margin:0;font:14px/1.5 -apple-system,"Segoe UI",sans-serif;background:var(--bg);color:var(--fg)}
header{padding:14px 20px;border-bottom:1px solid var(--line);display:flex;gap:16px;align-items:center}
h1{font-size:16px;margin:0}nav a{color:var(--dim);margin-right:12px;text-decoration:none;cursor:pointer}
nav a.on{color:var(--accent)}main{padding:20px;max-width:1200px}
.card{background:var(--card);border:1px solid var(--line);border-radius:10px;padding:16px;margin-bottom:16px}
.row{display:flex;gap:10px;flex-wrap:wrap;align-items:center}
input,textarea,select{background:#0d1117;border:1px solid var(--line);color:var(--fg);border-radius:8px;padding:8px 10px;font:inherit}
textarea{width:100%;min-height:72px;font-family:ui-monospace,monospace;font-size:12px}
button{background:var(--accent);color:#05130a;border:0;border-radius:8px;padding:8px 14px;font-weight:600;cursor:pointer}
button.ghost{background:transparent;color:var(--fg);border:1px solid var(--line)}
button:disabled{opacity:.5;cursor:default}
table{width:100%;border-collapse:collapse;font-size:13px}
th,td{padding:8px 10px;text-align:left;border-bottom:1px solid var(--line);vertical-align:top}
th{color:var(--dim);font-weight:500}.pill{padding:2px 8px;border-radius:99px;font-size:12px}
.ok{background:rgba(63,185,80,.15);color:var(--accent)}.warn{background:rgba(210,153,34,.15);color:var(--warn)}
.bad{background:rgba(248,81,73,.15);color:var(--bad)}.mono{font-family:ui-monospace,monospace;font-size:12px;color:var(--dim);word-break:break-all}
pre{background:#0d1117;border:1px solid var(--line);border-radius:8px;padding:12px;max-height:340px;overflow:auto;font-size:12px;white-space:pre-wrap}
#qrbox{display:none;text-align:center}#qrbox img{width:220px;height:220px;background:#fff;padding:8px;border-radius:8px}
</style></head><body>
<header><h1>豆包号池管理</h1>
<nav><a class="on" data-tab="accounts">账号</a><a data-tab="pool">号池</a><a data-tab="test">测试</a></nav>
<span id="gw" class="pill warn">连接中…</span></header>
<main>
<section id="tab-accounts">
  <div class="card">
    <div class="row" style="margin-bottom:10px">
      <button onclick="qrStart()">扫码加号</button>
      <button class="ghost" onclick="syncAccounts()">同步到网关</button>
      <span id="msg" class="mono"></span>
    </div>
    <div id="qrbox">
      <img id="qrimg" alt="二维码"><div id="qrstatus" class="mono"></div>
      <button class="ghost" onclick="qrCancel()">取消</button>
    </div>
  </div>
  <div class="card">
    <div class="row"><input id="f-name" placeholder="账号名（唯一）" style="width:180px">
      <input id="f-device" placeholder="device_id（可选）" style="width:200px">
      <input id="f-web" placeholder="web_id（可选）" style="width:200px"></div>
    <div style="margin:10px 0"><textarea id="f-cookie" placeholder="Cookie：sessionid=...; sid_tt=...; ttwid=...; s_v_web_id=...; odin_tt=...; passport_csrf_token=..."></textarea></div>
    <div class="row"><input id="f-note" placeholder="备注" style="width:260px">
      <button onclick="addAccount()">保存账号</button></div>
  </div>
  <div class="card"><table id="tbl"><thead><tr><th>账号</th><th>device_id</th><th>备注</th><th>状态</th><th>操作</th></tr></thead><tbody></tbody></table></div>
</section>
<section id="tab-pool" style="display:none"><div class="card"><pre id="pooljson">加载中…</pre></div></section>
<section id="tab-test" style="display:none">
  <div class="card">
    <div class="row"><input id="t-model" value="deepseek" style="width:140px">
      <input id="t-conv" value="panel-test" style="width:160px">
      <button onclick="runTest()">发送</button></div>
    <div style="margin:10px 0"><textarea id="t-msg">你好，你是什么模型</textarea></div>
    <pre id="t-out"></pre>
  </div>
</section>
</main>
<script>
const $=s=>document.querySelector(s);
let qrId=null, qrTimer=null;
document.querySelectorAll('nav a').forEach(a=>a.onclick=()=>{
  document.querySelectorAll('nav a').forEach(x=>x.classList.remove('on'));a.classList.add('on');
  ['accounts','pool','test'].forEach(t=>$('#tab-'+t).style.display=(t===a.dataset.tab?'':'none'));
  if(a.dataset.tab==='pool') loadPool();
});
async function api(path,opt){const r=await fetch(path,opt);return await r.json();}
async function loadStatus(){
  const s=await api('/api/status');
  const el=$('#gw');
  if(s.error){el.className='pill bad';el.textContent='网关离线';}
  else{el.className='pill ok';el.textContent=`网关在线 · ${s.healthy}/${s.total}`;}
}
async function loadAccounts(){
  const d=await api('/api/accounts'); const tb=$('#tbl tbody'); tb.innerHTML='';
  (d.accounts||[]).forEach(a=>{
    const tr=document.createElement('tr');
    tr.innerHTML=`<td>${a.name}</td><td class="mono">${a.device_id||'-'}</td><td>${a.note||''}</td>
      <td>${a.enabled?'<span class="pill ok">启用</span>':'<span class="pill bad">停用</span>'}</td>
      <td><button class="ghost" onclick="delAccount('${a.name}')">删除</button></td>`;
    tb.appendChild(tr);
  });
}
async function addAccount(){
  const body={name:$('#f-name').value.trim(),cookie:$('#f-cookie').value.trim(),
    device_id:$('#f-device').value.trim(),web_id:$('#f-web').value.trim(),note:$('#f-note').value.trim()};
  if(!body.name||!body.cookie){$('#msg').textContent='账号名和 Cookie 必填';return;}
  const r=await api('/api/accounts',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(body)});
  $('#msg').textContent = r.ok ? `已保存并同步：${JSON.stringify(r.reload||{})}` : ('失败: '+JSON.stringify(r));
  loadAccounts(); loadStatus();
}
async function delAccount(name){
  if(!confirm('删除账号 '+name+' ?'))return;
  await api('/api/accounts?name='+encodeURIComponent(name),{method:'DELETE'});
  loadAccounts();
}
async function syncAccounts(){
  const r=await api('/api/sync',{method:'POST'}); $('#msg').textContent=JSON.stringify(r);
  loadStatus();
}
async function qrStart(){
  $('#qrbox').style.display='block'; $('#qrimg').removeAttribute('src'); $('#qrstatus').textContent='请求二维码…';
  const r=await api('/api/qr',{method:'POST'});
  if(r.error){$('#qrstatus').textContent='失败: '+(r.error.message||JSON.stringify(r));return;}
  qrId=r.id;
  if(r.image){$('#qrimg').src=r.image.startsWith('data:')?r.image:'data:image/png;base64,'+r.image;}
  else if(r.qrcode_url){$('#qrstatus').innerHTML='请用豆包 App 打开：<br>'+r.qrcode_url;}
  $('#qrstatus').textContent='等待扫码…';
  qrTimer=setInterval(qrPoll,2000);
}
async function qrPoll(){
  if(!qrId)return;
  const r=await api('/api/qr?id='+qrId);
  if(r.error){$('#qrstatus').textContent='失败: '+(r.error.message||'');return;}
  const map={waiting:'等待扫码…',scanned:'已扫码，请在手机上确认',confirmed:'登录成功，账号已加入号池',
             expired:'二维码已过期',canceled:'已取消'};
  $('#qrstatus').textContent=map[r.status]||r.status;
  if(r.status==='confirmed'){qrCancel();loadAccounts();loadStatus();}
  if(r.status==='expired'||r.status==='canceled'){qrCancel();}
}
function qrCancel(){if(qrTimer){clearInterval(qrTimer);qrTimer=null;}qrId=null;$('#qrbox').style.display='none';}
async function loadPool(){
  const d=await api('/api/pool'); $('#pooljson').textContent=JSON.stringify(d,null,2);
}
async function runTest(){
  const out=$('#t-out'); out.textContent='';
  const resp=await fetch('/api/chat',{method:'POST',headers:{'Content-Type':'application/json'},
    body:JSON.stringify({model:$('#t-model').value,conversation_id:$('#t-conv').value,message:$('#t-msg').value})});
  const rd=resp.body.getReader(), dec=new TextDecoder();
  for(;;){const {value,done}=await rd.read(); if(done)break; out.textContent+=dec.decode(value,{stream:true}); out.scrollTop=out.scrollHeight;}
}
loadStatus(); loadAccounts(); setInterval(loadStatus,10000);
</script></body></html>"""


class Handler(BaseHTTPRequestHandler):
    server_version = "doubao-manager/1.0"

    def log_message(self, fmt, *args):  # 静音默认访问日志
        sys.stderr.write("%s - %s\n" % (self.address_string(), fmt % args))

    # ---- 基础输出 ----
    def _json(self, obj, code=200):
        body = json.dumps(obj, ensure_ascii=False).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read(self):
        n = int(self.headers.get("Content-Length") or 0)
        if not n:
            return {}
        try:
            return json.loads(self.rfile.read(n).decode("utf-8"))
        except Exception:
            return {}

    # ---- 路由 ----
    def do_GET(self):
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        if u.path == "/":
            body = INDEX.encode()
            self.send_response(200)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif u.path == "/api/accounts":
            conn = db()
            rows = conn.execute("SELECT name,device_id,web_id,note,enabled,created_at FROM accounts ORDER BY created_at").fetchall()
            conn.close()
            self._json({"accounts": [dict(r) for r in rows]})
        elif u.path == "/api/status":
            self._json(gw_json("GET", "/status"))
        elif u.path == "/api/pool":
            self._json(gw_json("GET", "/admin/accounts"))
        elif u.path == "/api/qr":
            qid = (q.get("id") or [""])[0]
            self._json(gw_json("GET", "/admin/accounts/qr?id=" + urllib.parse.quote(qid)))
        else:
            self._json({"error": {"message": "not found"}}, 404)

    def do_POST(self):
        u = urllib.parse.urlparse(self.path)
        if u.path == "/api/accounts":
            b = self._read()
            name, cookie = (b.get("name") or "").strip(), (b.get("cookie") or "").strip()
            if not name or not cookie:
                return self._json({"ok": False, "error": "name/cookie 必填"}, 400)
            conn = db()
            conn.execute(
                "INSERT INTO accounts(name,cookie,device_id,web_id,note,enabled) VALUES(?,?,?,?,?,1) "
                "ON CONFLICT(name) DO UPDATE SET cookie=excluded.cookie,device_id=excluded.device_id,"
                "web_id=excluded.web_id,note=excluded.note,enabled=1",
                (name, cookie, b.get("device_id", ""), b.get("web_id", ""), b.get("note", "")),
            )
            conn.commit()
            conn.close()
            reload_res = sync_accounts()
            # 已存在的账号走 cookie 热更新
            gw_json("POST", "/admin/cookie", {"name": name, "cookie": cookie})
            self._json({"ok": True, "reload": reload_res})
        elif u.path == "/api/sync":
            self._json(sync_accounts())
        elif u.path == "/api/qr":
            self._json(gw_json("POST", "/admin/accounts/qr"))
        elif u.path == "/api/chat":
            self._chat()
        else:
            self._json({"error": {"message": "not found"}}, 404)

    def do_DELETE(self):
        u = urllib.parse.urlparse(self.path)
        q = urllib.parse.parse_qs(u.query)
        if u.path == "/api/accounts":
            name = (q.get("name") or [""])[0]
            if not name:
                return self._json({"ok": False, "error": "name 必填"}, 400)
            conn = db()
            conn.execute("DELETE FROM accounts WHERE name=?", (name,))
            conn.commit()
            conn.close()
            gw_json("DELETE", "/admin/accounts?name=" + urllib.parse.quote(name))
            self._json({"ok": True, "reload": sync_accounts()})
        else:
            self._json({"error": {"message": "not found"}}, 404)

    # ---- 聊天测试（SSE 透传）----
    def _chat(self):
        b = self._read()
        payload = {
            "model": b.get("model") or "doubao",
            "stream": True,
            "metadata": {"conversation_id": b.get("conversation_id") or "panel-test"},
            "messages": [{"role": "user", "content": b.get("message") or "你好"}],
        }
        try:
            resp = gw("POST", "/v1/chat/completions", payload, timeout=300)
        except urllib.error.HTTPError as e:
            return self._json({"error": {"message": e.read().decode("utf-8", "ignore")}}, e.code)
        except Exception as e:  # noqa: BLE001
            return self._json({"error": {"message": str(e)}}, 502)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream; charset=utf-8")
        self.send_header("Cache-Control", "no-cache")
        self.end_headers()
        try:
            for line in resp:
                self.wfile.write(line)
                self.wfile.flush()
        except Exception:  # noqa: BLE001
            pass


def main():
    host, _, port = LISTEN.partition(":")
    srv = ThreadingHTTPServer((host or "127.0.0.1", int(port or 8860)), Handler)
    print(f"doubao-manager 监听 http://{LISTEN}  网关={GW_BASE}  账号文件={ACCOUNTS_FILE}")
    srv.serve_forever()


if __name__ == "__main__":
    main()
