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
查 latest release → 比对 VERSION 里的 tag → 下载 tar.gz + SHA256SUMS.txt
→ 校验 SHA256 → 解包审查（拒绝含 data/、*.db、*.key、accounts.json、.env 的包）
→ 冒烟（临时端口 3999）→ 备份 data/旧二进制 → 停服务换二进制 → /health 检查
→ 失败则装回旧二进制并重启
```

- 日志：`/var/log/zcode-gateway-autoupdate.log`
- 备份：`/opt/zcode-gateway-backups/<时间戳>/`（保留 7 份，含 SQLite 在线备份与旧二进制）
- 发布包留档：`/opt/zcode-gateway/dist/<tag>/`（含 contrib/、deploy/，保留 5 份）
- 手动触发：`systemctl start zcode-gateway-autoupdate.service`
- 强制重装当前版本：`FORCE_REINSTALL=1 /usr/local/bin/zcode-gateway-autoupdate.sh`
- 只替换二进制和文档，**不碰** `/opt/zcode-relay`、`/opt/zcode-claimer`（独立部署，可能有本地改动）
PUT >94:

9. **首次启动会自动导入桌面端账号**。`store.go` 的 `NewStore()` 在账号表为空时调用
   `ImportDesktop()`，读 `~/.zcode/v2/credentials.json`（ZCode 桌面端登录凭据）解密后
   写入 `data/accounts.json`。所以在装了桌面端并登录过的机器上跑一个全新的二进制，
   面板里会直接出现本机账号——这是上游行为，不是发布包里带了数据。日志会打一行
   `已从桌面端导入 N 个账号（来源 …）`；没有凭据时打 `未发现桌面端凭据 …，跳过自动导入`。

## 面板密码与并发

- **面板密码**：设置页「面板密码」可以直接改，写入 `data/accounts.json` 的 `adminPasswordHash`（bcrypt）
  + `adminPasswordManual: true`。**只有带 `manual` 标记的 hash 才压过环境变量**；首次启动自动生成的
  hash 不带这个标记，否则老部署会被它顶掉 `ZCODE_ADMIN_PASSWORD` 而锁在门外。改密码会踢掉其它浏览器上
  的登录。忘了密码：把 `adminPasswordHash` 和 `adminPasswordManual` 一起删掉再重启，会回落到环境变量；
  两者都没有则随机生成并打到启动日志。
- **并发**：`设置 → 全局默认并发`（1–5，默认 1）是每个账号的默认同时在途请求数；「账号管理」里给某个
  账号单独设了并发就以账号为准（下拉选「跟随」= 用全局值）。账号表「并发」列鼠标悬停能看到实际生效值。
  上游对单账号并发有上限，超过 5 没有意义。


## 错误分级与调度

上游失败不再压成一个 cooldown 数字，而是拆成「要不要重试 / 等多久 / 账号伤不伤」
（见 `classify.go`）。核心判断：**429 是频控不是故障，冷却它等于自己减员**。

| 上游信号 | 处置 | 账号状态 |
|---|---|---|
| 429 频控 | 按 `Retry-After` 原地等（默认 5s，封顶 120s，总预算 `ZCODE_RETRY_WAIT_SEC`=20s），耗尽才换号 | 保持可用，不计连败 |
| 1113 无资源包 | 不等待，直接换号；只记模型级失败 | 保持可用 |
| 3007 验证码被拒 | 换码重试（下一次请求会重新求解） | 保持可用 |
| 内容策略拦截 | 视为 system 指纹误报，换号重试 | 保持可用 |
| 5xx | 冷却 5 分钟后换号 | `cooling` |
| 3012 风控 | 不原地重试，指数退避 15min 起翻倍封顶 24h（`riskStrikes` 累计） | `cooling` |
| 401/403/3004 | 直接停用，等人工重新授权 | `disabled` |

选号从纯随机改成「余额占比 + 闲置时长」加权：先按权重取前 N（`ZCODE_POOL_TOPN`，默认 5）
做加权抽签，短名单之外再按权重降序兜底，保证每个健康账号仍会被试到。

## OpenAI 兼容端点

`POST /v1/chat/completions` 和 `/v1/messages` 并存，重试/降级逻辑共用一份 `relay()`。
入站把 chat.completions 翻成 Anthropic，出站把响应或 SSE 翻回 `chat.completion`：

- 支持文本、`image_url` 的 data URI、`tools[].function`、`role:tool` 的 tool 结果、
  `stop`、`temperature`/`top_p`、`max_tokens`（缺省补 4096，Anthropic 必填）。
- `tool_choice` 上游会忽略，网关仍按规范映射，但别指望它强制调用。
- 流式会把 Anthropic 的 `message_start`/`content_block_delta`/`message_delta`
  翻成 `chat.completion.chunk`，末尾补 `data: [DONE]`。

## 领取退避

`1005 名额用完` 的上游回执带 `next_at` 时，网关把它记进账号的 `claimBlockedUntil`，
`contrib/claimer` 会跳过还没到点的账号；回执没带 `next_at` 才回落到固定 6 小时缓存。
好处是省验证码——每解一次码都要开一次 Chromium。

## 面板安全

- **登录爆破防护**：同 IP 连续失败 5 次锁 5 分钟，来源优先取 `Cf-Connecting-Ip`
  （cloudflared 会带），其次 `X-Forwarded-For` 第一跳。
- **token 不回显**：`GET /api/settings` 只返回掩码（`sk-zc…abcd`）与 `proxyTokenSet`，
  明文走 `GET /api/settings/token`；面板里点「复制」才去取一次。留空＝不修改，
  点「清除」再保存才会清空。
## 发版节奏

`release.yml` 是 tag 驱动的：推一个 `v*` tag 就出一套六平台包。所以**一个 fix 一个 tag 会把版本刷爆**，
之前一轮连发 v1.3.0/1/2 就是这么来的。约定：

1. 改动先合到 `main`，本地跑 `go vet ./... && go test ./...` + `scripts/scan-secrets.sh`；
2. 攒够一批（或要上生产）再打 tag，一次只打一个；
3. 打完等 `Release` workflow 绿，再跑 `zcode-gateway-autoupdate.sh` 上线；
4. 打包内容由 `scripts/verify-package.sh` 把关，它挂了就别发。

`main` 领先于最新 release 是正常的——VPS 只跟 release，不跟 `main`。

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

9. **首次启动会自动导入桌面端账号**。`store.go` 的 `NewStore()` 在账号表为空时调用
   `ImportDesktop()`，读 `~/.zcode/v2/credentials.json`（ZCode 桌面端登录凭据）解密后
   写入 `data/accounts.json`。所以在装了桌面端并登录过的机器上跑一个全新的二进制，
   面板里会直接出现本机账号——这是上游行为，不是发布包里带了数据。日志会打一行
   `已从桌面端导入 N 个账号（来源 …）`；没有凭据时打 `未发现桌面端凭据 …，跳过自动导入`。

## 出口 IP 与风控

上游的 `3012 账号或者网络环境存在异常` 是按**出口 IP** 判的：同一个 IP 连续领取多个账号，
领到第 5～8 个就开始被拒。脚本侧的限速/熔断/缓存只能缓解，根治要换出口。

### 走过的弯路（别再试）

| 方案 | 为什么不行 |
|---|---|
| HE IPv6 隧道池（he-ipv6-pool） | **`zcode.z.ai` 没有 AAAA 记录**，只监听 IPv4，IPv6 出口根本到不了。实测 `getent ahostsv6` 无记录、`curl -6` 返回 000 |
| WARP | 出口是 Cloudflare 的一个共享机房 IP，换门牌不换结构；机房段还更容易被判成代理 |
| 把 `https://` 住宅代理直接填进 `ZCODE_PROXY_URL` | tls-client 在**代理拨号**阶段校验证书，自签且无 IP SAN 会直接失败；`WithInsecureSkipVerify` 只作用于上游 TLS，救不了这一段 |

### 实际采用：本地中继 + 住宅代理池

```
网关 ──明文 CONNECT──► 127.0.0.1:3129（zcode-relay）──TLS(不校验证书)──► 住宅代理 ──► zcode.z.ai
```

- `contrib/relay/` 是中继：对网关是明文 HTTP 代理，对上游代理做 TLS 并跳过证书校验；
  启动和每 10 分钟体检一次，按 TTL 轮换，`GET /rotate` 可按需换 IP
- 网关侧加 `ZCODE_PROXY_URL_CLAIM=http://127.0.0.1:3129`，
  **只有 billing / marketing / claim 走中继**，模型请求和额度刷新保持直连
- 领取脚本加 `ZC_RELAY_ROTATE_URL=http://127.0.0.1:3129/rotate`，每次 claim 前换一个出口

代理池在 `/etc/zcode-relay/proxies.txt`，一行一个 `host:port`（支持 `user:pass@host:port`）。
这批是开放代理，随时会挂——中继的体检会剔除失效的，全挂了就回 502，那一轮放弃、下一轮再来。

回退：删掉 `/etc/systemd/system/zcode-gateway.service.d/claim-proxy.conf` 再 restart，
网关就回到全部直连。

验证出口确实在变：

```bash
curl -s -x http://127.0.0.1:3129 https://api.ipify.org   # 住宅代理的 IP
curl -s http://127.0.0.1:3129/rotate                      # 换一个
curl -s -x http://127.0.0.1:3129 https://api.ipify.org   # 应该变了
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
