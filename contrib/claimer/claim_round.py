#!/usr/bin/env python3
"""ZCode 套餐自动领取 sidecar —— 配合 Ciii-ATM/zcode-gateway 使用。

做三件事：
  1. 回收：禁用账号一旦有生效额度（quota.remaining > 0），自动启用回转发池；
     只处理 lastError 带「迁移时停用」标记的号，手动禁用的账号不碰。
  2. 领取：逐个账号（含禁用账号——禁用的只是转发，领卡接口照样能用）问网关有哪些套餐，
     对还没领到的逐条提交领取；验证码用本机 Chromium 跑官方 SDK。
  3. 记忆：同一 (账号, 套餐) 命中 1001–1005 后 6 小时内不再重试，避免每小时重复解验证码。

关于 claimable：上游 preview 会把套餐列给很多账号，但只在少数账号上标 claimable=true。
实测（2026-10-30 zcode-v3-start-plan-1030-wk）标 false 的账号强行领取依然成功，
所以默认不再以 claimable 为门槛，只跳过「已领取」的项。想保守可设 ZC_REQUIRE_CLAIMABLE=1。

用法：
  claim_round.py            正常跑一轮
  claim_round.py --dry-run  只报告，不解码不领取不启用
环境变量：
  ZC_BASE               默认 http://127.0.0.1:3000
  ZC_PASSWORD           面板密码（必填）
  ZC_SOLVER_DIR         默认 /opt/zcode-claimer/solver
  ZC_MIN_AVAIL_MB       低于该可用内存就跳过本轮，默认 450
  ZC_MAX_SOLVES         单轮最多解几次验证码，默认 8
  ZC_REQUIRE_CLAIMABLE  1 = 只领 preview 标记可领的（默认 0）
  ZC_RETRY_SKIP_HOURS   失败缓存有效期，默认 6
  ZC_AUTO_ENABLE        0 = 关闭「有额度就自动启用」，默认 1
  ZC_DISABLED_MARKER    只对 lastError 含此标记的禁用号做自动启用
"""
import json
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request
import http.cookiejar

BASE = os.environ.get("ZC_BASE", "http://127.0.0.1:3000").rstrip("/")
PASSWORD = os.environ.get("ZC_PASSWORD", "")
SOLVER_DIR = os.environ.get("ZC_SOLVER_DIR", "/opt/zcode-claimer/solver")
MIN_AVAIL_MB = int(os.environ.get("ZC_MIN_AVAIL_MB", "450"))
MAX_SOLVES = int(os.environ.get("ZC_MAX_SOLVES", "8"))
SOLVER_TIMEOUT = int(os.environ.get("ZC_SOLVER_TIMEOUT", "120"))
REQUIRE_CLAIMABLE = os.environ.get("ZC_REQUIRE_CLAIMABLE", "0") == "1"
RETRY_SKIP_HOURS = float(os.environ.get("ZC_RETRY_SKIP_HOURS", "6"))
AUTO_ENABLE = os.environ.get("ZC_AUTO_ENABLE", "1") == "1"
DISABLED_MARKER = os.environ.get("ZC_DISABLED_MARKER", "迁移时停用")
DRY = "--dry-run" in sys.argv

LOG = os.environ.get("ZC_LOG", "/var/log/zcode-claimer.log")
STATE_DIR = os.environ.get("ZC_STATE_DIR", "/var/lib/zcode-claimer")
STATE_FILE = os.path.join(STATE_DIR, "attempts.json")
SKIP_CODES = {1001, 1002, 1003, 1004, 1005}


def say(msg):
    line = f"[{time.strftime('%Y-%m-%d %H:%M:%S%z')}] {msg}"
    print(line, flush=True)
    try:
        with open(LOG, "a") as f:
            f.write(line + "\n")
    except Exception:
        pass


def trim_log():
    try:
        if os.path.exists(LOG) and sum(1 for _ in open(LOG)) > 3000:
            open(LOG, "w").writelines(open(LOG).readlines()[-1500:])
    except Exception:
        pass


def avail_mb():
    try:
        for line in open("/proc/meminfo"):
            if line.startswith("MemAvailable:"):
                return int(line.split()[1]) // 1024
    except Exception:
        pass
    return 10**6


def load_attempts():
    try:
        return json.load(open(STATE_FILE))
    except Exception:
        return {}


def save_attempts(state):
    try:
        os.makedirs(STATE_DIR, mode=0o700, exist_ok=True)
        tmp = STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(state, f)
        os.replace(tmp, STATE_FILE)
    except Exception as e:
        say(f"    WARN: 失败缓存写入失败 {e}")


jar = http.cookiejar.CookieJar()
opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))


def api(method, path, body=None, timeout=60):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, method=method,
                                 headers={"Content-Type": "application/json"})
    try:
        with opener.open(req, timeout=timeout) as r:
            raw = r.read().decode("utf-8", "replace")
            return r.status, (json.loads(raw) if raw.strip() else {})
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", "replace")
        try:
            return e.code, json.loads(raw)
        except Exception:
            return e.code, {"raw": raw[:200]}
    except Exception as e:
        return 0, {"error": str(e)}


def solve_captcha(scene, region, prefix):
    t0 = time.time()
    try:
        p = subprocess.run(["node", "solver_pw.js", scene, region, prefix],
                           cwd=SOLVER_DIR, capture_output=True, text=True,
                           timeout=SOLVER_TIMEOUT)
    except subprocess.TimeoutExpired:
        say(f"    求解器超时（>{SOLVER_TIMEOUT}s）")
        return None
    for line in p.stdout.splitlines():
        if line.startswith("VERIFY_PARAM="):
            param = line[len("VERIFY_PARAM="):].strip()
            if len(param) >= 128:
                say(f"    求解成功 {len(param)}B 用时 {time.time() - t0:.1f}s")
                return param
    tail = (p.stdout or "")[-160:].replace("\n", " ") + (p.stderr or "")[-160:].replace("\n", " ")
    say(f"    求解失败 exit={p.returncode} {tail}")
    return None


def reclaim_pass(accounts):
    """禁用账号一旦有生效额度就启用回池。返回 (启用数, 跳过数)。"""
    if not AUTO_ENABLE:
        return 0, 0
    enabled = skipped = 0
    for a in accounts:
        if a.get("enabled"):
            continue
        label = a.get("label") or a.get("id")
        remaining = (a.get("quota") or {}).get("remaining", 0) or 0
        if remaining <= 0:
            continue
        if DISABLED_MARKER and DISABLED_MARKER not in (a.get("lastError") or ""):
            skipped += 1
            say(f"  {label}: 有额度 {remaining} 但禁用原因不是「{DISABLED_MARKER}」，不动")
            continue
        if DRY:
            say(f"  {label}: [dry-run] 有额度 {remaining}，本该自动启用")
            continue
        st, res = api("PATCH", f"/api/accounts/{a['id']}", {"enabled": True}, timeout=30)
        if st == 200 and res.get("ok"):
            a["enabled"] = True
            enabled += 1
            say(f"  {label}: 有额度 {remaining} → 已自动启用")
        else:
            say(f"  {label}: 自动启用失败 HTTP {st} {str(res)[:100]}")
    return enabled, skipped


def main():
    trim_log()
    if not PASSWORD:
        say("ERROR: 未设置 ZC_PASSWORD")
        return 1
    status, res = api("POST", "/api/auth/login", {"password": PASSWORD}, timeout=20)
    if status != 200:
        say(f"ERROR: 面板登录失败 HTTP {status} {str(res)[:120]}")
        return 1

    status, res = api("GET", "/api/accounts")
    if status != 200:
        say(f"ERROR: 取账号列表失败 HTTP {status} {str(res)[:120]}")
        return 1
    accounts = res.get("items") or []
    on = sum(1 for a in accounts if a.get("enabled"))
    say(f"================ 领取轮开始（{len(accounts)} 个账号：启用 {on} / 禁用 {len(accounts)-on}，dry_run={DRY}）================")

    woke, _ = reclaim_pass(accounts)
    if woke:
        say(f"  本轮自动启用 {woke} 个账号")

    attempts = load_attempts()
    now = time.time()
    todo, skipped = [], 0
    for a in accounts:
        aid, label = a.get("id"), a.get("label") or a.get("id")
        st, snap = api("GET", f"/api/accounts/{aid}/activities", timeout=60)
        if st != 200:
            say(f"  {label}: 活动查询失败 HTTP {st} {str(snap)[:100]}")
            continue
        cap = snap.get("captcha") or {}
        for p in snap.get("plans") or []:
            plan_id = p.get("planId")
            if not plan_id or p.get("claimed"):
                continue
            if REQUIRE_CLAIMABLE and not p.get("claimable"):
                continue
            prev = attempts.get(f"{aid}|{plan_id}")
            if prev and prev.get("code") in SKIP_CODES and now - prev.get("ts", 0) < RETRY_SKIP_HOURS * 3600:
                skipped += 1
                continue
            todo.append((aid, label, plan_id, p.get("name"),
                         cap.get("sceneId"), cap.get("region"), cap.get("prefix")))

    if not todo:
        say(f"  本轮没有可领套餐（按失败缓存跳过 {skipped} 项）")
        return 0

    for t in todo:
        say(f"  可领：{t[1]} / {t[3]} ({t[2]})")
    if DRY:
        say(f"  dry-run：待领 {len(todo)} 项，跳过缓存 {skipped} 项")
        return 0

    mem = avail_mb()
    if mem < MIN_AVAIL_MB:
        say(f"  可用内存 {mem}MB < {MIN_AVAIL_MB}MB，本轮跳过（避免把机器压垮）")
        return 0

    claimed = failed = 0
    for i, (aid, label, plan_id, name, scene, region, prefix) in enumerate(todo):
        if i >= MAX_SOLVES:
            say(f"  已达单轮上限 {MAX_SOLVES} 次求解，剩余留到下一轮")
            break
        say(f"  领取 {label} / {name} ({plan_id})")
        param = ""
        if scene and region and prefix:
            param = solve_captcha(scene, region, prefix)
            if not param:
                failed += 1
                continue
        else:
            # 上游没下发验证码配置时仍直接尝试（有的活动不校验验证码）
            say(f"  {label}: 无验证码配置，尝试不带验证码领取")
        body = {"planId": plan_id}
        if param:
            body["captchaVerifyParam"] = param
            body["captchaRegion"] = region
        st, res = api("POST", f"/api/accounts/{aid}/claim", body, timeout=120)
        code = res.get("code")
        if st == 200 and res.get("ok"):
            claimed += 1
            attempts.pop(f"{aid}|{plan_id}", None)
            say(f"    ✅ 领取成功 {name}")
        else:
            failed += 1
            attempts[f"{aid}|{plan_id}"] = {"code": code, "ts": now, "msg": str(res.get("message"))[:80]}
            say(f"    ❌ 领取失败 HTTP {st} code={code} msg={res.get('message')!r}")
        save_attempts(attempts)
        time.sleep(2)

    say(f"  本轮结束：成功 {claimed}，失败 {failed}，缓存跳过 {skipped}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
