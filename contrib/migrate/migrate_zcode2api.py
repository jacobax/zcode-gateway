#!/usr/bin/env python3
"""zcode2api(accounts.db) -> zcode-gateway(data/accounts.json + gateway.db account_profiles)

映射关系：
  accounts.id              -> Account.id
  data.jwt_token           -> secrets.zcodeJwt（AES-256-GCM，key=data/master.key）
  jwt payload .sub         -> Account.user.id
  data.fingerprint.*       -> account_profiles（保持设备指纹不变）
  data.quota               -> Account.quota（首屏展示，随后由后台刷新覆盖）
  data.use_count           -> Account.usage.requests
"""
import base64
import json
import os
import sqlite3
import time

SRC_DB = "/opt/zcode2api/data/accounts.db"
DST_DIR = "/opt/zcode-gateway/data"
GATEWAY_DB = os.path.join(DST_DIR, "gateway.db")
ACCOUNTS_JSON = os.path.join(DST_DIR, "accounts.json")
PROXY_TOKEN = os.environ.get("ZC_PROXY_TOKEN", "")  # 旧网关的客户端 key；留空则不改网关设置
CONCURRENCY = 2  # 旧库 meta.account_concurrency

master = open(os.path.join(DST_DIR, "master.key"), "rb").read()
assert len(master) == 32, "master.key 必须是 32 字节"

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

_b64 = lambda b: base64.urlsafe_b64encode(b).rstrip(b"=").decode()


def encrypt(plain: str) -> str:
    if not plain:
        return ""
    iv = os.urandom(12)
    sealed = AESGCM(master).encrypt(iv, plain.encode(), None)  # 末尾 16 字节是 tag
    tag, data = sealed[-16:], sealed[:-16]
    return f"enc:v1:{_b64(iv)}.{_b64(tag)}.{_b64(data)}"


def jwt_claims(token: str) -> dict:
    try:
        p = token.split(".")[1]
        p += "=" * (-len(p) % 4)
        return json.loads(base64.urlsafe_b64decode(p))
    except Exception:
        return {}


def pct(remaining, granted):
    if granted <= 0:
        return 0.0
    return round(max(0.0, min(100.0, remaining * 100.0 / granted)), 2)


src = sqlite3.connect(SRC_DB)
src.row_factory = sqlite3.Row
rows = src.execute(
    "select id, provider, name, mode, status, enabled, created_at, data from accounts order by created_at"
).fetchall()

now = int(time.time())
accounts, profiles, skipped = [], [], []

for i, row in enumerate(rows):
    try:
        d = json.loads(row["data"])
    except Exception as e:
        skipped.append((row["id"], f"data 解析失败: {e}"))
        continue
    jwt = (d.get("jwt_token") or "").strip()
    if not jwt:
        skipped.append((row["id"], "无 jwt_token"))
        continue
    claims = jwt_claims(jwt)
    uid = claims.get("sub") or claims.get("user_id") or row["id"]

    quota = d.get("quota") or {}
    models, granted, used, remaining = [], 0, 0, 0
    for mname, q in quota.items():
        if not isinstance(q, dict):
            continue
        g = int(q.get("total") or 0)
        u = int(q.get("used") or 0)
        r = int(q.get("remaining") or 0)
        granted += g
        used += u
        remaining += r
        models.append({
            "name": mname, "granted": g, "used": u, "remaining": r,
            "percent": pct(r, g), "source": "local",
        })

    accounts.append({
        "id": row["id"],
        "label": f"zai-{uid[:8]}",
        "provider": "zai",
        "mode": "start-plan",
        "user": {"id": uid, "email": "", "name": ""},
        "priority": i + 1,
        "enabled": bool(row["enabled"]),
        "createdAt": int(row["created_at"] or now),
        "usage": {
            "requests": int(d.get("use_count") or 0),
            "input_tokens": 0, "output_tokens": 0,
            "cache_read_input_tokens": 0, "cache_creation_input_tokens": 0,
            "reasoning_tokens": 0,
        },
        "models": {},
        "quota": {
            "granted": granted, "used": used, "remaining": remaining,
            "percent": pct(remaining, granted), "source": "local",
            "models": models, "planName": "", "planId": "", "status": "",
            "endsAt": 0, "updatedAt": now,
        },
        "secrets": {"zcodeJwt": encrypt(jwt)},
    })

    fp = d.get("fingerprint") or {}
    if fp.get("device_mid") and fp.get("platform") and fp.get("arch") and fp.get("os_version"):
        profiles.append((
            row["id"], fp.get("platform", ""), fp.get("arch", ""), fp.get("os_version", ""),
            fp.get("language", "en-US"), fp.get("timezone", "UTC"), fp.get("screen", "1512x982"),
            fp["device_mid"], CONCURRENCY, "", now, now,
        ))

old = {}
if os.path.exists(ACCOUNTS_JSON):
    try:
        old = json.load(open(ACCOUNTS_JSON))
    except Exception:
        old = {}
settings = old.get("settings") or {}
settings.update({
    "fingerprint": True,
    "fingerprintSet": True,
    "promptMode": settings.get("promptMode") or "append",
    "uaProfile": settings.get("uaProfile") or "zcode",
    "storeBodies": False,          # 不在 SQLite 里落请求/响应正文，面板可改
    "quotaIntervalSec": 30,
    "activeAccountId": "",
})
if PROXY_TOKEN:
    settings["proxyToken"] = PROXY_TOKEN
payload = {"version": 1, "accounts": accounts, "settings": settings}
tmp = ACCOUNTS_JSON + ".tmp"
with open(tmp, "w") as f:
    json.dump(payload, f, ensure_ascii=False, indent=2)
    f.write("\n")
os.chmod(tmp, 0o600)
os.replace(tmp, ACCOUNTS_JSON)

db = sqlite3.connect(GATEWAY_DB)
db.executemany(
    """INSERT INTO account_profiles
       (account_id, platform, arch, os_version, language, timezone, screen, device_mid,
        concurrency, last_activation_date, created_at, updated_at)
       VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
       ON CONFLICT(account_id) DO UPDATE SET
         platform=excluded.platform, arch=excluded.arch, os_version=excluded.os_version,
         language=excluded.language, timezone=excluded.timezone, screen=excluded.screen,
         device_mid=excluded.device_mid, concurrency=excluded.concurrency,
         updated_at=excluded.updated_at""",
    profiles,
)
db.commit()

print(f"源账号 {len(rows)} 个 → 写入 {len(accounts)} 个，指纹 {len(profiles)} 个，跳过 {len(skipped)} 个")
for sid, why in skipped:
    print(f"  跳过 {sid}: {why}")
print("settings.proxyToken =", settings["proxyToken"])
print("accounts.json 权限:", oct(os.stat(ACCOUNTS_JSON).st_mode & 0o777))
