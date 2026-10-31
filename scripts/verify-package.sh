#!/usr/bin/env bash
# 发布包体检：确认包里没有运行时数据/密钥，且部署所需文件齐全。
# 用法：scripts/verify-package.sh <解包后的目录>
set -euo pipefail

DIR=${1:?用法: verify-package.sh <解包后的目录>}
[ -d "$DIR" ] || { echo "ERROR: $DIR 不是目录"; exit 1; }
cd "$DIR"

fail=0

# ---- 1. 不许有私人数据 / 密钥 ----
bad=$(find . \( -name 'data' -o -name '*.db' -o -name '*.db-wal' -o -name '*.db-shm' \
        -o -name '*.db-journal' -o -name '*.key' -o -name 'accounts.json' \
        -o -name '.env' -o -name '*.log' -o -name '.git' \) -print)
if [ -n "$bad" ]; then
  echo "ERROR: 发布包含运行时数据/密钥，禁止发布："
  printf '%s\n' "$bad" | sed 's/^/    /'
  fail=1
fi

# ---- 2. 不许出现凭据形态的字符串 ----
patterns=(
  'ghp_[A-Za-z0-9]{30,}'
  'github_pat_[A-Za-z0-9_]{20,}'
  'sk-zc-[A-Za-z0-9]{20,}'
  '-----BEGIN [A-Z ]*PRIVATE KEY-----'
  '[0-9]{11}@phone\.local'
)
for p in "${patterns[@]}"; do
  hits=$(grep -rInE "$p" . 2>/dev/null | head -5 || true)
  if [ -n "$hits" ]; then
    echo "ERROR: 发布包含疑似凭据（/$p/）："
    printf '%s\n' "$hits" | sed 's/^/    /'
    fail=1
  fi
done

# ---- 3. 部署必需文件齐全 ----
required=(
  README.md
  OPERATIONS.md
  contrib/relay/relay.py
  contrib/claimer/claim_round.py
  contrib/migrate/migrate_zcode2api.py
  deploy/README.md
  deploy/zcode-gateway.service
  deploy/zcode-claimer.service
  deploy/zcode-relay.service
  deploy/zcode-gateway-autoupdate.sh
)
for f in "${required[@]}"; do
  [ -f "$f" ] || { echo "ERROR: 发布包缺少 $f"; fail=1; }
done
[ -f zcode-gateway ] || [ -f zcode-gateway.exe ] || { echo "ERROR: 发布包缺少主程序"; fail=1; }

if [ "$fail" != 0 ]; then exit 1; fi
echo "OK: 发布包体检通过（$(find . -type f | wc -l) 个文件，无数据/密钥，部署文件齐全）"
