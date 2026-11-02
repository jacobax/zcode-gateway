#!/usr/bin/env python3
"""ZCode 套餐自动领取 sidecar —— 配合 Ciii-ATM/zcode-gateway 使用。

做三件事：
做四件事：
  1. 按额度自动启停：有额度（remaining>0）就启用回池；上游明确回了空余额
     （source=api-empty）且 granted==0，连续 N 轮就禁用，避免它被选号选中后
     给客户端返回 1005。source=local 表示没查成功，不据此禁用。
  3. 领到≠能用：领取成功后再刷一次额度，确认真的到账才启用；部分活动有生效时间，
     没到账的记进 pending_activation，等后续轮次额度出现再自动启用。
     对还没领到的逐条提交领取；验证码用本机 Chromium 跑官方 SDK。
  3. 领到就启用：不管当初是谁禁用的，领取成功后立刻 PATCH enabled=true 并刷新额度。
  4. 记忆：同一 (账号, 套餐) 命中 1001–1005 后 6 小时内不再重试，避免每小时重复解验证码。

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
  ZC_RETRY_SKIP_HOURS   失败缓存有效期，默认 6（1001–1005 与 3012 都缓存）
  ZC_CLAIM_GAP_SEC      两次领取之间的间隔秒数，默认 15（防 3012 风控）
  ZC_RELAY_ROTATE_URL   本地中继的换 IP 接口，如 http://127.0.0.1:3129/rotate
                        （设了就在每次 claim 前换一个出口；见 contrib/relay）
  ZC_AUTO_MANAGE        1 = 按额度自动启停（默认 1，旧名 ZC_AUTO_ENABLE 仍可用）
  ZC_EMPTY_ROUNDS       连续几轮无额度才禁用，默认 2
  ZC_ENABLE_AFTER_CLAIM  1 = 领到套餐后自动启用该账号（默认 1）
  ZC_DISABLED_MARKER    额外认这个 lastError 标记的禁用号（默认「迁移时停用」）
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
CLAIM_GAP_SEC = float(os.environ.get("ZC_CLAIM_GAP_SEC", "15"))
# 每次 claim 之前让本地中继换一个出口 IP（中继的 GET /rotate）
RELAY_ROTATE_URL = os.environ.get("ZC_RELAY_ROTATE_URL", "")
AUTO_MANAGE = os.environ.get("ZC_AUTO_MANAGE", os.environ.get("ZC_AUTO_ENABLE", "1")) == "1"
EMPTY_ROUNDS = max(1, int(os.environ.get("ZC_EMPTY_ROUNDS", "2")))
# 领到套餐后自动启用该账号：不管当初是谁禁用的（脚本/迁移/手动），
# 只要能领到就说明它又有产能了。
ENABLE_AFTER_CLAIM = os.environ.get("ZC_ENABLE_AFTER_CLAIM", "1") == "1"
DISABLED_MARKER = os.environ.get("ZC_DISABLED_MARKER", "迁移时停用")
DRY = "--dry-run" in sys.argv

LOG = os.environ.get("ZC_LOG", "/var/log/zcode-claimer.log")
STATE_DIR = os.environ.get("ZC_STATE_DIR", "/var/lib/zcode-claimer")
STATE_FILE = os.path.join(STATE_DIR, "attempts.json")
SKIP_CODES = {1001, 1002, 1003, 1004, 1005, 3012}


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


def rotate_relay():
    """让本地中继换一个出口 IP；没配就什么都不做。"""
    if not RELAY_ROTATE_URL:
        return
    try:
        with urllib.request.urlopen(RELAY_ROTATE_URL, timeout=8) as r:
            ip = r.read().decode("utf-8", "replace").strip()
        say(f"    领取出口已切换 → {ip}")
    except Exception as e:
        say(f"    WARN: 切换出口失败 {e}")


QUOTA_STATE_FILE = os.path.join(STATE_DIR, "quota_state.json")


def load_quota_state():
    try:
        st = json.load(open(QUOTA_STATE_FILE))
    except Exception:
        st = {}
    st.setdefault("disabled_by_us", [])
    st.setdefault("empty_streak", {})
    return st


def save_quota_state(st):
    try:
        os.makedirs(STATE_DIR, mode=0o700, exist_ok=True)
        tmp = QUOTA_STATE_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(st, f, indent=2)
        os.replace(tmp, QUOTA_STATE_FILE)
    except Exception as e:
        say(f"    WARN: 额度状态写入失败 {e}")


def manage_pass(accounts):
    """按额度自动启停，返回 (启用数, 禁用数)。

    启用：禁用中 + remaining>0 + （脚本自己禁用的 或 lastError 带迁移标记）。
    禁用：启用中 + 额度数据新鲜 + granted==0 + 连续 EMPTY_ROUNDS 轮。
          只处理 granted==0（压根没套餐）——granted>0 但 remaining==0 是"用光了"，
          网关自己会用 ExhaustedUntil 处理，脚本不插手，否则会和它打架。
    """
    if not AUTO_MANAGE:
        return 0, 0
    st = load_quota_state()
    known = {a["id"] for a in accounts}
    st["disabled_by_us"] = [i for i in st["disabled_by_us"] if i in known]
    # 领取成功但额度还没到账的号：等额度真到账了要能自动启用。
    st["pending_activation"] = [i for i in (st.get("pending_activation") or []) if i in known]
    pending = st["pending_activation"]
    streaks = st["empty_streak"]
    enabled = disabled = 0
    now = time.time()

    for a in accounts:
        aid = a["id"]
        label = a.get("label") or aid
        if not a.get("enabled"):
            # 网关的定时刷新会跳过禁用账号，额度会永远停在禁用那一刻；
            # 这里主动刷一次，否则"有额度就启用"永远触发不了。
            rst, rres = api("POST", f"/api/accounts/{aid}/refresh", {}, timeout=60)
            if rst == 200 and isinstance(rres.get("account"), dict):
                a = {**a, **rres["account"]}
            elif rst != 200:
                say(f"  {label}: 刷新额度失败 HTTP {rst} {str(rres)[:60]}")
        q = a.get("quota") or {}
        remaining = q.get("remaining", 0) or 0
        granted = q.get("granted", 0) or 0
        # api = 有余额；api-empty = 上游明确回了空列表（确实没套餐）；
        # local = 没查成功，这时候不能据此禁用账号。
        source = q.get("source") or ""
        fresh = (now - updated) < 900 and source in ("api", "api-empty")

        if not a.get("enabled") and remaining > 0:
            ours = aid in st["disabled_by_us"]
            marked = bool(DISABLED_MARKER) and DISABLED_MARKER in (a.get("lastError") or "")
            if not (ours or marked or aid in pending):
                say(f"  {label}: 有额度 {remaining}，但禁用原因不是脚本/迁移，不动")
                continue
            if DRY:
                say(f"  {label}: [dry-run] 有额度 {remaining}，本该启用")
                continue
            code, res = api("PATCH", f"/api/accounts/{aid}", {"enabled": True}, timeout=30)
            if code == 200 and res.get("ok"):
                a["enabled"] = True
                enabled += 1
                if aid in st["disabled_by_us"]:
                    st["disabled_by_us"].remove(aid)
                if aid in pending:
                    pending.remove(aid)
                streaks.pop(aid, None)
                say(f"  {label}: 有额度 {remaining} → 已启用")
            else:
                say(f"  {label}: 启用失败 HTTP {code} {str(res)[:80]}")
            continue

        if a.get("enabled"):
            if fresh and granted == 0 and remaining == 0:
                streaks[aid] = streaks.get(aid, 0) + 1
                if streaks[aid] < EMPTY_ROUNDS:
                    say(f"  {label}: 无任何额度（{streaks[aid]}/{EMPTY_ROUNDS}），再观察一轮")
                    continue
                if DRY:
                    say(f"  {label}: [dry-run] 连续 {streaks[aid]} 轮无额度，本该禁用")
                    continue
                code, res = api("PATCH", f"/api/accounts/{aid}", {"enabled": False}, timeout=30)
                if code == 200 and res.get("ok"):
                    a["enabled"] = False
                    disabled += 1
                    if aid not in st["disabled_by_us"]:
                        st["disabled_by_us"].append(aid)
                    streaks.pop(aid, None)
                    say(f"  {label}: 连续 {EMPTY_ROUNDS} 轮无额度 → 已禁用")
                else:
                    say(f"  {label}: 禁用失败 HTTP {code} {str(res)[:80]}")
            else:
                streaks.pop(aid, None)

    st["empty_streak"] = streaks
    save_quota_state(st)
    return enabled, disabled


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

    woke, slept = manage_pass(accounts)
    if woke or slept:
        say(f"  本轮额度启停：启用 {woke} 个、禁用 {slept} 个")

    attempts = load_attempts()
    now = time.time()
    todo, skipped = [], 0
    for a in accounts:
        aid, label = a.get("id"), a.get("label") or a.get("id")
        blocked_until = int(a.get("claimBlockedUntil") or 0)
        if blocked_until > now:
            say(f"  {label}: 名额未恢复，{int(blocked_until - now)}s 后再试，跳过")
            skipped += 1
            continue
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
            if prev and prev.get("code") in SKIP_CODES:
                # 上游给了 next_at 就按它退避，否则用固定的失败缓存时长
                deadline = prev.get("until") or (prev.get("ts", 0) + RETRY_SKIP_HOURS * 3600)
                if now < deadline:
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

    claimed = failed = streak_3012 = 0
    for i, (aid, label, plan_id, name, scene, region, prefix) in enumerate(todo):
        if i >= MAX_SOLVES:
            say(f"  已达单轮上限 {MAX_SOLVES} 次求解，剩余留到下一轮")
            break
        if i:
            # 同一个出口 IP 连续领取很容易触发上游风控（3012），拉开节奏
            time.sleep(CLAIM_GAP_SEC)
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
        rotate_relay()
        st, res = api("POST", f"/api/accounts/{aid}/claim", body, timeout=120)
        code = res.get("code")
        if st == 200 and res.get("ok"):
            claimed += 1
            streak_3012 = 0
            attempts.pop(f"{aid}|{plan_id}", None)
            say(f"    ✅ 领取成功 {name}")
            if ENABLE_AFTER_CLAIM and not DRY:
                # 领到 ≠ 马上能用：部分活动有生效时间，成功领取后要等到某个时间点
                # 额度才到账。所以先刷新，确认真的拿到可用额度再启用；没到账就记进
                # pending_activation，交给后续轮次的 manage_pass 处理。
                rcode, rres = api("POST", f"/api/accounts/{aid}/refresh", {}, timeout=60)
                quota = {}
                if rcode == 200 and isinstance(rres.get("account"), dict):
                    quota = rres["account"].get("quota") or {}
                remaining = quota.get("remaining") or 0
                if remaining > 0:
                    ecode, eres = api("PATCH", f"/api/accounts/{aid}", {"enabled": True}, timeout=30)
                    if ecode == 200 and eres.get("ok"):
                        say(f"    ↳ 额度已到账（{remaining}）→ 已启用 {label}")
                        for item in accounts:
                            if item.get("id") == aid:
                                item["enabled"] = True
                        qstate = load_quota_state()
                        if aid in qstate["disabled_by_us"]:
                            qstate["disabled_by_us"].remove(aid)
                        pending = qstate.get("pending_activation") or []
                        if aid in pending:
                            pending.remove(aid)
                            qstate["pending_activation"] = pending
                        save_quota_state(qstate)
                    else:
                        say(f"    ↳ 启用失败 HTTP {ecode} {str(eres)[:60]}")
                else:
                    say("    ↳ 领取成功但额度尚未到账（活动可能有生效时间），下轮再检查")
                    qstate = load_quota_state()
                    pending = qstate.get("pending_activation") or []
                    if aid not in pending:
                        pending.append(aid)
                    qstate["pending_activation"] = pending
                    save_quota_state(qstate)
                    say("    ↳ 已刷新额度")
        else:
            failed += 1
            attempts[f"{aid}|{plan_id}"] = {
                "code": code, "ts": now, "msg": str(res.get("message"))[:80],
                "until": int(res.get("nextAt") or 0),
            }
            say(f"    ❌ 领取失败 HTTP {st} code={code} msg={res.get('message')!r}")
            streak_3012 = streak_3012 + 1 if code == 3012 else 0
        save_attempts(attempts)
        if streak_3012 >= 3:
            say("  连续 3 次 3012 风控，本轮提前结束（这些项 6 小时内不再重试）")
            break

    say(f"  本轮结束：成功 {claimed}，失败 {failed}，缓存跳过 {skipped}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
