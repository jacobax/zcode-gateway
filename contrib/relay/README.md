# contrib/relay —— 领取流量用的住宅代理中继

把一批 **TLS 包装的住宅代理**（`https://host:port`，自签证书、没有 IP SAN）转成网关能用的
**明文 HTTP 代理**，并按需轮换出口 IP。

## 为什么需要它

网关用的 `bogdanfinn/tls-client` 在代理拨号阶段会校验证书。实测：

```
WithProxyUrl("https://93.42.177.250:443")
  → x509: cannot validate certificate for 93.42.177.250
     because it doesn't contain any IP SANs
```

加 `WithInsecureSkipVerify()` 也没用——那个选项只作用于**上游** TLS，管不到**代理** TLS。
所以中间垫一层：中继对网关是明文 HTTP 代理（网关原生支持），对上游代理做 TLS 且不校验证书。

```
网关 ──明文 CONNECT──► 127.0.0.1:3129 ──TLS(不校验证书)──► 住宅代理 ──► zcode.z.ai
```

## 跑起来

```bash
mkdir -p /opt/zcode-relay /etc/zcode-relay
install -m 755 contrib/relay/relay.py /opt/zcode-relay/relay.py

# 代理池：一行一个 host:port，`#` 之后是注释，支持 user:pass@host:port
cat > /etc/zcode-relay/proxies.txt <<'EOF'
93.42.177.250:443
83.206.1.225:8090
# 带认证的写法
user:pass@proxy.example.com:1443
EOF
chmod 600 /etc/zcode-relay/proxies.txt

cp deploy/zcode-relay.service /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now zcode-relay
```

网关侧只需要一个 drop-in，让**领取/活动**走中继（模型请求保持直连）：

```ini
# /etc/systemd/system/zcode-gateway.service.d/claim-proxy.conf
[Service]
Environment=ZCODE_PROXY_URL_CLAIM=http://127.0.0.1:3129
```

领取脚本再加一行，做到"一次领取一个 IP"：

```bash
echo 'ZC_RELAY_ROTATE_URL=http://127.0.0.1:3129/rotate' >> /etc/zcode-claimer.env
```

## 行为

| 项 | 说明 |
|---|---|
| 监听 | `127.0.0.1:3129`，只接受 CONNECT（回环专用，别暴露到公网） |
| 体检 | 启动时 + 每 `RELAY_HEALTH_SEC`（默认 600s）一次：TLS 连上后 `CONNECT zcode.z.ai:443` 看回 200 |
| 轮换 | 每 `RELAY_TTL_SEC`（默认 3000s）换一个健康代理，并断开现有隧道 |
| 按需轮换 | `GET /rotate`（仅回环）立即换一个，返回新的 `host:port` |
| 失败处理 | 单个隧道按"当前 → 其它健康节点"最多试 3 个，全失败回 502 |

## 已知限制

- 这些是**开放代理**（无认证），随时可能挂掉或变慢。中继有体检兜底，但一轮领取里如果
  连续 3 次拿不到可用上游，这一轮就废了，下一轮再来。
- 到 `zcode.z.ai` 是端到端 TLS，代理看不到 JWT，但能看到你连了哪个域名。
- 延迟比直连高 1～2s，所以只给领取/活动用，别让模型请求走它。
