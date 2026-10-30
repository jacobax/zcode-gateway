# deploy/

systemd 单元与自动更新脚本，配套 `OPERATIONS.md` 使用。以下路径是脚本的硬编码约定，
改路径要同时改单元文件和脚本。

| 文件 | 安装到 | 作用 |
|---|---|---|
| `zcode-gateway.service` | `/etc/systemd/system/` | 网关本体（`127.0.0.1:3000`，数据在 `/opt/zcode-gateway/data`） |
| `zcode-gateway-autoupdate.service` / `.timer` | 同上 | 每天 05:50（北京时间）从本仓库 `main` 拉代码、编译、换二进制 |
| `zcode-gateway-autoupdate.sh` | `/usr/local/bin/` | 上面的 service 实际执行的脚本 |
| `zcode-claimer.service` / `.timer` | `/etc/systemd/system/` | 套餐自动领取 sidecar，每小时 :17 一轮 |

## 安装

```bash
# 1) 源码与二进制
git clone https://github.com/jacobax/zcode-gateway /opt/zcode-gateway-src
mkdir -p /opt/zcode-gateway/data
cd /opt/zcode-gateway-src && /usr/local/go/bin/go build -trimpath -ldflags="-s -w" \
  -o /opt/zcode-gateway/zcode-gateway .

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

自动更新脚本假定本机有 Go（`/usr/local/go/bin/go`）和 `python3`（用于 SQLite 在线备份）。

## 环境变量（网关）

单元里已经写死的两个是部署必需的，其余见上游 README：

| 变量 | 值 | 说明 |
|---|---|---|
| `ZCODE_GATEWAY_ADDR` | `127.0.0.1:3000` | 只监听回环，公网通过 cloudflared / 反向代理进来 |
| `ZCODE_GATEWAY_DATA` | `/opt/zcode-gateway/data` | 账号、master.key、SQLite |
| `ZCODE_ADMIN_PASSWORD` | 自定 | 面板登录密码；不设则首次启动随机生成并打到日志 |
