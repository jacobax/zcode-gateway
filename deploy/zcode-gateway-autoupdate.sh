#!/usr/bin/env bash
# zcode-gateway 自动更新（从本 fork 的 main 构建）
#
# 本仓库 main = 上游 Ciii-ATM/zcode-gateway + 本地补丁（见 git log）。
# 因此这里不再"下载官方 Release 二进制 + 打文本补丁"，而是：
#   拉本仓库 main → 用本机 Go 编译 → 冒烟 → 换二进制 → 健康检查 → 失败回滚。
# 吸收上游更新的流程见仓库根目录 OPERATIONS.md。
#
# 手动测试（强制重建当前版本）：FORCE_REINSTALL=1 /usr/local/bin/zcode-gateway-autoupdate.sh
set -uo pipefail

SRC=/opt/zcode-gateway-src          # 本 fork 的 clone
BRANCH=main
GO=/usr/local/go/bin/go
DIR=/opt/zcode-gateway
BIN=$DIR/zcode-gateway
VERSION_FILE=$DIR/VERSION           # 记录已部署的 commit sha
SERVICE=zcode-gateway
DATA=$DIR/data
BASE=http://127.0.0.1:3000

LOG=/var/log/zcode-gateway-autoupdate.log
LOCK=/var/lock/zcode-gateway-autoupdate.lock
BACKUP_ROOT=/opt/zcode-gateway-backups
KEEP_BACKUPS=7
HEALTH_TIMEOUT=120

if [ -f "$LOG" ] && [ "$(wc -l <"$LOG")" -gt 3000 ]; then
  tail -1500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
fi
exec >>"$LOG" 2>&1

say() { printf '[%s] %s\n' "$(date '+%F %T%z')" "$*"; }
die() { say "ERROR: $*"; exit 1; }

exec 9>"$LOCK"
if ! flock -n 9; then say "另一次更新正在运行，本次跳过"; exit 0; fi

[ -x "$BIN" ] || die "找不到 $BIN"
[ -x "$GO" ] || die "找不到 Go 工具链 $GO"
[ -d "$SRC/.git" ] || die "找不到源码目录 $SRC"
say "================ 开始检查更新（fork main 源码构建）================"

cd "$SRC" || die "进不去 $SRC"
git fetch --force origin "$BRANCH" >/dev/null 2>&1 || die "git fetch 失败"
TARGET=$(git rev-parse "origin/$BRANCH") || die "取不到 origin/$BRANCH"
TARGET_SHORT=$(git rev-parse --short "$TARGET")

CUR="(unknown)"
[ -f "$VERSION_FILE" ] && CUR=$(cat "$VERSION_FILE")
say "当前 $CUR，目标 $TARGET_SHORT ($BRANCH)"
if [ "$TARGET" = "$CUR" ] && [ "${FORCE_REINSTALL:-0}" != "1" ]; then
  say "已是最新版本，无需操作"; exit 0
fi

# ---- 备份数据 + 旧二进制 ----
BK="$BACKUP_ROOT/$(date +%Y%m%d-%H%M%S)"
if mkdir -p "$BK" && chmod 700 "$BK"; then
  cp -a "$DATA/accounts.json" "$DATA/master.key" "$BK/" 2>/dev/null || true
  if ! python3 - "$DATA/gateway.db" "$BK/gateway.db" <<'PY'
import sqlite3, sys
src = sqlite3.connect(sys.argv[1]); dst = sqlite3.connect(sys.argv[2])
with dst: src.backup(dst)
dst.close(); src.close()
PY
  then
    cp -a "$DATA"/gateway.db* "$BK/" 2>/dev/null || true
  fi
  cp -a "$BIN" "$BK/zcode-gateway.prev" 2>/dev/null || true
  say "已备份 data 与旧二进制 → $BK"
  ls -1dt "$BACKUP_ROOT"/*/ 2>/dev/null | tail -n +$((KEEP_BACKUPS+1)) | xargs -r rm -rf
else
  say "WARN: 备份目录创建失败，继续更新"
fi

# ---- 切代码 ----
git checkout -f "$TARGET" >/dev/null 2>&1 || die "git checkout $TARGET_SHORT 失败"
say "代码已切到 $TARGET_SHORT ($(git log -1 --format='%cs %s' | cut -c1-60))"

# ---- 构建 ----
say "开始构建..."
BUILD_T0=$SECONDS
NEWBIN=/tmp/zcode-gateway-build.$$
if ! GOPROXY=https://goproxy.cn,direct GOTOOLCHAIN=local "$GO" build -trimpath -ldflags="-s -w" -o "$NEWBIN" . ; then
  say "ERROR: 构建失败，保持现状（旧二进制未动）"
  exit 1
fi
say "构建成功，用时 $((SECONDS-BUILD_T0))s（$(stat -c%s "$NEWBIN") 字节）"

# ---- 冒烟测试：能在临时端口起监听才动线上服务 ----
SMOKE_DIR=$(mktemp -d /tmp/zcode-gateway-smoke.XXXXXX)
SMOKE=$(timeout 5 env ZCODE_GATEWAY_ADDR=127.0.0.1:3999 ZCODE_GATEWAY_DATA="$SMOKE_DIR" "$NEWBIN" 2>&1 | head -2)
rm -rf "$SMOKE_DIR"
if ! printf '%s' "$SMOKE" | grep -q "listening on"; then
  say "ERROR: 冒烟测试失败：$(printf '%s' "$SMOKE" | head -1)"
  rm -f "$NEWBIN"
  exit 1
fi
say "冒烟测试通过"

# ---- 换二进制 + 重启 ----
PREV="$DIR/zcode-gateway.prev"
cp -a "$BIN" "$PREV" 2>/dev/null || true
say "停止 $SERVICE ..."
systemctl stop "$SERVICE"
if ! install -m 755 "$NEWBIN" "$BIN"; then
  say "ERROR: 安装失败，回滚"
  install -m 755 "$PREV" "$BIN"; systemctl start "$SERVICE"; rm -f "$NEWBIN"; exit 1
fi
rm -f "$NEWBIN"
systemctl start "$SERVICE"

# ---- 健康检查 ----
say "等待服务就绪（最多 ${HEALTH_TIMEOUT}s）..."
OK=0
for _ in $(seq 1 $((HEALTH_TIMEOUT/5))); do
  sleep 5
  BODY=$(curl -s -m 5 "$BASE/health" 2>/dev/null) || BODY=""
  if printf '%s' "$BODY" | grep -q '"ok":true' \
     && N=$(printf '%s' "$BODY" | python3 -c 'import sys,json;print(json.load(sys.stdin).get("accounts",0))' 2>/dev/null) \
     && [ "${N:-0}" -gt 0 ]; then
    OK=1; break
  fi
done

if [ "$OK" = "1" ]; then
  printf '%s\n' "$TARGET" > "$VERSION_FILE"
  say "✅ 更新成功：$CUR → $TARGET_SHORT（服务 active，账号 $N 个）"
  ls -1dt "$DIR"/zcode-gateway.prev-* 2>/dev/null | tail -n +3 | xargs -r rm -f
  cp -a "$PREV" "$DIR/zcode-gateway.prev-$TARGET_SHORT" 2>/dev/null || true
  exit 0
fi

say "❌ 健康检查未通过（/health=$BODY），回滚"
journalctl -u "$SERVICE" --no-pager -n 20 2>&1 | sed 's/^/    /'
systemctl stop "$SERVICE"
install -m 755 "$PREV" "$BIN" 2>/dev/null || say "❗ 回滚拷贝失败，备份在 $BK"
systemctl start "$SERVICE"
sleep 10
BODY2=$(curl -s -m 5 "$BASE/health" 2>/dev/null) || BODY2=""
if printf '%s' "$BODY2" | grep -q '"ok":true'; then
  say "已回滚，服务已恢复：$BODY2"
else
  say "❗❗ 回滚后仍不健康：$BODY2  备份在 $BK"
fi
exit 1
