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
| **HTTP 200 + 业务码** | 先 peek 响应体判形状（`business.go`）：有 `content`/`choices`/`data` 的一律放行，否则按业务码处理。**不再当成功渲染，也不再 `ResetRiskStrikes`** | 见下面各码 |
| 429 频控 | 按 `Retry-After` 原地等（默认 5s，封顶 120s，总预算 `ZCODE_RETRY_WAIT_SEC`=20s），耗尽才换号 | 保持可用，不计连败 |
| 1005 / 1113 / 1308–1321 额度耗尽族 | 不等待，直接换号；只记模型级失败 | 保持可用 |
| 1302 / 1305 瞬时限流 | 短退避后换号 | 保持可用 |
| 3008 / 3009 / 3010 并发准入族（user 级 / 模型级） | 等 1s 重试（原版客户端也是 1s/2s 退避）；3008 的根治靠每账号在飞上限（OPERATIONS #28） | 保持可用 |
| 529 / `overloaded_error` 上游过载 | 等 1s 重试，模型级（与并发准入族同款；过载是服务器容量问题，落 5xx 会白记冷却。OPERATIONS #31） | 保持可用 |
| 3007 验证码被拒 | 换码重试（下一次请求会重新求解） | 保持可用 |
| 验证码挑战（`x-aliyun-captcha-*` 响应头，或 400/403 上的 captcha 文案） | 同上，**必须排在「403 → 停用」之前**，否则一次人机校验续期就把好号踢掉 | 保持可用 |
| 内容策略拦截 | 视为 system 指纹误报，换号重试 | 保持可用 |
| 5xx | 冷却 5 分钟后换号 | `cooling` |
| 3012 风控 | 不原地重试，指数退避 15min 起翻倍封顶 24h（`riskStrikes` 累计） | `cooling` |
| 401 / 403（非验证码）/ 3004 | 直接停用，等人工重新授权；**例外：HTML 拦截页**（CDN/WAF 边缘封禁，按出口 IP、与账号无关）走 InfraBlock，账号状态不动（OPERATIONS #31） | JSON 认证失败 = `disabled`；HTML 拦截页 = 账号不动 |

业务码解析兼容三种形态（`parseUpstreamEnvelope`）：`{"code":..}`、
`{"error":{"code":..}}`、以及 api.z.ai 回退通道的 `{"error":{"type":"1000"}}`
——**码在 `type` 里**，不读它就等于回退通道的业务码全丢。

## 兼容端点

三个入口并存，重试 / 降级 / 记账逻辑共用一份 `relay()`，只在编解码上有差别：

| 端点 | 入站转换 | 出站转换 |
|---|---|---|
| `POST /v1/messages` | 原样 | 原样 |
| `POST /v1/chat/completions` | chat.completions → Anthropic | Anthropic → `chat.completion` |
| `POST /v1/responses` | Responses → Anthropic | Anthropic → `response` |

**chat.completions**（`openai.go`）：

- 支持文本、`image_url` 的 data URI、`tools[].function`、`role:tool` 的 tool 结果、
  `stop`、`temperature`/`top_p`、`max_tokens`（缺省补 4096，Anthropic 必填）。
- `tool_choice` 上游会忽略，网关仍按规范映射，但别指望它强制调用。
- 流式会把 Anthropic 的 `message_start`/`content_block_delta`/`message_delta`
  翻成 `chat.completion.chunk`，末尾补 `data: [DONE]`。

**responses**（`responses.go`）：

- `input` 支持字符串和 item 数组；`instructions` → `system`；
  `function_call` / `function_call_output` 合并进相邻消息的 content block，
  避免出现空轮次。
- 工具是扁平格式（`{"type":"function","name":..,"parameters":..}`），
  转成 Anthropic 的 `input_schema`。
- 流式事件按 Responses 规范发：`response.created` → `output_item.added` →
  `content_part.added` → `output_text.delta`×N → `output_text.done` →
  `content_part.done` → `output_item.done` → `response.completed`。
- 上游没给 `stop_reason` 就断流时报 `status: incomplete`，不假装成功。

**流式请求的 `input_tokens` 上游一律报 0**。实测 `message_start` 里就是
`{"input_tokens":0,"output_tokens":0}`，非流式才给真实值（同一次请求
非流式 260 / 流式 0）。这是上游行为，不是网关丢数据——按 token 计费的
统计要么走非流式口径，要么用本地估算。

## 端点映射（抗变更）

上游通过 `GET /api/v1/agent/configs` 下发「客户端 base URL → 实际端点」映射。
实测 2026-11-03：

````json
{"data":{"proxyEndpoint":{"mapping":[
  {"from":"https://open.bigmodel.cn/api/anthropic/v1/messages",
   "to":"https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages",
   "zcodeAuthorizationHeader":"X-Zcode-Authorization"}]}}}
````

我们原来把 `ultra` / `ultra-zai` 写死在 `upstream.go` 里，上游一改映射就全挂。
现在 `endpointmap.go` 缓存这张表（TTL 5 分钟、失败 30 秒后重试、超时 3 秒、
上限 256 条），命中就用，没命中回落到硬编码。

三条硬约束：

- **严格 fail-open**：抓取/解析失败保留旧快照，绝不因为拿不到映射就拒绝服务
- **只接受 https 目标**：映射是服务端下发的，但降级到明文的代价太大（JWT 裸奔）
- **`ZCODE_UPSTREAM` 优先级最高**：调试和测试仍然可以强制指定端点，绕过整张表

`zcodeAuthorizationHeader` 标记要求把 zcode JWT 以 `X-Zcode-Authorization`
附到改写后的请求上——coding-plan 那条路径本来就这么做。

start-plan 不走这套：它不在客户端配置里，上游也不会给它下发映射。

## 模型白名单

`ZCODE_MODEL_ALLOWLIST=GLM-5.3,GLM-4.6`（逗号分隔，大小写不敏感），空 = 不限制。

设了之后，客户端请求了不在表里的模型会直接 400，不打上游——上游对不认识的
模型回 400/1005，白白花一次请求还会在账号上留一条失败记录。

只校验客户端**显式传**的 model；不传时用默认值 `GLM-5.3-Flash`，不走白名单。

## 模型清单与思考强度

`GET /api/v1/client/configs` 下发 `builtinModels`（实测 2026-11-03：`GLM-5.3` 和
`GLM-5.3-Flash`，各 1M 上下文 / 128K 输出上限）和 `reasoning.levels`：

````json
{"modelId":"GLM-5.3","contextWindow":1000000,"maxCompletionTokens":128000,
 "reasoning":{"defaultLevel":"max","levels":{
   "low":{"anthropic":{"set":[{"path":["output_config","effort"],"value":"low"}]}},
   "high":{...},"max":{...}}}}
````

`modelcatalog.go` 缓存它（TTL 1 小时、失败 5 分钟重试），三个用途：

1. `GET /v1/models` 不再硬编码，跟着上游走，并带上 `context_window` / `max_output_tokens`
2. `max_tokens` 超过模型上限时钳到上限——上游对超限回 `1210`
3. `thinking_level` 翻译成 `output_config.effort`

**刻意不做**：不用 `builtinModels` 自动生成白名单。它只有 2 个内置模型，而
`providers[].models` 里还有 `GLM-5.2` / `GLM-5-Turbo`——拿内置清单当白名单会把
合法请求误杀。白名单仍靠 `ZCODE_MODEL_ALLOWLIST` 手工配。

思考档位实测（同一道行程题，`max_tokens` 2000）：

| `thinking_level` | thinking 字符 | 输出 token |
|---|---|---|
| `low` | 12 | 243 |
| `high` | 20 | 267 |
| `max`（默认） | 690 | 514 |

`output_config.effort` 直接写进请求体也能生效（`sanitizeForUpstream` 不删它）；
客户端自己带了 `output_config` 时网关不覆盖。

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

## 账号自动启停

`contrib/claimer` 每轮先跑 `manage_pass`，再走领取。判定依据是账号的额度状态：

| `quota.source` | 含义 | 自动启停怎么用 |
|---|---|---|
| `api` | 上游返回了余额，有额度 | 可据此启用 |
| `api-empty` | 上游明确回了空列表（确实没套餐） | 连续 `ZC_EMPTY_ROUNDS` 轮就禁用 |
| `local` | 没查成功（网络/鉴权），额度未知 | **不据此禁用**，避免误杀 |

`api-empty` 是 v1.8.0 才分出来的。之前只有 `api` / `local` 两种，没套餐的号永远是 `local`，
脚本的「数据新鲜」条件永远不满足，于是这批号一直没被自动禁用——概览页那 17 个「无套餐但启用」
就是这么攒下来的。

领取成功**不等于**马上能用：部分活动有生效时间，要等到某个时间点额度才到账。
所以脚本领完会再刷一次额度，`remaining > 0` 才 PATCH `enabled=true`；没到账的记进
`pending_activation`，等后续轮次额度出现再自动启用（`ZC_ENABLE_AFTER_CLAIM=0` 可关整段）。
不管当初是谁禁用的（脚本/迁移/手动），只要额度真到账就启用。

### 中继体检为什么要"真发一次请求"

中继原来只测「能不能建到 `zcode.z.ai:443` 的隧道」，结果 41 条代理全绿、实际一个都转发不了
（能建隧道但一过真实 TLS 就断）。现在 `check_entry` 会完整走一遍：连代理 →（https 代理先
TLS）→ CONNECT → 对目标再做一次 TLS → 发 `GET /api/v1/client/configs` → 看状态码。
只回 200 不转发的"假健康"代理会当场被判死。切过去之后同一批代理 0/41 通过。

### 顺带修掉的两个坑（2026-11-02）

1. **体检探针不认 socks5**：完整链路探针一开始只走 HTTP CONNECT，socks5 代理会报
   「CONNECT 无响应」。现在按 scheme 分支：socks5 走 SOCKS5 握手（含账号密码），
   http/https 走 CONNECT。
2. **转发路径解包错了**：`open_tunnel` 返回 `(reader, writer, reason)` 三元组，
   CONNECT handler 里写的却是 `up_reader, up_writer = r`，于是**每次 CONNECT 都抛
   `ValueError`**——隧道刚建好就断。这个 bug 跟代理质量无关，任何代理都转发不出去，
   所以之前"代理全绿但请求全 502"是两个问题叠在一起。改成三元组解包后，
   `curl -x http://127.0.0.1:3129 https://zcode.z.ai/...` 从 HTTP 000 变成 200。
3. **Python 不会嵌套 TLS**：对已经是 SSLSocket 的连接再 `ctx.wrap_socket()`，外层会拿底层
   fd 裸发数据——目标的 ClientHello 变成明文，对端只能回 `INVALID_ALERT` /
   `UNEXPECTED_MESSAGE`。这就是「curl 能过、我们体检不过」的原因（curl 自己会嵌套）。
   修法是用 `ssl.MemoryBIO` + `SSLObject` 手动泵字节（见 `_TLSOverStream`）。
   注意这只影响**体检探针**；中继的转发路径是纯字节管道，不受影响。

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

4. **选号看不看额度分两层**。账号级 `p.healthy` 检查 `Granted>0 && Remaining<=0`，
   额度耗尽的号不会被派发（2026-11-11 确认，早年的「不看额度」已修）；但缓存有
   30 秒滞后，刚用完的号仍可能吃一次 1005——那会触发强制额度刷新（见第 27 条）。

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

10. **客户端版本不能盲目跟进**。2026-10-31 试过把 `ZCODE_APP_VERSION` 从 3.16.3 提到官方桌面端
    当时的 3.17.1：同一个账号先用 3.16.3 发请求正常，隔 30 秒换 3.17.1 立刻被上游回
    `3012 request has been blocked due to unusual activity`，换回 3.16.3 又恢复正常。
    也就是说版本号本身是上游的校验项，**不是越新越好**。升级流程：

    1. 复制一份 `data/` 到临时目录，只保留一个健康账号（其余 `enabled=false`）；
    2. 用新版本跑临时实例（换端口），发一次真实请求；
    3. 通过才改 `runtime.go` 的默认值；失败就保持原版本。

    务必只留一个号：网关的重试循环会把 3012 扩散到池子里的其他账号。

    2026-11-07 又验了一遍 3.17.2（干净池、只导 10 个号）：3.16.3 基线两次 200，
    只开 3 块 system 形态两次 200，**只把版本改成 3.17.2 立刻 405/3012**，之后池子
    全进 15 分钟冷却。同一天也确认「2 块 system」本身不触发 3012——之前怀疑的形态
    问题在 start-plan 上不成立，所以 `ZCODE_SYSTEM_SHAPE=3` 只作为灰度开关保留，默认关。

    **但 3.17.2 本身是可行的**：同一天在 aws3 上跑 7sen1ot/zcode2api（v1.6.1）和
    gakiyukr/zcode2api-plus（v2.1.0）——两家内嵌的 `zcode_system.json` 是同一份官方
    3.17.2 提示词（首块 `You are ZCode, an interactive agent`，注意没有 "coding"），
    zcode2api-plus 跳过它自己的验证码门槛后，用 3.17.2 全套形态 + 真实 JWT 拿到 200
    （input_tokens 1079）。把它的 `ZCODE_CLIENT_VERSION` 改回 3.16.3 再打，同一个号
    立刻 3012。结论：**版本号和提示词形态是绑定的，必须整套一起换**，单独改版本号
    必吃 3012。要升 3.17.2 就得把三段提示词、身份头（X-ZCode-Agent / HTTP-Referer /
    X-Title / X-Release-Channel / X-Client-Language / X-Timezone / X-Platform /
    X-Os-Category / X-Os-Version）、UA（`ZCode/3.17.2 ai-sdk/anthropic/3.0.81`）和首个
    user turn 前的日期 reminder 一起搬过来。

    **2026-11-22 复验：这条绑定关系已经不成立了。** 在 aws2 用 10 个账号的隔离实例
    做了三组对照（每组 4 个真实请求）：3.17.2 + 317 三段提示词 → 4/4 = 200；
    只把 `ZCODE_APP_VERSION` 改成 3.19.1、提示词完全不动 → 4/4 = 200。

    **2026-11-27 推翻：3.19.1 头现在有问题，已回滚 3.17.2。** aws3 用真实账号做
    交替对照（同一账号、同一二进制、四轮）：`3.19.1 → 1005`、`3.17.2 → 200`、
    `3.19.1 → 1005`、`3.17.2 → 200`，**100% 复现**——3.19.1 头会落到另一个权益桶，
    账号看起来「额度耗尽」（1005），随后上游下发
    `405 code 3012 "Your account has been suspended for misusing the Start Plan,
    such as proxying it to non-ZCode clients"`（生产 6 个号中招，其中 2 个在改回
    3.17.2 后自愈）。把 3 个「封停」号导入独立实例、用 3.17.2 复测：**8/8 全部 200**
    ——**该文案是形态问题的误导性错误信息，不是真封停**。
    处置：删掉 drop-in `version319.conf`（317 形态自己默认 3.17.2），代码里加了
    启动警告：317 形态下版本号 ≠ 3.17.2 就打印警告（zcode317.go init）。
    外部佐证：dengyie/zcode2api#16（3.17.2→3.19.1 正文锁 + 拆包比对）、
    magpie-community/plugins#1559（「the error message misleads」，账号在官方客户端仍可用）、
    zai-org/feedback#1610/#716（官方渠道同症，纯官方客户端也有）。

    这不代表以后也不会收紧：升级仍要走上面的单账号灰度流程。另外，**升版本号对
    WAF 405 毫无帮助**——同条件下 3.17.2 和 3.19.1 的「每次成功摊到的 405」都是
    9.2~10.0，差异在噪声内（见「并发上限」一节）。

    **2026-11-22 官方客户端取证：3.19.1 的提示词文本与 3.17.2 完全相同。**
    所以「有没有更新的提示词」这个问题现在有定论了——没有。取证方法可复用：

    ```bash
    # 1. 版本号从 https://zcode.z.ai/releases 解析，下载对应平台的官方客户端
    curl -sSL -o zcode.AppImage \
      https://cdn-zcode.z.ai/zcode/electron/releases/3.19.1/linux-x64/ZCode-3.19.1-linux-x64.AppImage
    # 2. AppImage 自带解包，不需要 FUSE 或 unsquashfs
    chmod +x zcode.AppImage && ./zcode.AppImage --appimage-extract
    # 3. 提示词在 resources/glm/zcode.cjs（15.8MB，md5 05733647823ffcf6d755fc6f4fee1a9f）
    ```

    比对结果（`ZCode-3.19.1-linux-x64` 的 bundle，md5 `05733647823ffcf6…`）：

    | 段落 | 官方 3.19.1 | 我们 317 | 结果 |
    |---|---|---|---|
    | CLI Prefix | `You are ZCode, an interactive agent` | 同 | 逐字相同 |
    | Harness | 673 字符（`e=true` 分支） | 同 | 逐字相同 |
    | Dynamic Behavior | 975 字符（qoc+Woc+Voc） | 同 | 逐字相同 |
    | Desktop Context | 存在 | 有这一段 | 一致 |

    官方 harness 按参数分两支，我们用的正是默认的 `e=true`：

    ```js
    ` - ${e ? "The system may send updates... via mid-conversation system turns..."
            : "`<system-reminder>` tags ... injected by the harness..."}`
    ```

    **唯一差异是行尾**：官方 harness 用 ``.join(`\r\n`)``，我们用 `\n`（同内容差 5 字节）。
    上游不校验这个（我们一直是 200），不做改动。

    结论：**不存在「3.19.x 新提示词」**。3.16.x → 3.17.2 → 3.19.1 这段区间官方没改文本，
    所以 `3.19.1 版本号 + 317 文本` 是内容自洽的，不是「旧文本配新版本号」。

    旁证（2026-11-22 查 GitHub 活跃同类项目）：

    | 项目 | 星 | 更新 | messages 版本策略 | 提示词 |
    |---|---|---|---|---|
    | dengyie/zcode2api | 165 | 11-21 | `PROMPT_ALIGNED_VERSION="3.17.2"`；messages 保 3.17.2、billing 抬 3.18.2 | 与我们逐字段相同 |
    | 7sen1ot/zcode2api | 158 | 11-22 | 只出现 3.17.2 | 同一份（数组格式） |
    | TriDefender/zcode-api | 577 | 11-20 | 最高 3.17.1 | 缺 Desktop Context 段 |
    | B3hnamR/AutoZcoder | 53 | 11-17 | 3.18.2 | 3.16.x 抓包 |

    引用 AutoZcoder 时注意分寸：它的 `PROTOCOL.md` 标题是
    `(3.16.x captures, 3.18.0 client default)`，release notes 里也写明回归测试
    「**do not establish the server's matching rules**」「不能证明上游接受未改动的旧客户端」。
    所以它不是「3.19.x 已适配」的证据，只是同源文本的旁证。


    另一个坑：zcode2api-plus 对 `mode=jwt` 的账号**强制先解验证码**（`needsCaptcha :=
    acc.Mode == "jwt"`），没有浏览器求解器就直接 503 `captcha_required`、根本不发上游。
    所以拿它做形态验证必须先跳掉这一步，否则看到的 503 是它自己的门槛，不是上游拒绝。

    本仓库已按这套形态实现，开关 `ZCODE_SYSTEM_SHAPE=317`：提示词在
    `zcode317_prompt.json`（逐字节照抄 MIT 来源），`zcode317.go` 负责三块拼装、
    日期 reminder、cache_control 重排和官方 CLI 身份头，`appVersion` 在没显式
    指定时自动跟着切 3.17.2。aws3 单账号灰度：3.16.3 基线 `input_tokens=257`、
    两次 200；切 317 后版本变 3.17.2、两次 200、`input_tokens=1076`（与
    zcode2api-plus 的 1079 同一量级，差异来自 environment 段和日期）。
    **默认关**：换默认值前必须再走一遍单账号 A/B——提示词整段换掉会改变所有
    客户端的模型行为，不是纯兼容性开关。

    2026-11-07 15:42 aws1 生产已切 317：先用「只留一个号的 data 副本 + 临时实例
    :3001」灰度（两次 200、`input_tokens=1074`），再给主服务加 drop-in
    `/etc/systemd/system/zcode-gateway.service.d/shape317.conf`
    （`Environment=ZCODE_SYSTEM_SHAPE=317`）。切换后 `/health` 的 `version` 变
    3.17.2，生产请求 200、`input_tokens=1074`，5 分钟内无 3012/405，64 个号全部
    健康。回滚：删掉该 drop-in + `systemctl daemon-reload && systemctl restart`。

11. **活动接口的客户端版本要跟着官方发版走**。2026-11-03 实测：同一账号同一套头，只改
    `app_version`——`3.15.0`/`3.16.3`/`3.16.4`/`3.17.0` 全部 `deliveries:[]` +
    领取回 `1004`，`3.17.1`/`3.18.0` 才有横幅、领取才 200。写死一个版本号，上游抬一次
    门槛就要改一次代码。现在 `claimversion.go` 从官网 `https://zcode.z.ai/releases` 页
    解析最新版本（实测拿到 `3.17.2`），6 小时刷新一次，失败回落内置下限
    `claimVersionFloor`；`ZCODE_CLAIM_APP_VERSION` 可以强制指定（此时完全不联网）。
    **诊断口径：活动横幅为空或领取回 1004 时，先看 `/health` 里的
    `claimVersion.version` 是不是落后了**，再怀疑账号资格。

12. **解验证码失败不等于账号没资格**。`solver_pw.js` 退出码 4 = 三次尝试都没产出
    `verifyParam`（`--single-process` 的 Chromium 在共享小内存盒上会闪退）。
    2026-11-03 首轮 64 个号有 5 个栽在这上面，手动轮换中继出口后 5/5 一次通过。
    以前 `solveCaptcha` 失败直接记账跳过，现在 `ZCODE_CLAIM_SOLVER_RETRIES`（默认 1）
    会换出口重来，并把 node 的 stderr（`[pw] 第 N/3 次…`）带进错误信息——
    只报 `exit status 4` 分不清是内存不够还是页面没加载出来。

13. **领取时段的起点要贴着开闸时间**。活动 19:00 开闸，claimer 原来 `:17` 才跑第一轮，
    等于白丢 17 分钟；64 个号一轮 40+ 分钟，火爆的活动这时可能已经领完。现在 timer 改
    `:03`，默认时段 `19-21`（19:03 抢名额 / 20:03 补首轮失败 / 21:03 兜住晚开的活动）。
    已经领到的号在本地比对后直接跳过，多排一轮几乎不花时间。

14. **HTTP 200 也会承载业务失败**。没套餐的号回 `{"code":1005,"msg":"exceed quota limit"}`，
    额度耗尽的 13xx 族同理。原来 `handleAttempt` 只看 status，200 就直接渲染 +
    记成功 + `ResetRiskStrikes`——错误 JSON 塞给客户端、日志里全是成功、
    还把 3012 的 strike 计数洗掉（指数退避永远升不上去）。现在先 peek 开头 4KB
    判形状（`business.go`），有 `content`/`choices`/`data` 的一律放行，
    避免把正常响应误判成错误。

15. **403 不一定是认证失败**。阿里云验证码挑战会回 400/403 + `x-aliyun-captcha-*`
    响应头，body 里没有 3007。原来 `classify.go` 把 401/403 一律 `Disable`，
    等于一次人机校验续期就把健康号停用 30 分钟。现在验证码判定排在状态码之前，
    头、3007、文案三条证据任一条命中都按「换码重试」处理。

16. **流式请求的 `input_tokens` 不在 `message_start`**。那里恒为
    `{"input_tokens":0,"output_tokens":0}`，真实值（实测 258）在最后的
    `message_delta.usage` 里。任何新写的流式 usage 解析都得从 `message_delta`
    取 `input_tokens`，否则成本系统性少记——`/v1/responses` 和
    `/v1/chat/completions` 的翻译器 2026-11-07 才补上。

17. **`prompt_cache_key` 默认关闭**（`ZCODE_PROMPT_CACHE_KEY=1` 才开）。业界实测
    这个字段能把 8k 前缀的缓存命中率从 0 拉到 7808 token（费用差约 17 倍），
    但上游对未知顶层字段的容忍度没实测过，贸然加一旦被拒就是全站 400。
    构造里掺了账号 uid，防止跨账号命中别人的前缀缓存。

18. **禁用的号不会被定时刷新**。`refreshAllQuotas` 原来只刷 `enabled` 的号，而
    `api-pending` 的号正是禁用的——「额度没到账 → 禁用 → 不刷新 → 永远发现不了
    到账」死锁。claimer 侧有兜底（判断前先 `POST /api/accounts/{id}/refresh`），
    所以线上没炸；面板和直接调 API 的用户会一直卡在「待生效」。现在禁用的号只要
    `source=api-pending` 就继续刷，真正停用的（401 / 手动禁用）不刷。
    11-03 实测：23:04 那轮 claimer 逐个刷新后确认 40 个号各 3 亿到账，全部自动启用。

19. **`/v1/responses` 必须处理 thinking block**。GLM 先发 thinking 再发 text，
    只处理 text 会让「思考把 max_output_tokens 吃满」的回合返回空 output——
    客户端以为模型没回话。现在 thinking 转成 Responses 的 reasoning item。
    实测 32 token 时确实空，400 token 才正常。

20. **`ZCODE_UPSTREAM` 是端到端测试的唯一入口**。`gateway_test.go` 里所有
    真发请求的用例都靠它把上游指到 `httptest`。改端点选择逻辑（比如加映射表）
    时如果把 override 弄丢，测试不会明显报「端点算错了」，而是全部变成 401
    ——请求打到了真实上游，用假 token 被拒。看到一批测试同时 401 先查这里。

21. **`zcode-gateway --version` 曾经会启动服务**。`main()` 早期完全不看
    `os.Args`，aws1 上有人敲 `--version`，结果在默认端口 8787 上留了一个
    跑三天的幽灵实例（0 账号、build `1.6.0-test`），直到查端口才发现。
    现在参数在创建 store 之前处理：`-v/--version`、`-h/--help`，其余一律
    拒绝启动（exit 2）。排查「端口被占 / 账号数为 0」时先看 `ss -ltnp`，
    别假设只有一个实例在跑。

22. **OpenAI 兼容端点曾经不把 `model` 带给上游**。`openAIRequestToAnthropic` /
    `responsesRequestToAnthropic` 只翻消息体，`model` 一直是空的，`forward()`
    于是回落到 `GLM-5.3-Flash`：客户端点 `GLM-5.3` 实际跑的是 Flash，模型清单
    的 `max_tokens` 钳制和思考档位翻译也全都失效（`applyModelCatalog` 靠
    `model` 找清单）。2026-11-07 修，两个转换器都显式带上 `model`，端到端
    测试断言上游收到的是客户端点的那个。

23. **`metadata.user_id` 是 JSON blob，且前缀缓存跟会话 id 无关**。官方客户端的
    形状是 `{"device_id","account_uuid","session_id"}`，上游接受（2026-11-07 实测
    200）。但缓存命中是**按内容**算的：同一个 8.8k 字符 system 前缀，换会话头、
    甚至不带会话头，第二次起都是 `cache_read_input_tokens=5248 / input_tokens=5262`。
    所以别把缓存命中率归因于 `X-Session-Id` 稳定化——它的作用是让上游认出
    「同一段对话」，不是缓存的开关。

24. **`UpdateSettings` 先写盘、失败回滚内存**。原来是「改内存 → 尽力落盘 → 忽略
    错误」：写失败时界面显示改成功、进程按新值跑，重启后又变回旧值，看起来像
    设置随机丢失。现在落盘失败就回滚并记日志，等于这次修改没发生。

25. **上游不拒未知 `anthropic-beta`，但我们仍按白名单透传**。2026-11-07 实测：
    `totally-made-up-beta` 在 `ZCODE_PASSTHROUGH_BETA=1` 下也是 200，所以白名单
    不是防 400，而是防「把客户端的能力开关原样转发、我们却兑现不了语义」。
    目前放行 `mid-conversation-system`；同一天验证带不带这个 beta、中途 system
    轮次都是 200，说明上游对这块比较宽松。

26. **业务错误要换成真实状态码**。上游用 HTTP 200 承载 `{"code":1005}` 这类失败，
    池子里所有号都试过仍失败时，原来把 200 原样回给客户端——OpenAI SDK 会把错误
    体当成空回复，连异常都不抛，排障时只看到「模型不说话」。现在按业务码映射：
    额度/限流/模型并发 → 429，风控/会话过期 → 502。实测没额度的号三个端点都从
    `200 + {"code":...}` 变成 `429`，错误消息保留。

27. **额度类失败不许把模型熔断推到 Disabled**。模型级熔断原来是 3 次失败升一档，
    stage3 永久禁用——但 Disabled 的 (账号,模型) 对不会再被派发请求，永远等不来
    能清掉它的成功。2026-11-10 实测：5 号站 Start Plan 额度耗尽触发熔断，次日额度
    恢复后仍显示「GLM-5.3-Flash · 已禁用」。修复（对齐 zcode2api-plus 的
    markModelExhausted + fireRefresh + SyncExhaustedModels）：
    1) `RecordModelFailureCapped`：1005/13xx 封顶 stage2（10 分钟冷却），不 Disabled；
       网络/鉴权类失败仍走原升级路径。
    2) `forceQuotaRefresh`：1005 命中立刻异步刷一次真实额度（按号去重，30s 一次）。
    3) `healModelStates`：额度刷新时**按模型**清熔断——只清「快照里该模型剩余>0」
       的，快照缺条目或缺分模型数据时保守不动，记「模型熔断解除」事件——
       已存在的 Disabled 状态也由此自愈。
    线上已有的 Disabled 记录不用手动清：部署后首轮额度刷新（≤30s）就会清掉。

    2026-11-11 补丁（当天线上抓到的反例）：上游把 Start Plan 8M 分成
    GLM-5.3 3M + GLM-5.3-Flash 5M 两个独立桶。某号 Flash 桶耗尽但账号合计
    还剩 3M：选号只看合计一直放行；v1.16.1 的账号级 heal 更是把 Flash 的
    10 分钟冷却每 30 秒洗一次 → 无限重试「失败 200」。修复：选号与 heal
    都改用 `Quota.Models` 分桶数据（`quotaModelExhausted` / `modelRemaining`），
    快照缺条目按「未知」放行，绝不过滤空池子。

28. **3008 也是并发准入族**（2026-11-18 吸收，dengyie 2.7.44 / hj01857655
    双源确认）。3008 是 **user 级并发上限**（同一账号并发挤爆），语义与
    3009/3010 同族：账号没坏。原来它是未知码——若上游用 400 承载，会落进
    「不重试」直接甩给客户端，且不记 `tried` 原号无退避连打。现在与
    3009/3010 同处置：模型级、等 1s 重试、不冷却。根治本就在调度层：每账号
    在飞上限（默认并发 1、上限 5）早已掐死触发条件，所以 aws1 生产 14 天
    日志 0 次 3008——这条是潜伏路径加固，不是线上救火。dengyie 的「重试
    指数退避（3/6/12/24/30s）」没吸收：它有 935 次重试的生产数据支撑，
    我们没有；等自己的日志出现 3008 且分布证明白等再议。

29. **`/v1/responses` 工具结果的 content-part 数组**（2026-11-18 吸收，
    TriDefender/zcode-api 31f4d6e 同款修复）。Responses 规范允许
    `function_call_output.output` 是数组：`input_text`/`input_image` 混排
    （工具回图给模型看截图）。原实现只认 text/content 键——图片静默丢弃、
    结构化对象变空串。现在：结构化对象一律 JSON 序列化；文本件拼接行为
    不变；图片件在 `ZCODE_TOOL_RESULT_IMAGES=1` 时翻成 `tool_result` 里的
    image block（复用 `imageBlock`，只收 data URI），**默认关**——给上游
    加新 block 形态必须先灰度，生产开一轮无异常再议默认值。认不出的件
    降级成 JSON 文本，内容不丢。

30. **多礼包可同时挂，整号跳过的语义是「全部拥有」**（2026-11-18 吸收，
    magpie 上游插件源码 packages/zcode #503）。ZCode 的活动可同时提供多份
    礼包（横幅、预览、可领列表）。claim job 的省流跳过原来是
    `accountOwnsAny`——拥有礼包 A 的号在礼包 B 还可领时也被整号跳过，
    B 永远领不到。改成 `accountOwnsAll`：activePlans 里每一份都拥有才跳过；
    单礼包场景优化不变（拥有 A = 拥有全部 {A}），多礼包场景继续领下一个。
    副作用可控：拥有部分礼包的号会多打一次 detectActivities（仅多礼包时
    发生）。单测 `TestAccountOwnsAllClaimable` 钉住改前/改后差异。

31. **两条上游「非账号伤害」瞬态族**（2026-11-18 吸收，AutoZcoder）。
    本项目的 `InfraBlock`（WAF 405 HTML 页）家族再扩两员，处置都是
    账号状态一概不动：
    1) **CDN/WAF 边缘封禁页以 403 出现**（AutoZcoder breaker.py，
       2026-09-16 捕获："Your client does not have permission to get
       URL ..."，按**出口 IP** 生效）。原来 403 → 停用，一次 IP 级封禁
       会逐号清空整池；现在 401/403 的 HTML 拦截页走 InfraBlock，纯
       JSON 认证失败仍停用（`isUpstreamWAFBlock` 的 "{" 前缀排除兜底）。
       aws1 生产 20 天日志 0 次出现，潜伏路径加固。
    2) **529 / `overloaded_error` 上游过载**（Anthropic 族语义，AutoZcoder
       model_retry.py）：服务器容量问题，落 5xx 会白记 5 分钟冷却；
       现在归并发准入族（模型级、等 1s、不冷却）。
    测试：`TestIsUpstreamWAFBlock` 新增 CDN 403 页/JSON 误伤两行；
    classify 表新增边缘封禁/529/overloaded_error 三例。

32. **「suspended for misusing」文案不等于真封停**（2026-11-27 生产事故 + 外部考证）。
    现象：6 个号返回 405 + code 3012 +
    "Your account has been suspended for misusing the Start Plan, such as
    proxying it to non-ZCode clients."，额度仍显示 3 亿。
    根因：drop-in 把 `ZCODE_APP_VERSION` 单独升到 3.19.1（2026-11-22 那次「复验」
    的结论），而 3.19.1 头会落到另一个权益桶——同一账号交替测试 100% 复现
    `3.19.1 → 1005` / `3.17.2 → 200`。1005 又触发强制刷额度，进一步制造异常流量。
    验证：3 个「封停」号导入独立实例用 3.17.2 复测 → 8/8 = 200（没有真封停）。
    代码处置：
      - 文案识别 → `Suspended` 判定，排在业务码之前（`suspendedVerdict`）；
      - 处置是 **6 小时冷却 + 事件「上游封停文案」**，不是永久停用（会误杀可恢复号），
        也不是 Risk 的 15 分钟节奏（那会把软拦截推成真封停，见 magpie plugins#1559）；
      - 317 形态下版本号 ≠ 3.17.2 时启动打印警告。
    教训：**版本号不是「改成最新」就行**，它决定上游给哪个权益桶；
    任何版本变更必须单账号交替对照（3 轮以上）再上生产。

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

### 2026-11-22 事故：8 并发打挂全池，以及两道护栏

现象：客户端起 8 个子代理并发，06:33 起 `POST /v1/messages` 开始 405（3012 装在 405 里），
06:36 起全变 503 `no available account`，`/api/accounts` 里 **42/42 全部 `lastError=上游风控 3012`**。
24 小时里此前有 531 次 200，说明不是号坏了，是被一件事一起打挂的。

两层原因，必须分开看：

1. **触发面**：模型请求直连 aws1（只有 billing/claim 走中继），42 个号共用同一个机房 IP。
   8 个并发 = 同一个 IP 上多账号同时出网，这正是上游跨账号关联风控的典型特征
   （同类项目 AutoZcoder 的行话：*spreading one model across every account at once
   is what tends to get a whole pool flagged together*）。
2. **放大面（真正的坑）**：`relay()` 的 `maxAttempts = len(accounts)+1`，且 3012 走
   `applyVerdict` 的 `Risk` 分支 → 每个失败尝试都给该号 `BumpRiskStrikes` +
   `MarkExhausted`（15 分钟起、逐次翻倍）。于是 **6 个坏请求顺着轮转把 42 个号逐个
   打进冷却**，一个人的请求造成全池下线。

护栏（已上线，各自有测试 `risk_test.go`）：

| 护栏 | 行为 | 旋钮 |
|---|---|---|
| 形态问题不记账号账 | 3012 文案是 `not supported` / `官方客户端` 时判为**构建级**故障（每个号都会拒），不 strike、不冷却，直接 400 快速失败 | 无（按文案识别） |
| 风控轮转上限 | 单个请求最多换 3 个号就停（原来是全部 42 个），返回 502 并提示检查出口/并发/形态 | `ZCODE_RISK_MAX_SWITCHES`（默认 3） |
| 冷却封顶下调 | `riskCooldownMax` 24h → 4h，误伤当天可恢复 | 代码常量 |

配套操作建议（按性价比排序）：

1. **并发别一次开满**：8 路并发在单 IP 上等价于「多账号同时出网」。降到 3~4 路是最省事
   的办法。**不要**为此把 `egressMode` 切 `always`——下一节实测结论是 `always` 因为 RTT
   高 + 池内 IP 本身脏，反而更容易吃 3012；保持 `fallback`（直连出事才换住宅出口重打一次）。
2. **出事先清冷却，不要重启**：`ExhaustedUntil` 是持久化的，重启不清。
   面板按号点「清除冷却」，或 `POST /api/accounts/<id>/clear-cooldown`
   （清 `ExhaustedUntil` + `LastError` + **`RiskStrikes`** + 模型态）。不清 strike 的话
   账号会停在「第 N 次」的退避档位上，下一次 3012 直接从 30m/1h 起跳——2026-11-22
   实测：清过冷却的号下一次风控直接写「第 2 次，冷却 30m0s」。已经自然恢复（不在
   「冷却中」）但 `riskStrikes>0` 的号也要对全部号跑一遍才能清干净。
3. **看错误先看 405 不是 3012**：上游把 3012 装在 HTTP 405 里，日志里 grep `3012` 是 0 命中。
   现在换号重试的中间失败也会落一条 `ok=false` 的日志（`error` 以 `换号重试：` 开头），
   不再只存在于内存里。



### 2026-11-22 第二次复盘：唯一真凶是 egress 失败路径，不是并发

第一轮护栏上线后，用户把并发降到 **2 个子代理**，仍然 1 分钟内 20 个号进冷却。日志层
证据有两处「看不见」：

- `/api/logs` 默认排除 `mode=account`，要 `?status=account` 才看得到账号事件；
- 换号重试的中间失败**既不写日志表也不进 journal**（journal 只记客户端最终状态码），
  所以 07:50–08:00 那一分钟里客户端 25 个请求全是 200，而同期 20 个号被写了风控退避。

用真实账号做了直连 / 住宅代理的对照实验（同一 JWT、同一身份头），结果两种 405 完全不同：

| 出口 | 状态 | 响应体 |
|---|---|---|
| 直连 aws1 | 405 | **阿里云 WAF 的 HTML 页面**（`<!doctypehtml>` + `data-spm` + `<title>405</title>`），0.3s 返回，没进业务层 |
| 住宅中继 | 405 | 业务 JSON `{"code":3012,"msg":"This client is not supported..."}` |

而账号事件里的 `Reason` 全部是字面量 `上游风控 3012`——那是 `classifyUpstream` 的 **fallback
文案**，只在 body 里没有 `msg` 时出现，即**那 20 条全是 WAF 的 HTML 405**。完整链条：

1. 直连被 WAF 挡回 HTML 405 → `classifyUpstream` 落到 `status == 405` 分支 → `Risk = true`；
2. `Risk` 触发 `egressMode=fallback` 的住宅代理重试；
3. 代理这一发**网络层失败**（`err2 != nil`）→ 走 `s.applyVerdict(acc, verdict)`，用的是
   **直连那次 WAF 的结论** → `BumpRiskStrikes` + `MarkExhausted`；
4. **这条路径不消耗 `riskSwitches`**，所以 `ZCODE_RISK_MAX_SWITCHES=3` 完全拦不住它——
   一个客户端请求就能顺着轮转写到 `maxAttempts = len(accounts)+1`。

2 个并发 = 2 × 43 次尝试机会，足够把 20 个号打掉。并发从来不是自变量。

修复（`classify.go` / `handlers.go`，测试在 `risk_test.go`）：

| 改动 | 行为 |
|---|---|
| `isUpstreamWAFBlock` | 405 且 body 是 HTML（非 JSON）时判 `InfraBlock`：`Retry` 但**不 `Risk`**，`applyVerdict` 直接 return，连 `LastError` 都不写 |
| egress 失败不记账 | `err2 != nil` 时只 `lastErr = err2` 并计数，**不再 applyVerdict**；连续失败达 `ZCODE_EGRESS_MAX_FAILURES`（默认 3）返回 502 快速失败，提示检查中继 |
| `clear-cooldown` 清 strike | 顺带把 `RiskStrikes` 归零，避免下次风控直接跳到 30m 档 |
| 中间失败落日志 | `handleAttempt` 的 `verdict.Retry` 分支记一条 `ok=false`、`error` 前缀 `换号重试：` 的上游尝试 |

上线后实测（3 个真实请求）：每个请求先吃 1 次 WAF 405，走住宅代理后**同一个号** 2 秒内
回 200，`riskStrikes` 全程 0，池子 42/42 可用。

### 2026-11-22 第三轮：405 的主因是并发，`egressRotate` 是独立的放大器

第二轮收尾时我写过「aws1 的直连 IP 已被 WAF 拦掉（3/3 直连必 405）」，**这个结论下早了**。
后续数据推翻了它：

- 06:32 起的 3008 风暴之后，07:xx、08:xx 的出网 IP 没变，直连照样有大量 200；
- 08:26:46–08:35:11 是 137 次 WAF 405，08:35:11 之后**立刻归零**并持续十几次成功；
- 而 `egressRotate` 到 08:44 才关掉。**405 停得比 rotate 早**，所以 rotate 不是 405 的源头。

正确的归因：**并发高 → 同一 IP 上的请求速率被 WAF 限速（HTML 405）+ 业务层报 3008；
并发降下来就恢复。** 之前那次「3/3 直连必 405」的对照实验，payload 是裸的最小请求
（缺 317 三段 system 与完整身份头），本身就不代表网关真实流量，不能拿来给 IP 定性。

`egressRotate` 是另一个独立缺陷，表现为放大伤害而不是触发：

| 现象 | 数据 |
|---|---|
| 轮换次数按小时 | 05h 4 次 → 06h 109 → 07h 56 → **08h 214** |
| 中继自身 TTL | 3000 秒（50 分钟才换一次 IP） |
| 后果 | `/rotate` 主动断开正在传输的隧道 → 单请求耗时 47~54 秒，成片 502 |

修复：

1. `rotateEgress` 加节流 `ZCODE_EGRESS_ROTATE_MIN_SEC`（默认 30 秒，设 0 关闭节流）。
   用 `envSecondsAllowZero` 读取——`envInt` 把 0 当非法值，会让「设 0 关闭」静默失效
   （`TestRotateEgressWindowExpires` 就是抓这个的）。
2. `ZCODE_EGRESS_MAX_FAILURES` 默认 3 → 10：中继抖一下不该让客户端看到 502，
   账号是好的，多换几个号更划算。
3. 线上直接把面板的「兜底前先换出口 IP」关掉了（`egressRotate: false`）——
   中继自己有 TTL，网关这边按请求轮换纯属负收益。

实测见下一节「并发上限」：2 路持续 10 分钟即可触发封禁，且换版本号 / 换形态 /
换 CDN 出口都无效，只有独享住宅 IP 明显更好。

## 并发上限（实测）

上游对**出口 IP** 做速率限制，与账号、客户端版本、出口是不是 CDN 都无关。被拦时返回
阿里云 WAF 的 **HTML 405**（`<!doctypehtml>` + `data-spm` + `<title>405</title>`），
请求根本到不了业务层；封禁期内该 IP 上**所有账号**一起不可用。

2026-11-22 用 10 个账号的隔离实例、2 路并发、每档 10 分钟实测：

| 手段 | 请求 | 成功 | 成功率 | 每次成功摊到的 405 |
|---|---|---|---|---|
| 直连，无 pacing | 461 | 147 | 32% | 18.5 |
| 直连 + pacing 150~400ms | 344 | 143 | 41% | 9.2 |
| 版本号 3.19.1 + pacing | 430 | 145 | 33% | 10.0 |
| CF edge relay + pacing | 342 | 155 | 45% | 9.2 |
| **住宅代理（生产 fallback，08:26–08:35）** | — | 68 | — | **2.0** |

结论：

1. **换版本号、换形态、换 Cloudflare edge relay 都没有用**——405 的量级纹丝不动。
   限速判定的是「这个 IP 上的请求速率」，不是「谁在打」。
2. **pacing 有用但不够**：把速率压下来能把成功率从 32% 抬到 41~45%，仍过不了半。
3. **只有独享住宅 IP 明显更好**（2.0 次/成功，好 4.6 倍），这也是生产 `fallback`
   一直在做的事。
4. **2 路持续 10 分钟就能把一个数据中心 IP 打封**，封禁 30 分钟以上（是速率窗口，
   停了会恢复，不是永久拉黑）。

运维含义：

- 面板概览页常驻一条提醒（`#concurrency-warn`），告知多路并发风险；
- 多子代理 / 批量任务**串行或分批**跑，不要同时开满；
- 判断某台机器的出口 IP 当前是否被限速，用裸探测即可（不需要账号）：

  ```bash
  curl -s -o /dev/null -w '%{http_code}\n' -X POST \
    https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages \
    -H 'Content-Type: application/json' \
    -d '{"model":"GLM-5.3-Flash","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}'
  ```

  `401` = 正常（进了业务层）；`405` = 该 IP 正被 WAF 拦。


## 网关出口（代理池要不要兼作出口）

结论：**默认别开**。

- 住宅代理的 RTT 比直连高一个数量级，流式响应会明显变卡；而且池子里的出口 IP 本身可能
  已经被别人用脏，`always` 反而更容易吃 3012。
- 真正有用的是 `fallback`：直连正常时零成本，只有直连被 3012/429 时才换住宅出口重打一次，
  给「VPS 的 IP 被盯上」留一条退路。
- 面板「设置 → 网关出口」三档：`direct`（默认）/ `fallback` / `always`；
  「兜底前先换出口 IP」打开时先调中继 `/rotate`。
- 出口地址取 `ZCODE_PROXY_URL_EGRESS`，没配就复用 `ZCODE_PROXY_URL_CLAIM`。
- **领取出口四档**（面板同一张卡片）：`direct` 仅直连 / `direct_fallback` 直连优先、失败回退代理 /
  `proxy` 仅代理 / `proxy_fallback` 代理优先、失败回退直连。回退只在**传输层失败**时触发，
  拿到响应就不再重试，避免把一次领取提交两次。

验证出口确实在变：

```bash
curl -s -x http://127.0.0.1:3129 https://api.ipify.org   # 住宅代理的 IP
curl -s http://127.0.0.1:3129/rotate                      # 换一个
curl -s -x http://127.0.0.1:3129 https://api.ipify.org   # 应该变了
```

## 账号备份

面板「账号管理 → 导出备份」把账号连同凭据打包成口令加密的 JSON（scrypt + AES-256-GCM），
「导入备份」按 `provider + 上游用户 ID` 判重，已存在的跳过、账号 ID 重新分配。
不直接拷 `data/accounts.json` 的原因：那份里的凭据绑本机 `master.key`，换机器解不开。
口令丢了文件就废了，没有后门。


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
