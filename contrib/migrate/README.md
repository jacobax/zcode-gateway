# contrib/migrate —— 从 dengyie/zcode2api 迁移账号

一次性脚本，把旧网关（Python，`data/accounts.db`）的账号搬进本网关（Go，
`data/accounts.json` + `data/gateway.db`）。

## 字段映射

| 旧库 `accounts.db` | 新网关 |
|---|---|
| `accounts.id` | `accounts.json` 的 `Account.id` |
| `data.jwt_token` | `secrets.zcodeJwt`（AES-256-GCM，密钥 `data/master.key`） |
| jwt payload 的 `sub` | `Account.user.id`，`label` 取 `zai-<sub前8>` |
| `data.fingerprint.*` | SQLite `account_profiles`（设备指纹原样保留，避免触发上游 3012） |
| `data.quota` | `Account.quota`（首屏展示用，随后被后台刷新覆盖） |
| `data.use_count` | `Account.usage.requests` |
| `meta.account_concurrency` | `account_profiles.concurrency` |

加密格式：`enc:v1:<b64url(iv)>.<b64url(tag)>.<b64url(ciphertext)>`，
AES-256-GCM，密钥是 `data/master.key` 的 32 字节原文。脚本用 `cryptography` 复刻，
所以不必走 OAuth 重新登录。

## 用法

```bash
# 必须先让新网关启动一次，生成 data/master.key 与 gateway.db 的建表
systemctl stop zcode-gateway
cp /opt/zcode2api/data/accounts.db /tmp/accounts-snapshot.db   # 先快照
python3 migrate_zcode2api.py
systemctl start zcode-gateway
```

跑完记得核对：`/health` 的账号数、面板里的账号数与额度。

## prune_pool.py

迁移后的一次性清理，两件事：

1. 停用「上游无有效套餐」的账号（当时按 `billing/current plans=[]` 判断），
   避免它们参与转发后返回 `{"code":1005,"exceed quota limit"}`（HTTP 200，
   网关不识别该业务码，会原样透传给客户端）。
2. 同一 `user.id` 的重复行只保留一条。

注意：`billing/current` 现在对所有账号都返回空，这个判据已经不可靠，脚本留作历史参考。
