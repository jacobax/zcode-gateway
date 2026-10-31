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
  - 控制口：`GET /rotate`（仅回环）立即换一个出口并断开现有隧道，
    领取脚本在每次 claim 之前调它，做到"一次领取一个 IP"

环境变量：
  RELAY_LISTEN     默认 127.0.0.1:3129
  RELAY_POOL       默认 /etc/zcode-relay/proxies.txt
  RELAY_TTL_SEC    默认 3000
  RELAY_HEALTH_SEC 默认 600
  RELAY_LOG        默认 /var/log/zcode-relay.log
"""
import asyncio
import base64
import os
import random
import ssl
import sys
import time

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

healthy = []          # [(host, port, auth_header)]
current = None        # 当前使用的上游
tunnels = set()       # 活跃隧道，轮换时断开


def log(msg):
    line = f"[{time.strftime('%Y-%m-%d %H:%M:%S%z')}] {msg}"
    print(line, flush=True)
    try:
        with open(LOG_PATH, "a") as f:
            f.write(line + "\n")
    except Exception:
        pass


def load_pool():
    out = []
    try:
        for raw in open(POOL_FILE, encoding="utf-8"):
            raw = raw.strip()
            if not raw or raw.startswith("#"):
                continue
            raw = raw.split("#", 1)[0].strip()
            if raw.startswith("http://") or raw.startswith("https://"):
                raw = raw.split("://", 1)[1]
            auth = None
            if "@" in raw:
                cred, _, raw = raw.rpartition("@")
                auth = "Basic " + base64.b64encode(cred.encode()).decode()
            host, _, port = raw.rpartition(":")
            if not host or not port.isdigit():
                continue
            out.append((host, int(port), auth))
    except FileNotFoundError:
        log(f"ERROR: 找不到代理池文件 {POOL_FILE}")
    return out


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


async def check_one(entry):
    target_host, _, target_port = CHECK_TARGET.rpartition(":")
    r = await try_upstream(entry, target_host, int(target_port))
    if r:
        _, w = r
        w.close()
        return True
    return False


def rotate_now(reason="按 TTL"):
    global current
    if not healthy:
        current = None
        return None
    nxt = random.choice([e for e in healthy if e != current] or healthy)
    current = nxt
    log(f"轮换出口（{reason}）→ {current[0]}:{current[1]}，断开 {len(tunnels)} 条隧道")
    for t in list(tunnels):
        try:
            t.close()
        except Exception:
            pass
    tunnels.clear()
    return current


async def health_loop():
    global healthy, current
    pool = load_pool()
    log(f"代理池 {len(pool)} 条，开始体检（目标 {CHECK_TARGET}）")
    results = await asyncio.gather(*[check_one(e) for e in pool])
    healthy = [e for e, ok in zip(pool, results) if ok]
    log(f"体检完成：健康 {len(healthy)}/{len(pool)}")
    if healthy:
        rotate_now("首次选择")
    while True:
        await asyncio.sleep(HEALTH_SEC)
        results = await asyncio.gather(*[check_one(e) for e in pool])
        healthy = [e for e, ok in zip(pool, results) if ok]
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

    # 控制口：立即换出口（只允许回环）
    if parts[0].upper() == "GET" and parts[1] == "/rotate":
        if not is_loopback(client_writer):
            client_writer.write(b"HTTP/1.1 403 Forbidden\r\n\r\n")
            try:
                await client_writer.drain()
            except Exception:
                pass
            client_writer.close()
            return
        entry = rotate_now("脚本请求")
        body = (f"{entry[0]}:{entry[1]}".encode() if entry else b"none")
        client_writer.write(b"HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: "
                            + str(len(body)).encode() + b"\r\n\r\n" + body)
        try:
            await client_writer.drain()
        except Exception:
            pass
        client_writer.close()
        return

    if parts[0].upper() != "CONNECT":
        client_writer.write(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")
        try:
            await client_writer.drain()
        except Exception:
            pass
        client_writer.close()
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
        log(f"隧道 {entry[0]}:{entry[1]} → {target_host}:{target_port}")
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
