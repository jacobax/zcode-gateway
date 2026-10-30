#!/usr/bin/env python3
"""迁移后清理：停用「无有效套餐」的账号 + 同一 uid 只保留一条启用。

依据：/opt/zcode-gateway 的额度刷新对每个账号调 billing/current，
有套餐的返回 plans[] 非空（Start Plan 8M 等），没套餐的返回 plans: []，
这类账号在调度池里仍然 Available（ExhaustedUntil=0），被选中后上游回
{"code":1005,"msg":"exceed quota limit"}，且新网关不识别该业务码、直接透传给客户端。
"""
import json
import shutil
import time

PATH = "/opt/zcode-gateway/data/accounts.json"
shutil.copy2(PATH, PATH + ".bak-" + time.strftime("%Y%m%d-%H%M%S"))

d = json.load(open(PATH))
accs = d["accounts"]

dead = 0
for a in accs:
    q = a.get("quota") or {}
    has_plan = bool(q.get("planName")) or bool(q.get("planId")) or bool(q.get("models"))
    if not has_plan:
        a["enabled"] = False
        a["lastError"] = "迁移时停用：上游无有效套餐（billing/current plans=[]）"
        a["lastErrorAt"] = int(time.time())
        dead += 1

seen = {}
dupes = 0
for a in sorted(accs, key=lambda x: x.get("priority", 0)):
    uid = (a.get("user") or {}).get("id") or a["id"]
    if uid in seen:
        if a.get("enabled"):
            a["enabled"] = False
            a["lastError"] = f"迁移时停用：同一 uid 重复行，已由 {seen[uid]} 接管"
            a["lastErrorAt"] = int(time.time())
            dupes += 1
    else:
        seen[uid] = a["id"]

tmp = PATH + ".tmp"
with open(tmp, "w") as f:
    json.dump(d, f, ensure_ascii=False, indent=2)
    f.write("\n")
import os
os.chmod(tmp, 0o600)
os.replace(tmp, PATH)

enabled = [a for a in accs if a.get("enabled")]
print(f"账号 {len(accs)} 条：停用无套餐 {dead} 条，停用重复 uid {dupes} 条，最终启用 {len(enabled)} 条")
for a in enabled:
    q = a.get("quota") or {}
    print(f"  {a['label']}  {q.get('planName','?')}  remaining={q.get('remaining')}")
