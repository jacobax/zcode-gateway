#!/usr/bin/env python3
"""zcode-relay —— 代理池中继：把 http / https / socks5 代理统一成网关能用的明文 HTTP 代理。

为什么需要它：
  https:// 形式的代理是 TLS 包装的（自签证书、没有 IP SAN），网关用的
  bogdanfinn/tls-client 在代理拨号阶段会校验证书，`WithInsecureSkipVerify`
  只作用于上游 TLS、救不了代理那一段（实测 x509: cannot validate certificate）。
  所以在本地起一个明文 CONNECT 代理，由它去和上游代理握手（https 走 TLS 不校验证书，
  socks5 走 SOCKS5 握手）。

行为：
  - 监听 127.0.0.1:3129，只接受 CONNECT
  - 上游池来自 POOL_FILE，一行一个，支持：
        host:port                 （等价 https://host:port，兼容老数据）
        http://host:port          （明文 CONNECT）
        https://host:port         （TLS 包装的 CONNECT，不校验代理证书）
        https+insecure://host:port（同上，别名，兼容别的项目的写法）
        socks5://user:pass@host:port
        socks5h://host:port       （远端解析域名）
    行内 `#` 之后是注释
  - 按 TTL 轮换出口，轮换时断开已有隧道
  - 每 health_sec 秒体检一次；连续 delete_after 次失败自动从池子里删掉
  - 单个隧道失败会自动换下一个健康代理重试（最多 3 次），全失败回 502

管理接口（都只允许回环访问）：
  GET    /rotate             立即换一个出口
  GET    /proxies            列出池子（地址 / 健康 / 当前 / 连续失败次数）
  POST   /proxies            {"addrs":[...]} 逐条验证，通过的写回池文件
  DELETE /proxies?addr=…     删除一条
  GET    /settings           体检间隔与自动删除阈值
  POST   /settings           {"health_sec":N,"delete_after":M} 热更新

环境变量：
  RELAY_LISTEN / RELAY_POOL / RELAY_TTL_SEC / RELAY_LOG / RELAY_CHECK_TARGET
  RELAY_SETTINGS     默认 /etc/zcode-relay/settings.json
"""
import asyncio
import base64
import json
import os
import random
import ssl
import sys
import time
import urllib.parse

LISTEN_HOST, LISTEN_PORT = (os.environ.get("RELAY_LISTEN", "127.0.0.1:3129").rsplit(":", 1))
LISTEN_PORT = int(LISTEN_PORT)
POOL_FILE = os.environ.get("RELAY_POOL", "/etc/zcode-relay/proxies.txt")
SETTINGS_FILE = os.environ.get("RELAY_SETTINGS", "/etc/zcode-relay/settings.json")
TTL_SEC = int(os.environ.get("RELAY_TTL_SEC", "3000"))
LOG_PATH = os.environ.get("RELAY_LOG", "/var/log/zcode-relay.log")
CHECK_TARGET = os.environ.get("RELAY_CHECK_TARGET", "zcode.z.ai:443")
CONNECT_TIMEOUT = float(os.environ.get("RELAY_CONNECT_TIMEOUT", "10"))

DEFAULT_SETTINGS = {"health_sec": 600, "delete_after": 3, "check_concurrency": 8}

ssl_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ssl_ctx.check_hostname = False
ssl_ctx.verify_mode = ssl.CERT_NONE

pool = []             # [{"scheme","host","port","user","pass"}]
healthy = []
fail_streak = {}      # addr -> 连续失败次数
current = None
tunnels = set()
pool_lock = asyncio.Lock()
settings = dict(DEFAULT_SETTINGS)


def log(msg):
    line = f"[{time.strftime('%Y-%m-%d %H:%M:%S%z')}] {msg}"
    print(line, flush=True)
    try:
        with open(LOG_PATH, "a") as f:
            f.write(line + "\n")
    except Exception:
        pass


def entry_addr(e):
    # https 省略前缀（兼容老数据），其它协议带上，否则 http/https 同端口会被当成同一条
    if e["scheme"] == "https":
        return f"{e['host']}:{e['port']}"
    return f"{e['scheme']}://{e['host']}:{e['port']}"


def entry_label(e):
    """池文件里的一行（带 scheme，https 省略前缀保持可读）。"""
    auth = f"{e['user']}:{e['pass']}@" if e.get("user") else ""
    if e["scheme"] == "https":
        return f"{auth}{e['host']}:{e['port']}"
    return f"{e['scheme']}://{auth}{e['host']}:{e['port']}"


def parse_entry(raw):
    """解析一行成 dict；不合法返回 None。"""
    raw = (raw or "").strip()
    if not raw or raw.startswith("#"):
        return None
    raw = raw.split("#", 1)[0].strip().rstrip("/")
    scheme = "https"                      # 老数据没有 scheme，按 https 处理
    if "://" in raw:
        scheme, _, raw = raw.partition("://")
        scheme = scheme.lower()
    if scheme == "socks5h":
        scheme = "socks5"
    # https+insecure 是别的项目对「TLS 到代理但不校验证书」的叫法；
    # 我们的 https 本来就不校验（CERT_NONE），当成同一个东西。
    if scheme in ("https+insecure", "https-insecure"):
        scheme = "https"
    if scheme not in ("http", "https", "socks5"):
        return None
    user = pw = None
    if "@" in raw:
        cred, _, raw = raw.rpartition("@")
        user, _, pw = cred.partition(":")
    host, _, port = raw.rpartition(":")
    if not host or not port.isdigit() or not (0 < int(port) < 65536):
        return None
    return {"scheme": scheme, "host": host, "port": int(port), "user": user, "pass": pw}


def load_settings():
    global settings
    try:
        data = json.load(open(SETTINGS_FILE))
        for k in DEFAULT_SETTINGS:
            if k in data:
                settings[k] = int(data[k])
    except Exception:
        pass
    settings["health_sec"] = max(30, min(86400, int(settings.get("health_sec", 600))))
    settings["delete_after"] = max(0, min(100, int(settings.get("delete_after", 3))))
    settings["check_concurrency"] = max(1, min(64, int(settings.get("check_concurrency", 8))))


def save_settings():
    try:
        tmp = SETTINGS_FILE + ".tmp"
        with open(tmp, "w") as f:
            json.dump(settings, f)
        os.chmod(tmp, 0o600)
        os.replace(tmp, SETTINGS_FILE)
    except Exception as e:
        log(f"WARN: 写设置失败 {e}")


def load_pool():
    out = []
    try:
        for raw in open(POOL_FILE, encoding="utf-8"):
            e = parse_entry(raw)
            if e:
                out.append(e)
    except FileNotFoundError:
        log(f"ERROR: 找不到代理池文件 {POOL_FILE}")
    return out


def save_pool():
    lines = [entry_label(e) for e in pool]
    try:
        tmp = POOL_FILE + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
        os.chmod(tmp, 0o600)
        os.replace(tmp, POOL_FILE)
    except Exception as e:
        log(f"WARN: 写回代理池失败 {e}")


async def socks5_handshake(reader, writer, host, port, user, password):
    methods = b"\x00" if not user else b"\x00\x02"
    writer.write(b"\x05" + bytes([len(methods)]) + methods)
    await writer.drain()
    resp = await reader.readexactly(2)
    if resp[0] != 5:
        return False
    if resp[1] == 0x02:
        if not user:
            return False
        u = user.encode()
        p = (password or "").encode()
        writer.write(b"\x01" + bytes([len(u)]) + u + bytes([len(p)]) + p)
        await writer.drain()
        ar = await reader.readexactly(2)
        if ar[1] != 0:
            return False
    elif resp[1] != 0x00:
        return False
    hb = host.encode()
    writer.write(b"\x05\x01\x00\x03" + bytes([len(hb)]) + hb + port.to_bytes(2, "big"))
    await writer.drain()
    rep = await reader.readexactly(4)
    if rep[1] != 0:
        return False
    atyp = rep[3]
    if atyp == 1:
        await reader.readexactly(4)
    elif atyp == 4:
        await reader.readexactly(16)
    elif atyp == 3:
        ln = (await reader.readexactly(1))[0]
        await reader.readexactly(ln)
    await reader.readexactly(2)
    return True


async def open_tunnel(entry, target_host, target_port):
    """连上游代理并打通到 target。成功 (reader, writer, "")，失败 (None, None, 原因)。"""
    host, port = entry["host"], entry["port"]
    use_tls = entry["scheme"] == "https"
    try:
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection(host, port, ssl=ssl_ctx if use_tls else None,
                                    server_hostname=host if use_tls else None),
            timeout=CONNECT_TIMEOUT)
    except asyncio.TimeoutError:
        return None, None, "连接超时"
    except ssl.SSLError:
        return None, None, "TLS 握手失败"
    except ConnectionRefusedError:
        return None, None, "连接被拒"
    except OSError as e:
        return None, None, f"连接失败（{e.strerror or e.__class__.__name__}）"
    except Exception as e:
        return None, None, f"连接失败（{e.__class__.__name__}）"
    try:
        if entry["scheme"] == "socks5":
            ok = await asyncio.wait_for(
                socks5_handshake(reader, writer, target_host, target_port,
                                 entry.get("user"), entry.get("pass")),
                timeout=CONNECT_TIMEOUT)
            if not ok:
                writer.close()
                return None, None, "SOCKS5 握手被拒"
            return reader, writer, ""

        req = f"CONNECT {target_host}:{target_port} HTTP/1.1\r\nHost: {target_host}:{target_port}\r\n"
        if entry.get("user"):
            cred = f"{entry['user']}:{entry.get('pass') or ''}".encode()
            req += "Proxy-Authorization: Basic " + base64.b64encode(cred).decode() + "\r\n"
        req += "\r\n"
        writer.write(req.encode())
        await asyncio.wait_for(writer.drain(), timeout=CONNECT_TIMEOUT)
        head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), timeout=CONNECT_TIMEOUT)
        first = head.split(b"\r\n")[0].decode("latin1", "replace")
        if " 200" not in first:
            writer.close()
            code = first.split(" ")[1] if len(first.split(" ")) > 1 else "?"
            return None, None, f"CONNECT 被拒（{code}）"
        return reader, writer, ""
    except asyncio.TimeoutError:
        try:
            writer.close()
        except Exception:
            pass
        return None, None, "CONNECT 超时"
    except Exception as e:
        try:
            writer.close()
        except Exception:
            pass
        return None, None, f"CONNECT 失败（{e.__class__.__name__}）"


# 体检路径：能拿到 2xx/3xx/4xx 就说明代理真的把流量转出去了。
CHECK_PATH = os.environ.get("RELAY_CHECK_PATH", "/api/v1/client/configs")


def _recv_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise OSError("连接被关闭")
        buf += chunk
    return buf


class _TLSOverStream:
    """在已有流（可能是套在代理 TLS 里的 SSLSocket）上再做一层 TLS。

    坑：Python 里对 SSLSocket 直接 ctx.wrap_socket() 不会嵌套 TLS —— 外层会拿底层 fd
    裸发数据，目标 ClientHello 变成明文，对端只能回 alert（INVALID_ALERT /
    UNEXPECTED_MESSAGE）。必须用 MemoryBIO 自己泵字节。
    """

    def __init__(self, sock, ctx, server_hostname):
        self.sock = sock
        self.inbio = ssl.MemoryBIO()
        self.outbio = ssl.MemoryBIO()
        self.obj = ctx.wrap_bio(self.inbio, self.outbio, server_hostname=server_hostname)
        while True:
            try:
                self.obj.do_handshake()
                break
            except ssl.SSLWantReadError:
                self._flush()
                self._fill()
            else:
                self._flush()
        self._flush()

    def _flush(self):
        data = self.outbio.read()
        if data:
            self.sock.sendall(data)

    def _fill(self):
        data = self.sock.recv(16384)
        if not data:
            raise OSError("连接被关闭")
        self.inbio.write(data)

    def sendall(self, data):
        self.obj.write(data)
        self._flush()

    def recv(self, n):
        while True:
            try:
                return self.obj.read(n)
            except ssl.SSLWantReadError:
                self._flush()
                self._fill()

    def close(self):
        try:
            self.sock.close()
        except Exception:
            pass



def _socks5_connect(sock, host, port, user, password):
    """在已连上的 socket 上做 SOCKS5 握手，返回 (ok, 原因)。"""
    methods = b"\x00\x02" if user else b"\x00"
    sock.sendall(b"\x05" + bytes([len(methods)]) + methods)
    resp = _recv_exact(sock, 2)
    if resp[0] != 5:
        return False, "SOCKS5 版本不符"
    if resp[1] == 0x02:
        if not user:
            return False, "SOCKS5 要求认证但没配账号"
        u, p = user.encode(), (password or "").encode()
        sock.sendall(b"\x01" + bytes([len(u)]) + u + bytes([len(p)]) + p)
        ar = _recv_exact(sock, 2)
        if ar[1] != 0:
            return False, "SOCKS5 认证失败"
    elif resp[1] != 0x00:
        return False, f"SOCKS5 方法被拒（0x{resp[1]:02x}）"
    hb = host.encode()
    sock.sendall(b"\x05\x01\x00\x03" + bytes([len(hb)]) + hb + port.to_bytes(2, "big"))
    rep = _recv_exact(sock, 4)
    if rep[1] != 0:
        return False, f"SOCKS5 CONNECT 被拒（{rep[1]}）"
    atyp = rep[3]
    if atyp == 1:
        _recv_exact(sock, 4)
    elif atyp == 4:
        _recv_exact(sock, 16)
    elif atyp == 3:
        _recv_exact(sock, _recv_exact(sock, 1)[0])
    _recv_exact(sock, 2)
    return True, ""


def _probe_blocking(entry, target_host, target_port, path, timeout):
    """完整走一遍：连代理 → 打通目标（http/https 用 CONNECT，socks5 用握手）
    → 对目标做一次真实 TLS → 发一个 GET。

    只做 TCP 隧道连通是不够的：能建隧道但转发真实 TLS 就断的"假健康"代理
    在旧体检里全是绿的，实际请求却一个都出不去。
    """
    import socket

    host, port = entry["host"], entry["port"]
    scheme = entry["scheme"]
    user, password = entry.get("user"), entry.get("pass")
    t0 = time.time()

    def elapsed():
        return int((time.time() - t0) * 1000)

    sock = None
    try:
        sock = socket.create_connection((host, port), timeout=timeout)
        sock.settimeout(timeout)
        if scheme == "https":
            ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
            sock = ctx.wrap_socket(sock, server_hostname=host)

        if scheme == "socks5":
            ok, reason = _socks5_connect(sock, target_host, target_port, user, password)
            if not ok:
                return False, elapsed(), reason
        else:
            sock.sendall(
                f"CONNECT {target_host}:{target_port} HTTP/1.1\r\n"
                f"Host: {target_host}:{target_port}\r\n".encode()
            )
            if user:
                cred = f"{user}:{password or ''}".encode()
                sock.sendall(b"Proxy-Authorization: Basic " + base64.b64encode(cred) + b"\r\n")
            sock.sendall(b"\r\n")
            head = b""
            while b"\r\n\r\n" not in head:
                chunk = sock.recv(1024)
                if not chunk:
                    return False, elapsed(), "CONNECT 无响应"
                head += chunk
                if len(head) > 8192:
                    return False, elapsed(), "CONNECT 响应异常"
            status_line = head.split(b"\r\n", 1)[0].decode("latin1", "replace")
            if " 200" not in status_line:
                code = status_line.split(" ")[1] if len(status_line.split(" ")) > 1 else "?"
                return False, elapsed(), f"CONNECT 被拒（{code}）"

        tctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
        tctx.check_hostname = False
        tctx.verify_mode = ssl.CERT_NONE
        tsock = _TLSOverStream(sock, tctx, target_host)
        tsock.sendall(
            f"GET {path} HTTP/1.1\r\nHost: {target_host}\r\n"
            f"User-Agent: ZCode/{os.environ.get('ZCODE_APP_VERSION', '3.16.3')}\r\n"
            f"Accept: application/json\r\nConnection: close\r\n\r\n".encode()
        )
        resp = tsock.recv(256)
        if not resp:
            return False, elapsed(), "目标无响应"
        line = resp.split(b"\r\n", 1)[0].decode("latin1", "replace")
        parts = line.split(" ")
        if len(parts) < 2 or not parts[1].isdigit():
            return False, elapsed(), f"响应异常（{line[:32]}）"
        code = int(parts[1])
        if code >= 500:
            return False, elapsed(), f"目标 HTTP {code}"
        return True, elapsed(), ""
    except ssl.SSLError as e:
        return False, elapsed(), f"TLS 失败（{getattr(e, 'reason', '') or e.__class__.__name__}）"
    except socket.timeout:
        return False, elapsed(), "超时"
    except OSError as e:
        return False, elapsed(), f"连接失败（{e.strerror or e.__class__.__name__}）"
    except Exception as e:  # noqa: BLE001 - 体检不能让单条代理拖垮整轮
        return False, elapsed(), f"失败（{e.__class__.__name__}）"
    finally:
        try:
            if sock is not None:
                sock.close()
        except Exception:
            pass


async def check_entry(entry):
    th, _, tp = CHECK_TARGET.rpartition(":")
    return await asyncio.to_thread(
        _probe_blocking,
        entry,
        th,
        int(tp),
        CHECK_PATH,
        CONNECT_TIMEOUT,
    )


async def check_many(entries):
    """并发体检，并发上限取 settings['check_concurrency']。返回顺序与输入一致。"""
    limit = max(1, int(settings.get("check_concurrency") or 8))
    sem = asyncio.Semaphore(limit)

    async def one(e):
        async with sem:
            return await check_entry(e)

    return await asyncio.gather(*[one(e) for e in entries])


def rotate_now(reason="按 TTL"):
    global current
    if not healthy:
        current = None
        return None
    nxt = random.choice([e for e in healthy if e != current] or healthy)
    current = nxt
    log(f"轮换出口（{reason}）→ {entry_addr(current)}，断开 {len(tunnels)} 条隧道")
    for t in list(tunnels):
        try:
            t.close()
        except Exception:
            pass
    tunnels.clear()
    return current


async def health_loop():
    global pool, healthy, current, fail_streak
    load_settings()
    pool = load_pool()
    log(f"代理池 {len(pool)} 条，开始体检（目标 {CHECK_TARGET}，间隔 {settings['health_sec']}s，"
        f"连续失败 {settings['delete_after']} 次自动删除，并发 {settings['check_concurrency']}）")
    while True:
        async with pool_lock:
            snapshot = list(pool)
        results = await check_many(snapshot)
        dropped = []
        async with pool_lock:
            healthy = []
            for e, (ok, _ms, _reason) in zip(snapshot, results):
                addr = entry_addr(e)
                if ok:
                    healthy.append(e)
                    fail_streak[addr] = 0
                    continue
                fail_streak[addr] = fail_streak.get(addr, 0) + 1
                if settings["delete_after"] > 0 and fail_streak[addr] >= settings["delete_after"]:
                    dropped.append(e)
            if dropped:
                drop_addrs = {entry_addr(e) for e in dropped}
                pool = [e for e in pool if entry_addr(e) not in drop_addrs]
                for a in drop_addrs:
                    fail_streak.pop(a, None)
                save_pool()
                if current and entry_addr(current) in drop_addrs:
                    rotate_now("当前出口被判死")
        log(f"复检完成：健康 {len(healthy)}/{len(pool)}"
            + (f"，自动删除 {len(dropped)} 条：{', '.join(entry_addr(e) for e in dropped)}" if dropped else ""))
        if not healthy:
            current = None
        elif current is None or current not in healthy:
            rotate_now("首次选择")
        await asyncio.sleep(max(30, settings["health_sec"]))


async def rotate_loop():
    while True:
        await asyncio.sleep(TTL_SEC)
        rotate_now()


async def pipe(reader, writer):
    try:
        while True:
            data = await reader.read(65536)
            if not data:
                break
            writer.write(data)
            await writer.drain()
    except Exception:
        pass
    finally:
        try:
            writer.close()
        except Exception:
            pass


def is_loopback(writer):
    peer = writer.get_extra_info("peername")
    return bool(peer) and peer[0] in ("127.0.0.1", "::1")


async def send_json(writer, status, payload):
    body = json.dumps(payload, ensure_ascii=False).encode()
    head = (f"HTTP/1.1 {status}\r\nContent-Type: application/json; charset=utf-8\r\n"
            f"Content-Length: {len(body)}\r\nConnection: close\r\n\r\n").encode()
    try:
        writer.write(head + body)
        await writer.drain()
    except Exception:
        pass
    try:
        writer.close()
    except Exception:
        pass


async def handle_admin(method, path, query, body, writer):
    global pool, healthy, current, settings

    if path == "/rotate":
        entry = rotate_now("脚本请求")
        await send_json(writer, "200 OK", {"current": entry_addr(entry) if entry else None})
        return True

    if path == "/settings":
        if method == "GET":
            await send_json(writer, "200 OK", settings)
            return True
        if method == "POST":
            try:
                payload = json.loads(body or b"{}")
            except Exception:
                await send_json(writer, "400 Bad Request", {"error": "invalid json"})
                return True
            if "health_sec" in payload:
                try:
                    settings["health_sec"] = max(30, min(86400, int(payload["health_sec"])))
                except Exception:
                    pass
            if "check_concurrency" in payload:
                try:
                    settings["check_concurrency"] = max(1, min(64, int(payload["check_concurrency"])))
                except Exception:
                    pass
            if "delete_after" in payload:
                try:
                    settings["delete_after"] = max(0, min(100, int(payload["delete_after"])))
                except Exception:
                    pass
            save_settings()
            log(f"设置更新：间隔 {settings['health_sec']}s，连续失败 {settings['delete_after']} 次删除，并发 {settings['check_concurrency']}")
            await send_json(writer, "200 OK", settings)
            return True

    if path == "/proxies":
        if method == "GET":
            cur = entry_addr(current) if current else None
            hset = {entry_addr(e) for e in healthy}
            items = [{
                "addr": entry_addr(e),
                "scheme": e["scheme"],
                "label": entry_label(e),
                "healthy": entry_addr(e) in hset,
                "current": entry_addr(e) == cur,
                "failStreak": fail_streak.get(entry_addr(e), 0),
            } for e in pool]
            await send_json(writer, "200 OK",
                            {"current": cur, "total": len(items), "items": items, "settings": settings})
            return True

        if method == "POST":
            try:
                payload = json.loads(body or b"{}")
            except Exception:
                await send_json(writer, "400 Bad Request", {"error": "invalid json"})
                return True
            addrs = payload.get("addrs") or []
            if isinstance(addrs, str):
                addrs = [addrs]
            results = []
            async with pool_lock:
                existing = {entry_addr(e) for e in pool}
            pending = []
            dup = 0
            for raw in addrs[:200]:
                entry = parse_entry(raw)
                if not entry:
                    results.append({"addr": str(raw)[:80], "ok": False, "error": "格式不合法"})
                    continue
                addr = entry_addr(entry)
                if addr in existing:
                    # 池子里已有，或这一批里前面已经出现过同一条
                    results.append({"addr": addr, "ok": True, "existing": True, "error": "已存在"})
                    dup += 1
                    continue
                existing.add(addr)
                pending.append((addr, entry))
            added = 0
            if pending:
                checks = await check_many([e for _, e in pending])
                for (addr, entry), (ok, ms, reason) in zip(pending, checks):
                    if not ok:
                        results.append({"addr": addr, "ok": False, "error": reason or "验证失败"})
                        continue
                    async with pool_lock:
                        pool.append(entry)
                        healthy.append(entry)
                        fail_streak[addr] = 0
                    added += 1
                    results.append({"addr": addr, "ok": True, "latencyMs": ms,
                                    "scheme": entry["scheme"]})
                    log(f"新增代理 {entry_label(entry)}（{ms}ms）")
            if added:
                async with pool_lock:
                    save_pool()
            invalid = sum(1 for r in results if r.get("error") == "格式不合法")
            failed = sum(1 for r in results if not r.get("ok"))
            summary = {"total": len(results), "added": added, "existing": dup,
                       "invalid": invalid, "failed": failed - invalid}
            log(f"批量添加：共 {summary['total']} 条，新增 {added}，已存在 {dup}，"
                f"格式错误 {invalid}，验证失败 {summary['failed']}")
            await send_json(writer, "200 OK", {"results": results, "summary": summary})
            return True

        if method == "DELETE":
            addr = (query.get("addr") or [""])[0].strip()
            if not addr:
                await send_json(writer, "400 Bad Request", {"error": "缺少 addr"})
                return True
            async with pool_lock:
                before = len(pool)
                pool = [e for e in pool if entry_addr(e) != addr]
                healthy = [e for e in healthy if entry_addr(e) != addr]
                fail_streak.pop(addr, None)
                removed = before - len(pool)
                save_pool()
            if current and entry_addr(current) == addr:
                rotate_now("当前出口被删除")
            log(f"删除代理 {addr}（{'成功' if removed else '未找到'}）")
            await send_json(writer, "200 OK", {"ok": removed > 0, "addr": addr})
            return True

    return False


async def handle(client_reader, client_writer):
    try:
        head = await asyncio.wait_for(client_reader.readuntil(b"\r\n\r\n"), timeout=15)
    except Exception:
        client_writer.close()
        return
    first = head.split(b"\r\n", 1)[0].decode("latin1", "replace")
    parts = first.split()
    if len(parts) < 2:
        client_writer.close()
        return
    method = parts[0].upper()
    path, _, qs = parts[1].partition("?")
    query = {}
    for kv in qs.split("&"):
        if "=" in kv:
            k, _, v = kv.partition("=")
            query.setdefault(urllib.parse.unquote(k), []).append(urllib.parse.unquote(v))

    if method != "CONNECT":
        if not is_loopback(client_writer):
            await send_json(client_writer, "403 Forbidden", {"error": "只允许回环访问"})
            return
        body = b""
        if method in ("POST", "PUT"):
            try:
                clen = 0
                for line in head.split(b"\r\n"):
                    if line.lower().startswith(b"content-length:"):
                        clen = int(line.split(b":", 1)[1].strip())
                body = await asyncio.wait_for(client_reader.read(clen), timeout=10) if clen else b""
            except Exception:
                body = b""
        if await handle_admin(method, path, query, body, client_writer):
            return
        await send_json(client_writer, "405 Method Not Allowed", {"error": "unsupported"})
        return

    target_host, _, target_port = parts[1].rpartition(":")
    if not target_host or not target_port.isdigit():
        client_writer.write(b"HTTP/1.1 400 Bad Request\r\n\r\n")
        client_writer.close()
        return

    candidates = ([current] if current else []) + [e for e in healthy if e != current]
    for entry in candidates[:3]:
        r = await open_tunnel(entry, target_host, int(target_port))
        if not r:
            continue
        up_reader, up_writer, _ = r
        try:
            client_writer.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            await client_writer.drain()
        except Exception:
            up_writer.close()
            return
        tunnels.add(client_writer)
        log(f"隧道 {entry_addr(entry)} → {target_host}:{target_port}")
        try:
            await asyncio.gather(pipe(client_reader, up_writer), pipe(up_reader, client_writer))
        finally:
            tunnels.discard(client_writer)
            for w in (client_writer, up_writer):
                try:
                    w.close()
                except Exception:
                    pass
        return

    log(f"无可用上游（目标 {target_host}:{target_port}，健康 {len(healthy)}）")
    try:
        client_writer.write(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
        await client_writer.drain()
    except Exception:
        pass
    client_writer.close()


async def main():
    load_settings()
    asyncio.create_task(health_loop())
    asyncio.create_task(rotate_loop())
    server = await asyncio.start_server(handle, LISTEN_HOST, LISTEN_PORT)
    log(f"监听 {LISTEN_HOST}:{LISTEN_PORT}，TTL={TTL_SEC}s")
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        sys.exit(0)
