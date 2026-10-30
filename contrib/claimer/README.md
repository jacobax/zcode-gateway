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
4. 禁用账号一旦 `quota.remaining > 0`（套餐生效、额度到账）就自动启用回转发池。
5. 同一 (账号, 套餐) 命中 `1001–1005` 后 6 小时内不再重试，避免反复解验证码。

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
| `ZC_AUTO_ENABLE` | `1` | 有额度就自动启用禁用账号 |

## 已知的上游坑

- **`ok=true` 不等于额度到账**。claim 接口返回成功只代表请求被接受，额度要等
  `starts_at` 生效后才出现在 `billing/balance`。
- **`claimable` 不可信**。已领取的套餐在 preview 里可能仍标 `claimable=true`，
  重复领取返回 `1003 You have already claimed this plan`。
- **领取必须带 `X-Aliyun-Captcha-Verify-Region`**，漏掉会返回 `3007`。
- 直连上游 `billing/claim` 会因缺 `X-ZCode-*` 头返回 `3001 parameter error`，
  所以一律走网关的管理接口，别自己拼请求。
