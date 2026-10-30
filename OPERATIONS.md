# 运维手册（本 fork 专用）

本仓库是 [Ciii-ATM/zcode-gateway](https://github.com/Ciii-ATM/zcode-gateway) 的 fork。
`main` = 上游 + 本地补丁，`upstream` 远端指向上游仓库。

```
origin    https://github.com/jacobax/zcode-gateway   （本 fork）
upstream  https://github.com/Ciii-ATM/zcode-gateway   （上游）
```

## 与上游的差异

| 提交 | 文件 | 内容 |
|---|---|---|
| `fix(quota)` 死锁 | `quota.go` | 池里有禁用账号时，配额刷新循环会永久卡死 |
| `fix(quota)` 兜底 | `quota.go` | `billing/current` 返回空 plans 时，改用 `billing/balance` 聚合额度 |

除 `quota.go` 外，`deploy/`、`contrib/` 都是新增目录，不碰上游代码。

## 吸收上游更新

```bash
cd /opt/zcode-gateway-src          # 或本地 clone
git fetch upstream
git rebase upstream/main           # 两个补丁在 quota.go，冲突通常只在这里
go build ./... && go test ./...    # 本地没有 Go 就在部署机上编
git push origin main               # 触发部署机的自动更新
```

补丁本身很小（约 50 行，集中在一个函数块），上游如果重构 `refreshAllQuotas`
或 `refreshQuota`，rebase 时冲突会直接暴露——比之前"文本锚点打补丁"的方式安全。

**建议**：这两个补丁本质是上游的 bug，先给上游提 PR；合了就可以在 rebase 时丢弃本地提交。

## 部署形态

```
cloudflared / 反向代理 ──► 127.0.0.1:3000  zcode-gateway（systemd）
                                     │
                                     ├── data/accounts.json   账号 + 加密凭据 + 设置
                                     ├── data/master.key      AES-256-GCM 主密钥（0600）
                                     └── data/gateway.db      日志 / 会话粘性 / 面板会话 / 指纹

zcode-claimer.timer（每小时 :17）──► 面板管理接口 ──► 上游 billing/claim
                                  └── Chromium 求解阿里云验证码
```

数据目录 `/opt/zcode-gateway/data` 是唯一的状态源，升级/回滚都不碰它。

## 自动更新

`zcode-gateway-autoupdate.timer`（每天 05:50 北京时间）执行
`/usr/local/bin/zcode-gateway-autoupdate.sh`：

```
git fetch origin main → 比对 VERSION 里的 commit → 备份 data/旧二进制
→ checkout → go build → 冒烟（临时端口 3999）→ 停服务换二进制 → /health 检查
→ 失败则装回旧二进制并重启
```

- 日志：`/var/log/zcode-gateway-autoupdate.log`
- 备份：`/opt/zcode-gateway-backups/<时间戳>/`（保留 7 份，含 SQLite 在线备份与旧二进制）
- 手动触发：`systemctl start zcode-gateway-autoupdate.service`
- 强制重建：`FORCE_REINSTALL=1 /usr/local/bin/zcode-gateway-autoupdate.sh`

## 已知坑（都踩过）

1. **`billing/current` 已不可信**。2026-10 起它对所有账号返回 `plans: []`，真实额度只在
   `billing/balance`。上游原版据此聚合，面板恒显示 0 —— 已由本 fork 的补丁修复。

2. **禁用账号会拖死配额刷新**。上游 `refreshAllQuotas` 按全部账号数等信号，禁用账号不发
   信号，循环卡死。现象是所有账号的 `quota.updatedAt` 停在上次重启后的第一轮。

3. **`1005 exceed quota limit` 是 HTTP 200**。没有额度的账号被选中时，上游回
   `{"code":1005,...}` 且状态码 200，网关不认识这个业务码，会把它当成正常响应透传给客户端。
   所以没额度的号要禁用，别留在转发池里。

4. **选号不看额度**。`Available()` 只判断 `enabled && ExhaustedUntil<=now`，额度为 0 的
   启用账号照样会被选中 —— 这也是第 3 条的触发条件。

5. **`claimable` 不可信**。已领取的套餐在 preview 里可能仍标 `claimable=true`，重复领取
   返回 `1003`。判断"能不能领"只能靠实际提交。

6. **验证码只能真浏览器解**。happy-dom 路线在 2026-09 之后的上游 pe 虚拟机上 stall；
   Chromium 路线稳定在 6–8s/次，峰值内存约 590MB。

7. **`billing/claim` 必须带 `X-Aliyun-Captcha-Verify-Region`**，漏掉返回 `3007`；
   直连上游还要补齐 `X-ZCode-*` 头，否则 `3001 parameter error` —— 所以领取统一走网关接口。
8. **`granted == 0` 的号不会显示"额度耗尽"**。`summary()` 的判定是
   `Granted > 0 && Remaining <= 0`，所以"压根没有套餐"的账号状态仍是「可用」，
   照样会被选号选中然后返回 1005。概览卡片的"可用"用的是另一个口径
   （`enabled && Remaining > 0`），两者会差出这些号——看到数字对不上先查这里。
   `contrib/claimer` 的自动启停就是补这个洞：`granted==0` 连续两轮就禁用，
   等额度回来再自动启用。

## 出口 IP 与风控

上游的 `3012 账号或者网络环境存在异常` 是按**出口 IP** 判的：同一台机器、同一个 IP
连续领取多个账号，领到第 6～8 个就开始被拒。脚本侧的限速/熔断/缓存只能缓解，
要根治得让每个账号（或每轮）从不同 IP 出去。

网关本身只支持一个全局上游代理（`ZCODE_PROXY_URL` / `ZCODE_PROXY`，支持
`http`/`https`/`socks5`/`socks5h`），见 `upstream.go`。所以思路是给它挂一个
**能按用户名切换出口地址的代理池**：

```
[he-ipv6-pool](https://github.com/x6nux/he-ipv6-pool)
  一条免费 HE IPv6 隧道 → SOCKS5/HTTP 代理池，出口地址由代理用户名后缀决定
  例：alice-ttl-15m-cidr-64 → 每 15 分钟换一个地址，且每次换到不同的 /64
```

接法（在网关的 systemd 单元里加一行）：

```ini
Environment=ZCODE_PROXY_URL=socks5h://alice-ttl-15m-cidr-64:<密码>@127.0.0.1:24601
```

前置条件与代价：

- 需要一个 HE 隧道（`tunnelbroker.net` 免费注册），VPS 的公网 IPv4 要能被 HE ping 通，
  AWS 安全组要放行**协议 41**（入站）和代理端口（回环可不开）
- **所有上游流量都会走这个代理**——不只是领取，还有模型请求和额度刷新。好处是 IP 轮换，
  代价是多一跳 HE 隧道（亚洲机建议选美西节点，RTT 增加约 150～250ms）以及免费隧道的抖动
- 代理挂了网关就完全打不到上游。排查时先把它摘掉：
  `systemctl edit zcode-gateway` 里置空 `ZCODE_PROXY_URL` 再 `restart`

验证出口确实在变：

```bash
curl -s -x socks5h://alice-ttl-1m-cidr-64:<密码>@127.0.0.1:24601 https://ipv6.icanhazip.com
# 隔一分钟再跑一次，应该拿到不同的地址（且不在同一个 /64）
```

## 回滚

```bash
# 单个二进制回滚
systemctl stop zcode-gateway
install -m 755 /opt/zcode-gateway/zcode-gateway.prev /opt/zcode-gateway/zcode-gateway
systemctl start zcode-gateway

# 连数据一起回滚
systemctl stop zcode-gateway
cp -a /opt/zcode-gateway-backups/<时间戳>/accounts.json /opt/zcode-gateway/data/
cp -a /opt/zcode-gateway-backups/<时间戳>/gateway.db   /opt/zcode-gateway/data/
systemctl start zcode-gateway
```
