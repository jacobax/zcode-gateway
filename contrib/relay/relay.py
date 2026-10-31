#!/usr/bin/env python3
"""zcode-relay —— 把一批「TLS 包装的住宅代理」转成网关能用的明文 HTTP 代理。

为什么需要它：
  那批代理是 https:// 形式（TLS 包装，自签证书且没有 IP SAN）。网关用的
  bogdanfinn/tls-client 在代理拨号阶段会校验证书，`WithInsecureSkipVerify`
  只作用于上游 TLS、救不了代理那一段（实测 x509: cannot validate certificate）。
  所以在本地起一个明文 CONNECT 代理，由它去和上游代理做 TLS（不校验证书）。

行为：
  - 监听 127.0.0.1:3129，只接受 CONNECT
  - 上游池来自 POOL_FILE，一行一个 host:port（`#` 之后是注释；支持 user:pass@host:port）
  - 按 TTL 轮换出口（默认 50 分钟），轮换时断开已有隧道
  - 每 HEALTH_INTERVAL 秒重新体检；体检方式是 TLS 连上后 CONNECT zcode.z.ai:443 看回 200
  - 单个隧道失败会自动换下一个健康代理重试（最多 3 次），全失败回 502

管理接口（都只允许回环访问）：
  GET    /rotate            立即换一个出口，返回新的 host:port
  GET    /proxies           列出池子：地址 / 是否健康 / 是否当前出口
  POST   /proxies           {"addrs":["host:port", ...]} 逐条验证，通过的写回池文件
  DELETE /proxies?addr=…    从池子里删掉一条

环境变量：
  RELAY_LISTEN     默认 127.0.0.1:3129
  RELAY_POOL       默认 /etc/zcode-relay/proxies.txt
  RELAY_TTL_SEC    默认 3000
  RELAY_HEALTH_SEC 默认 600
  RELAY_LOG        默认 /var/log/zcode-relay.log
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
TTL_SEC = int(os.environ.get("RELAY_TTL_SEC", "3000"))
HEALTH_SEC = int(os.environ.get("RELAY_HEALTH_SEC", "600"))
LOG_PATH = os.environ.get("RELAY_LOG", "/var/log/zcode-relay.log")
CHECK_TARGET = os.environ.get("RELAY_CHECK_TARGET", "zcode.z.ai:443")
CONNECT_TIMEOUT = float(os.environ.get("RELAY_CONNECT_TIMEOUT", "10"))

ssl_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_CLIENT)
ssl_ctx.check_hostname = False
ssl_ctx.verify_mode = ssl.CERT_NONE

pool = []             # [(host, port, auth_header)]
healthy = []
current = None
tunnels = set()
pool_lock = asyncio.Lock()


def log(msg):
    line = f"[{time.strftime('%Y-%m-%d %H:%M:%S%z')}] {msg}"
    print(line, flush=True)
    try:
        with open(LOG_PATH, "a") as f:
            f.write(line + "\n")
    except Exception:
        pass


def entry_addr(entry):
    host, port, _ = entry
    return f"{host}:{port}"


def parse_entry(raw):
    """把一行解析成 (host, port, auth)；不合法返回 None。"""
    raw = (raw or "").strip()
    if not raw or raw.startswith("#"):
        return None
    raw = raw.split("#", 1)[0].strip()
    if raw.startswith("http://") or raw.startswith("https://"):
        raw = raw.split("://", 1)[1]
    raw = raw.rstrip("/")
    auth = None
    if "@" in raw:
        cred, _, raw = raw.rpartition("@")
        auth = "Basic " + base64.b64encode(cred.encode()).decode()
    host, _, port = raw.rpartition(":")
    if not host or not port.isdigit() or not (0 < int(port) < 65536):
        return None
    return host, int(port), auth


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
    """把当前池子写回文件（保留 user:pass@ 形式）。"""
    lines = []
    for host, port, auth in pool:
        if auth:
            cred = base64.b64decode(auth.split(" ", 1)[1]).decode()
            lines.append(f"{cred}@{host}:{port}")
        else:
            lines.append(f"{host}:{port}")
    try:
        tmp = POOL_FILE + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            f.write("\n".join(lines) + "\n")
        os.chmod(tmp, 0o600)
        os.replace(tmp, POOL_FILE)
    except Exception as e:
        log(f"WARN: 写回代理池失败 {e}")


async def try_upstream(entry, target_host, target_port):
    """连上游代理并发 CONNECT，成功返回 (reader, writer)。"""
    host, port, auth = entry
    try:
        reader, writer = await asyncio.wait_for(
            asyncio.open_connection(host, port, ssl=ssl_ctx, server_hostname=host),
            timeout=CONNECT_TIMEOUT)
    except Exception:
        return None
    try:
        req = f"CONNECT {target_host}:{target_port} HTTP/1.1\r\nHost: {target_host}:{target_port}\r\n"
        if auth:
            req += f"Proxy-Authorization: {auth}\r\n"
        req += "\r\n"
        writer.write(req.encode())
        await asyncio.wait_for(writer.drain(), timeout=CONNECT_TIMEOUT)
        head = await asyncio.wait_for(reader.readuntil(b"\r\n\r\n"), timeout=CONNECT_TIMEOUT)
        if b" 200" not in head.split(b"\r\n")[0]:
            writer.close()
            return None
        return reader, writer
    except Exception:
        try:
            writer.close()
        except Exception:
            pass
        return None


async def check_entry(entry):
    t0 = time.time()
    target_host, _, target_port = CHECK_TARGET.rpartition(":")
    r = await try_upstream(entry, target_host, int(target_port))
    if r:
        _, w = r
        w.close()
        return True, int((time.time() - t0) * 1000)
    return False, int((time.time() - t0) * 1000)


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
    global pool, healthy, current
    pool = load_pool()
    log(f"代理池 {len(pool)} 条，开始体检（目标 {CHECK_TARGET}）")
    results = await asyncio.gather(*[check_entry(e) for e in pool])
    healthy = [e for e, (ok, _) in zip(pool, results) if ok]
    log(f"体检完成：健康 {len(healthy)}/{len(pool)}")
    if healthy and current is None:
        rotate_now("首次选择")
    while True:
        await asyncio.sleep(HEALTH_SEC)
        async with pool_lock:
            snapshot = list(pool)
        results = await asyncio.gather(*[check_entry(e) for e in snapshot])
        async with pool_lock:
            healthy = [e for e, (ok, _) in zip(snapshot, results) if ok]
        log(f"复检完成：健康 {len(healthy)}/{len(pool)}")
        if not healthy:
            current = None


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
    """返回 True 表示已处理。"""
    global pool, healthy, current

    if path == "/rotate":
        entry = rotate_now("脚本请求")
        await send_json(writer, "200 OK", {"current": entry_addr(entry) if entry else None})
        return True

    if path == "/proxies":
        if method == "GET":
            cur = entry_addr(current) if current else None
            hset = {entry_addr(e) for e in healthy}
            items = [{"addr": entry_addr(e), "healthy": entry_addr(e) in hset,
                      "current": entry_addr(e) == cur} for e in pool]
            await send_json(writer, "200 OK", {"current": cur, "total": len(items), "items": items})
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
            for raw in addrs[:200]:
                entry = parse_entry(raw)
                if not entry:
                    results.append({"addr": str(raw)[:80], "ok": False, "error": "格式不合法"})
                    continue
                addr = entry_addr(entry)
                if addr in existing:
                    results.append({"addr": addr, "ok": True, "error": "已存在"})
                    continue
                ok, ms = await check_entry(entry)
                if not ok:
                    results.append({"addr": addr, "ok": False, "error": "验证失败（连接/证书/CONNECT）"})
                    continue
                async with pool_lock:
                    pool.append(entry)
                    healthy.append(entry)
                existing.add(addr)
                results.append({"addr": addr, "ok": True, "latencyMs": ms})
                log(f"新增代理 {addr}（{ms}ms）")
            async with pool_lock:
                save_pool()
            await send_json(writer, "200 OK", {"results": results})
            return True

        if method == "DELETE":
            addr = (query.get("addr") or [""])[0].strip()
            if not addr:
                await send_json(writer, "400 Bad Request", {"error": "缺少 addr"})
                return True
            async with pool_lock:
                pool = [e for e in pool if entry_addr(e) != addr]
                healthy = [e for e in healthy if entry_addr(e) != addr]
                save_pool()
            if current and entry_addr(current) == addr:
                rotate_now("当前出口被删除")
            log(f"删除代理 {addr}")
            await send_json(writer, "200 OK", {"ok": True, "addr": addr})
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
        r = await try_upstream(entry, target_host, int(target_port))
        if not r:
            continue
        up_reader, up_writer = r
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
    asyncio.create_task(health_loop())
    asyncio.create_task(rotate_loop())
    server = await asyncio.start_server(handle, LISTEN_HOST, LISTEN_PORT)
    log(f"监听 {LISTEN_HOST}:{LISTEN_PORT}，TTL={TTL_SEC}s，体检间隔={HEALTH_SEC}s")
    async with server:
        await server.serve_forever()


if __name__ == "__main__":
    try:
        asyncio.run(main())
    except KeyboardInterrupt:
        sys.exit(0)
