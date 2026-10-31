# deploy/

systemd 单元与自动更新脚本，配套 `OPERATIONS.md` 使用。以下路径是脚本的硬编码约定，
改路径要同时改单元文件和脚本。

## 从 Release 包部署（推荐）

每个 release 的 tar.gz / zip 里已经有**完整可部署内容**：

```
zcode-gateway              二进制（含所有本地补丁）
README.md / OPERATIONS.md  运维文档
contrib/relay/             领取流量用的代理中继
contrib/claimer/           套餐自动领取脚本
deploy/                    systemd 单元 + 自动更新脚本
```

```bash
tar xzf zcode-gateway_<版本>_linux_amd64.tar.gz -C /opt/zcode-gateway
# 网关没有配置文件，全部走环境变量（见下面的单元文件）；
# 然后按表格装 systemd 单元
```

注意：`contrib/claimer` 还缺验证码求解器（第三方代码，不随仓库分发），
按 `contrib/claimer/README.md` 自行放置。

| 文件 | 安装到 | 作用 |
|---|---|---|
| `zcode-gateway.service` | `/etc/systemd/system/` | 网关本体（`127.0.0.1:3000`，数据在 `/opt/zcode-gateway/data`） |
| `zcode-gateway-autoupdate.service` / `.timer` | 同上 | 每天 05:50（北京时间）查最新 release，下载 + SHA256 校验 + 换二进制 |
| `zcode-gateway-autoupdate.sh` | `/usr/local/bin/` | 上面的 service 实际执行的脚本 |
| `zcode-claimer.service` / `.timer` | `/etc/systemd/system/` | 套餐自动领取 sidecar，每小时 :17 一轮 |
| `zcode-relay.service` | 同上 | 领取流量用的住宅代理中继（见 `contrib/relay/`），监听 `127.0.0.1:3129` |

## 安装

```bash
# 1) 下载发布包（含二进制、文档、contrib/、deploy/）
mkdir -p /opt/zcode-gateway/data /opt/zcode-gateway/dist
TAG=$(curl -fsSL https://api.github.com/repos/jacobax/zcode-gateway/releases/latest | grep -m1 '"tag_name"' | cut -d'"' -f4)
cd /tmp && curl -fsSLO "https://github.com/jacobax/zcode-gateway/releases/download/$TAG/zcode-gateway_${TAG#v}_linux_amd64.tar.gz"
curl -fsSLO "https://github.com/jacobax/zcode-gateway/releases/download/$TAG/SHA256SUMS.txt"
sha256sum -c --ignore-missing SHA256SUMS.txt
tar xzf "zcode-gateway_${TAG#v}_linux_amd64.tar.gz" -C /opt/zcode-gateway
printf '%s\n' "$TAG" > /opt/zcode-gateway/VERSION
# 2) 网关
cp deploy/zcode-gateway.service /etc/systemd/system/
# 面板密码写进单元里的 Environment=ZCODE_ADMIN_PASSWORD=...
systemctl daemon-reload && systemctl enable --now zcode-gateway

# 3) 自动更新
install -m 755 deploy/zcode-gateway-autoupdate.sh /usr/local/bin/
cp deploy/zcode-gateway-autoupdate.service deploy/zcode-gateway-autoupdate.timer /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now zcode-gateway-autoupdate.timer

# 4) 领取 sidecar（可选，见 contrib/claimer/README.md）
mkdir -p /opt/zcode-claimer/solver /var/lib/zcode-claimer
cp contrib/claimer/claim_round.py /opt/zcode-claimer/
printf 'ZC_PASSWORD=%s\nZC_MIN_AVAIL_MB=450\n' '<面板密码>' > /etc/zcode-claimer.env
chmod 600 /etc/zcode-claimer.env
cp deploy/zcode-claimer.service deploy/zcode-claimer.timer /etc/systemd/system/
systemctl daemon-reload && systemctl enable --now zcode-claimer.timer
```

自动更新脚本只依赖 `curl` + `python3`（SQLite 在线备份、解析 release JSON），**不再需要本机 Go**。
它只替换二进制和文档；`contrib/`、`deploy/` 会解到 `/opt/zcode-gateway/dist/<tag>/` 备查，
不会自动覆盖 `/opt/zcode-relay`、`/opt/zcode-claimer`（那两个独立部署，线上可能有本地改动）。

## 发布前的两道闸

| 脚本 | 在哪跑 | 干什么 |
|---|---|---|
| `scripts/scan-secrets.sh` | CI（每次 push / PR） | 扫已跟踪文件里的令牌、私钥、手机号等；个人词条写进 `.private-terms`（gitignore）后一并扫 |
| `scripts/verify-package.sh` | release（打包后） | 拒绝含 `data/`、`*.db`、`*.key`、`accounts.json`、`.env` 的包，并检查部署文件齐全 |

自动更新脚本装包前还会再跑一遍同样的路径检查，包里混进 `data/` 会直接拒绝安装。

## 环境变量（网关）

单元里已经写死的两个是部署必需的，其余见上游 README：

| 变量 | 值 | 说明 |
|---|---|---|
| `ZCODE_GATEWAY_ADDR` | `127.0.0.1:3000` | 只监听回环，公网通过 cloudflared / 反向代理进来 |
| `ZCODE_GATEWAY_DATA` | `/opt/zcode-gateway/data` | 账号、master.key、SQLite |
| `ZCODE_ADMIN_PASSWORD` | 自定 | 面板登录密码；不设则首次启动随机生成并打到日志 |
