# ZCode Gateway（个人 fork）

把 ZCode 订阅额度变成标准的 Anthropic / OpenAI 兼容 API，带多账号调度池和 Web 管理台。
基于 [Ciii-ATM/zcode-gateway](https://github.com/Ciii-ATM/zcode-gateway)，本仓库在这个基础上
补齐了错误分级、OpenAI 兼容、加权调度、领取自动化和面板运维能力。

## 和上游的区别

| 能力 | 上游 | 本 fork |
|---|---|---|
| 协议入口 | `/v1/messages` | `/v1/messages` + `/v1/chat/completions`（OpenAI 兼容） |
| 上游失败处置 | 一个 cooldown 数字 | 拆成「重试 / 等待 / 冷却 / 停用」，429 不冷却账号 |
| 选号 | 健康账号里纯随机 | 余额占比 + 闲置时长加权，Top-N 内加权抽签 |
| 并发 | 只有单账号 | 全局默认并发 + 账号覆盖（`EffectiveConcurrency`） |
| 面板密码 | 只能靠环境变量 | 面板里可改，bcrypt 落库，改完踢掉其它会话 |
| 面板安全 | 无 | 登录爆破防护（5 次 / 5 分钟）、token 不回显明文 |
| 领取 | 外部脚本自行实现 | `contrib/claimer` + 1005 的 `next_at` 退避 |
| 领取出口 | 直连 | `contrib/relay` 住宅代理中继，按次换 IP |
| 网关出口 | 只能直连 | 可选：直连 / 被风控时兜底 / 全量走代理池（面板里选） |
| 账号备份 | 无 | 口令加密的导入导出，换机可用 |
| 版本 | 无 | 面板显示网关版本 + 检查 GitHub 最新 release |
| 发布 | 手工编译 | tag 触发六平台构建，发布包含 `contrib/`、`deploy/` |
| 运维 | 无 | `OPERATIONS.md`：已知坑、回滚、发版节奏 |

## 端点

| 端点 | 协议 | 典型客户端 |
|---|---|---|
| `POST /v1/messages` | Anthropic Messages | Claude Code、ZCode 桌面端 |
| `POST /v1/chat/completions` | OpenAI Chat Completions | OpenAI SDK、Cursor、各类兼容工具 |
| `POST /v1/responses` | OpenAI Responses | Codex CLI 及只认 Responses 形状的客户端 |
| `GET /v1/models` | 模型列表 | — |
| `GET /health` | 存活探测（含账号数、在途请求、构建版本、活动客户端版本） | systemd / 监控 |

鉴权：面板里设了「代理 Token」后，三个入口都要带 `Authorization: Bearer <token>`
或 `x-api-key: <token>`；留空表示本机免鉴权（只建议在回环地址上这么做）。

```bash
# Anthropic 风格
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_AUTH_TOKEN=<token>

# OpenAI 风格
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer <token>" -H "content-type: application/json" \
  -d '{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}'

# Responses 风格（Codex CLI 走这条）
curl -s http://127.0.0.1:8787/v1/responses \
  -H "Authorization: Bearer <token>" -H "content-type: application/json" \
  -d '{"model":"GLM-5.3","input":"hi","max_output_tokens":64}'
```

```bash
# Anthropic 风格
export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
export ANTHROPIC_AUTH_TOKEN=<token>

# OpenAI 风格
curl -s http://127.0.0.1:8787/v1/chat/completions \
  -H "Authorization: Bearer <token>" -H "content-type: application/json" \
  -d '{"model":"GLM-5.3","messages":[{"role":"user","content":"hi"}]}'
```

面板「设置 → 客户端接入指引」会按当前 Base URL / Token 生成可直接复制的示例。

## 快速开始

**用发布包（推荐）**

```bash
TAG=$(curl -fsSL https://api.github.com/repos/jacobax/zcode-gateway/releases/latest | grep -m1 '"tag_name"' | cut -d'"' -f4)
curl -fsSLO "https://github.com/jacobax/zcode-gateway/releases/download/$TAG/zcode-gateway_${TAG#v}_linux_amd64.tar.gz"
tar xzf "zcode-gateway_${TAG#v}_linux_amd64.tar.gz" -C /opt/zcode-gateway
printf '%s\n' "$TAG" > /opt/zcode-gateway/VERSION
```

包里有二进制、`README.md`、`OPERATIONS.md`、`contrib/`（中继 + 领取）和 `deploy/`
（systemd 单元 + 自动更新脚本）。装法见 `deploy/README.md`。

**从源码**

```bash
git clone https://github.com/jacobax/zcode-gateway && cd zcode-gateway
go build -trimpath -ldflags="-s -w" -o zcode-gateway .
./zcode-gateway          # 默认 127.0.0.1:8787，数据在 ./data
```

首次启动账号表为空时，会尝试从本机 ZCode 桌面端（`~/.zcode/v2/credentials.json`）
导入一次，日志里会写 `已从桌面端导入 N 个账号`；没有凭据则打 `未发现桌面端凭据 …，跳过自动导入`。

## 面板

- **概览**：账号/可用数、今日 token 分布、多账号策略说明、最近请求。
- **账号管理**：添加（浏览器授权 / 导入本机）、启停、排序、并发（可「跟随」全局）、
  新指纹、活动查询与领取、刷新额度、清除冷却；顶部有「怎么添加账号」说明。
- **请求日志**：成功/失败/**账号事件** 三类筛选，支持按账号过滤和详情抽屉。
- **设置**：代理 Token（掩码回显）、面板密码、客户端接入指引、版本与更新、
  指纹与额度刷新、全局默认并发、领取代理池。

## 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `ZCODE_GATEWAY_ADDR` | `127.0.0.1:8787` | 监听地址，生产建议只绑回环 |
| `ZCODE_GATEWAY_DATA` | `data` | 数据目录（`accounts.json`、`master.key`、`gateway.db`） |
| `ZCODE_ADMIN_PASSWORD` | 随机生成 | 面板密码**初始值**；面板里改过后以面板为准 |
| `ZCODE_APP_VERSION` | `3.16.3` | 模拟的 ZCode 客户端版本（UA 与 `X-ZCode-App-Version` 等身份头都取它）。**升级前必须做单账号 A/B 验证**，见 `OPERATIONS.md` 第 10 条坑 |
| `ZCODE_UPSTREAM` | `https://zcode.z.ai` | 上游地址 |
| `ZCODE_PROXY_URL_CLAIM` | 空 | 只给领取/活动用的出口（通常指向本地中继） |
| `ZCODE_PROXY_URL_EGRESS` | 同 `CLAIM` | 模型流量可选出口；面板「网关出口」决定用不用 |
| `ZCODE_POOL_TOPN` | `5` | 加权选号的短名单大小 |
| `ZCODE_RETRY_WAIT_SEC` | `20` | 429 原地重试的总预算，0 表示不等待 |
| `ZCODE_REQUEST_TIMEOUT_SEC` | `600` | 单个上游请求超时 |
| `ZCODE_STICKY_TTL_SEC` / `ZCODE_STICKY_WAIT_MS` | `86400` / `3000` | 会话粘性 TTL 与等待预算 |
| `ZCODE_MAX_HOLD_SEC` | `600` | 单请求占用账号槽位的硬上限（看门狗） |
| `ZCODE_GITHUB_TOKEN` | 空 | 面板「检查更新」查 GitHub 时用，避免限流 |

完整说明见 `OPERATIONS.md`。

## 数据与安全

- 账号凭据用 `data/master.key` 做 AES-256-GCM 加密后存 `accounts.json`；日志与指纹在 `gateway.db`。
- 面板密码 bcrypt 落库（`adminPasswordHash` + `adminPasswordManual`），只有面板里设过的才压过环境变量。
- 代理 Token 不在 `GET /api/settings` 里回显明文，需要时走 `GET /api/settings/token`。
- 发布包由 `scripts/verify-package.sh` 把关，混进 `data/`、`*.db`、`*.key` 会直接构建失败；
  仓库侧由 `scripts/scan-secrets.sh` 扫凭据与私人词条（`.private-terms`，gitignore）。

## 版本与自动更新

- `GET /health` 返回 `build`（网关版本）与 `version`（模拟的客户端版本），别混。
- 面板「设置 → 版本与更新」会查 GitHub 最新 release；关掉「自动更新」后，
  宿主机上的 `zcode-gateway-autoupdate.sh` 会直接跳过（`FORCE_REINSTALL=1` 可强制）。
- 发布流程：合 main → 攒够一批 → 打一个 `v*` tag → Actions 出六平台包 → 跑 autoupdate 上线。
  细节见 `OPERATIONS.md` 的「发版节奏」。

## 目录

```
main.go / handlers.go / auth.go / classify.go / openai.go / scheduler.go / store.go
  ├─ classify.go   上游失败分级（重试/等待/冷却/停用）
  ├─ openai.go     OpenAI ⇄ Anthropic 双向翻译
  ├─ scheduler.go  账号池：并发槽位、会话粘性、加权选号
  └─ update.go     检查 GitHub 最新 release
web/               面板前端（embed 进二进制，改完记得 bump ?v=N）
contrib/relay/     领取用的住宅代理中继
contrib/claimer/   套餐自动领取 sidecar
contrib/migrate/   从旧 zcode2api 迁移账号
deploy/            systemd 单元 + 自动更新脚本
scripts/           发布包体检 + 仓库凭据扫描
OPERATIONS.md      运维手册（已知坑、回滚、发版节奏）
```

## 说明

仅供个人学习与自用。使用前请确认符合上游服务条款；账号凭据由使用者自行保管，
本仓库不收集任何遥测。上游接口随时可能变化，遇到 `3012` 之类的问题先看 `OPERATIONS.md`。
