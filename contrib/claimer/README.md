# contrib/claimer —— ZCode 套餐自动领取 sidecar

上游网关的「活动 → 领取」是纯手动的：面板里点一下，浏览器跑阿里云验证码，拿到
`X-Aliyun-Captcha-Verify-Param` 再提交。这个脚本把那一步自动化，**不改网关本体**，
只调用它的管理接口。

## 它做什么

每轮（默认每小时）：

1. 面板登录，列出全部账号（含被禁用的——禁用只影响转发，不影响领取）。
2. 逐个问 `/api/accounts/{id}/activities`，取可领套餐列表与验证码配置。
3. 对「列表里出现且未标记已领」的项：调本机 Chromium 跑官方 SDK 解出验证码 →
   `POST /api/accounts/{id}/claim`。
4. 按额度自动启停（见下节）。
5. 同一 (账号, 套餐) 命中 `1001–1005` 后 6 小时内不再重试，避免反复解验证码。

## 额度启停策略

网关的选号只看 `enabled` 和冷却，**不看额度**；而上游对「没有套餐」的账号返回的是
HTTP 200 + `{"code":1005,"exceed quota limit"}`，网关不识别这个业务码，会原样透传给
客户端。所以没额度的账号必须从转发池里拿掉。

脚本按下面的规则维护这件事（`ZC_AUTO_MANAGE=1` 时生效）：

| 条件 | 动作 |
|---|---|
| 禁用中 + `remaining > 0` + 是脚本禁用的（或 lastError 带迁移标记） | 启用 |
| 启用中 + 额度数据新鲜（`source=api`，15 分钟内）+ `granted==0` 连续 2 轮 | 禁用 |
| 启用中 + `granted>0` 但 `remaining==0`（用光了） | **不动**，交给网关的 `ExhaustedUntil` |
| 人工禁用的账号（不在脚本的管辖名单里） | **不动** |

管辖名单存在 `/var/lib/zcode-claimer/quota_state.json` 的 `disabled_by_us` 里：
脚本自己禁用的会自动登记，人工禁用的不会。这样"我不想要这个号"和"这个号暂时没额度"
两种意图不会互相覆盖。
## 依赖

脚本本体只用 Python 标准库，但**验证码求解需要外部求解器**：

- `node`（≥ 18）
- 系统 Chromium（`/usr/bin/chromium`）
- `puppeteer-core`
- 一个能输出 `VERIFY_PARAM=<base64>` 的 `solver_pw.js` + 它加载的 `page.html`

求解器不在本仓库（第三方代码，未随仓库授权）。放置方式：

```
/opt/zcode-claimer/solver/
├── solver_pw.js      # 入参：node solver_pw.js <sceneId> <region> <prefix>
├── page.html         # solver_pw.js 用 file:// 加载的宿主页
└── node_modules/     # puppeteer-core
```

`solver_pw.js` 的契约：命令行三个位置参数 `scene region prefix`，成功时 stdout 打印
`VERIFY_PARAM=<verifyParam>`，失败非零退出。可参考社区实现
（dengyie/zcode2api 的 `captcha_node/solver_pw.js`）。

## 为什么必须用真浏览器

无浏览器的 happy-dom 路线（纯 JS 模拟 DOM 跑阿里云 SDK）在 2026-09 之后的上游
pe 虚拟机上会 stall（实测 `[pe-stall] pe.075.…`，退出码 2）。真浏览器路线稳定在
6–8 秒一次，峰值内存约 590MB——所以 `deploy/zcode-claimer.service` 里有
`MemoryMax=700M` 兜底。

## 配置

写在 `/etc/zcode-claimer.env`：

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZC_PASSWORD` | 必填 | 面板密码 |
| `ZC_BASE` | `http://127.0.0.1:3000` | 网关地址 |
| `ZC_SOLVER_DIR` | `/opt/zcode-claimer/solver` | 求解器目录 |
| `ZC_MIN_AVAIL_MB` | `450` | 可用内存低于此值跳过本轮 |
| `ZC_MAX_SOLVES` | `8` | 单轮最多解几次验证码 |
| `ZC_REQUIRE_CLAIMABLE` | `0` | `1` = 只领 preview 标 `claimable=true` 的项 |
| `ZC_RETRY_SKIP_HOURS` | `6` | 失败缓存有效期 |
| `ZC_AUTO_MANAGE` | `1` | 按额度自动启停（旧名 `ZC_AUTO_ENABLE` 仍可用） |
| `ZC_EMPTY_ROUNDS` | `2` | 连续几轮查不到额度才禁用 |

## 已知的上游坑

- **`ok=true` 不等于额度到账**。claim 接口返回成功只代表请求被接受，额度要等
  `starts_at` 生效后才出现在 `billing/balance`。
- **`claimable` 不可信**。已领取的套餐在 preview 里可能仍标 `claimable=true`，
  重复领取返回 `1003 You have already claimed this plan`。
- **领取必须带 `X-Aliyun-Captcha-Verify-Region`**，漏掉会返回 `3007`。
- 直连上游 `billing/claim` 会因缺 `X-ZCode-*` 头返回 `3001 parameter error`，
  所以一律走网关的管理接口，别自己拼请求。
