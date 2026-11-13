"""doubao-manager 核心层：数据库、网关客户端、运行配置。

沿用 workbuddy 面板的分层：routers(接口) / services(逻辑) / web(静态前端)。
本文件等价于它的 services + config。
"""
import json
import os
import sqlite3
import urllib.error
import urllib.parse
import urllib.request

GW_BASE = os.environ.get("GW_BASE", "http://127.0.0.1:8850").rstrip("/")
GW_KEY = os.environ.get("GW_KEY", "")
ACCOUNTS_FILE = os.environ.get(
    "ACCOUNTS_FILE", "/opt/doubao2api/accounts.json"
)
DB_PATH = os.environ.get("DB_PATH", os.path.join(os.path.dirname(__file__), "panel.db"))


# ---------------- 数据库 ----------------
def db() -> sqlite3.Connection:
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
    conn.execute(
        """CREATE TABLE IF NOT EXISTS req_logs(
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            ts TEXT DEFAULT CURRENT_TIMESTAMP,
            model TEXT, status INTEGER, latency_ms INTEGER,
            chars INTEGER, conv TEXT, account TEXT, error TEXT
        )"""
    )
    conn.commit()
    return conn


# ---------------- 网关调用 ----------------
def gw(method: str, path: str, body=None, timeout: int = 120):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(GW_BASE + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if GW_KEY:
        req.add_header("Authorization", "Bearer " + GW_KEY)
    return urllib.request.urlopen(req, timeout=timeout)


def gw_json(method: str, path: str, body=None, timeout: int = 30):
    try:
        with gw(method, path, body, timeout) as r:
            return json.loads(r.read().decode("utf-8", "ignore"))
    except urllib.error.HTTPError as e:
        try:
            return json.loads(e.read().decode("utf-8", "ignore"))
        except Exception:
            return {"error": {"message": f"网关 HTTP {e.code}"}}
    except Exception as e:  # noqa: BLE001
        return {"error": {"message": f"网关不可达: {e}"}}


# ---------------- 账号同步 ----------------
def sync_accounts() -> dict:
    """把面板启用的账号写进 accounts.json，并让网关热加载。"""
    conn = db()
    rows = conn.execute(
        "SELECT name,cookie,device_id,web_id,note FROM accounts WHERE enabled=1"
    ).fetchall()
    conn.close()
    payload = {
        "accounts": [
            {
                "name": r["name"],
                "cookie": r["cookie"],
                "device_id": r["device_id"],
                "web_id": r["web_id"],
                "note": r["note"],
                "local_app_id": "582478",
            }
            for r in rows
        ]
    }
    os.makedirs(os.path.dirname(os.path.abspath(ACCOUNTS_FILE)), exist_ok=True)
    with open(ACCOUNTS_FILE, "w", encoding="utf-8") as f:
        json.dump(payload, f, ensure_ascii=False, indent=2)
    return gw_json("POST", "/admin/reload")


def log_request(**kw) -> None:
    conn = db()
    conn.execute(
        "INSERT INTO req_logs(model,status,latency_ms,chars,conv,account,error)"
        " VALUES(?,?,?,?,?,?,?)",
        (
            kw.get("model", ""),
            kw.get("status", 0),
            kw.get("latency_ms", 0),
            kw.get("chars", 0),
            kw.get("conv", ""),
            kw.get("account", ""),
            kw.get("error", ""),
        ),
    )
    conn.commit()
    conn.close()
