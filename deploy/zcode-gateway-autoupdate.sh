#!/usr/bin/env bash
# zcode-gateway 自动更新（下载 GitHub Release 包，不再本地编译）
#
# 流程：查 latest release → 与已部署 VERSION 比对 → 下载 tar.gz + 校验 SHA256
#       → 解包检查（拒绝含 data/ *.db *.key 的包）→ 冒烟 → 备份 → 换二进制
#       → 健康检查 → 失败回滚。
#
# 只替换二进制和文档；contrib/ 与 deploy/ 会解到 $DIST/<tag>/ 备查，
# 不会自动覆盖 /opt/zcode-relay 与 /opt/zcode-claimer（那两个是独立部署，
# 线上可能已有本地改动，覆盖会出事故）。
#
# 手动强制重装当前版本：FORCE_REINSTALL=1 /usr/local/bin/zcode-gateway-autoupdate.sh
# 私有仓库或遇到 API 限流：在 /etc/zcode-gateway-autoupdate.env 里写 GH_TOKEN=...
set -uo pipefail

REPO=jacobax/zcode-gateway
DIR=/opt/zcode-gateway
BIN=$DIR/zcode-gateway
VERSION_FILE=$DIR/VERSION           # 记录已部署的 release tag，如 v1.2.3
DIST=$DIR/dist                      # 每个版本的完整发布包留存处
SERVICE=zcode-gateway
DATA=$DIR/data
BASE=http://127.0.0.1:3000

LOG=/var/log/zcode-gateway-autoupdate.log
LOCK=/var/lock/zcode-gateway-autoupdate.lock
BACKUP_ROOT=/opt/zcode-gateway-backups
KEEP_BACKUPS=7
KEEP_DIST=5
HEALTH_TIMEOUT=120

# 发布包里绝不允许出现的东西（防止把私人数据打进包）
FORBIDDEN='^\./(data/|.*\.db$|.*\.key$|accounts\.json$|\.env$|.*\.log$)'

[ -r /etc/zcode-gateway-autoupdate.env ] && . /etc/zcode-gateway-autoupdate.env

if [ -f "$LOG" ] && [ "$(wc -l <"$LOG")" -gt 3000 ]; then
  tail -1500 "$LOG" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
fi
exec >>"$LOG" 2>&1

say() { printf '[%s] %s\n' "$(date '+%F %T%z')" "$*"; }
die() { say "ERROR: $*"; exit 1; }

exec 9>"$LOCK"
if ! flock -n 9; then say "另一次更新正在运行，本次跳过"; exit 0; fi

[ -x "$BIN" ] || die "找不到 $BIN"
case "$(uname -m)" in
  x86_64)          ARCH=amd64 ;;
  aarch64|arm64)   ARCH=arm64 ;;
  *)               die "不支持的架构 $(uname -m)" ;;
esac
PLATFORM="linux_${ARCH}"

say "================ 开始检查更新（release 包）================"

# ---- 1. 查最新 release ----
AUTH=()
[ -n "${GH_TOKEN:-}" ] && AUTH=(-H "Authorization: Bearer $GH_TOKEN")
TAG=$(curl -fsS -m 30 "${AUTH[@]}" -H 'Accept: application/vnd.github+json' \
        "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null \
      | python3 -c 'import sys,json;print(json.load(sys.stdin).get("tag_name",""))' 2>/dev/null)
[ -n "$TAG" ] || die "查询最新 release 失败（网络或 API 限流），保持现状"
VER=${TAG#v}
PKG="zcode-gateway_${VER}_${PLATFORM}.tar.gz"

CUR="(unknown)"
[ -f "$VERSION_FILE" ] && CUR=$(cat "$VERSION_FILE")
say "当前 $CUR，最新 release $TAG ($PLATFORM)"
if [ "$TAG" = "$CUR" ] && [ "${FORCE_REINSTALL:-0}" != "1" ]; then
  say "已是最新版本，无需操作"; exit 0
fi

# ---- 2. 下载 + 校验 ----
TMP=$(mktemp -d /tmp/zcode-gateway-rel.XXXXXX) || die "建临时目录失败"
trap 'rm -rf "$TMP"' EXIT
BASEURL="https://github.com/$REPO/releases/download/$TAG"
say "下载 $PKG ..."
curl -fsSL -m 300 -o "$TMP/$PKG" "$BASEURL/$PKG" || die "下载 $PKG 失败"
curl -fsSL -m 60  -o "$TMP/SHA256SUMS.txt" "$BASEURL/SHA256SUMS.txt" || die "下载 SHA256SUMS.txt 失败"

EXPECT=$(awk -v f="$PKG" '$2==f {print $1}' "$TMP/SHA256SUMS.txt")
[ -n "$EXPECT" ] || die "SHA256SUMS.txt 里没有 $PKG"
ACTUAL=$(sha256sum "$TMP/$PKG" | awk '{print $1}')
[ "$EXPECT" = "$ACTUAL" ] || die "校验失败：期望 $EXPECT 实际 $ACTUAL"
say "SHA256 校验通过（$ACTUAL）"

# ---- 3. 解包 + 内容审查 ----
mkdir -p "$TMP/x" && tar -C "$TMP/x" -xzf "$TMP/$PKG" || die "解包失败"
BAD=$(tar -tzf "$TMP/$PKG" | grep -E "$FORBIDDEN" || true)
if [ -n "$BAD" ]; then
  say "ERROR: 发布包含私人数据，拒绝安装："
  printf '%s\n' "$BAD" | sed 's/^/    /'
  exit 1
fi
[ -x "$TMP/x/zcode-gateway" ] || die "包里没有 zcode-gateway 可执行文件"
say "包内容检查通过（$(tar -tzf "$TMP/$PKG" | wc -l) 项）"

# ---- 4. 冒烟测试 ----
SMOKE_DIR=$(mktemp -d /tmp/zcode-gateway-smoke.XXXXXX)
SMOKE=$(timeout 5 env ZCODE_GATEWAY_ADDR=127.0.0.1:3999 ZCODE_GATEWAY_DATA="$SMOKE_DIR" \
          "$TMP/x/zcode-gateway" 2>&1 | head -2)
rm -rf "$SMOKE_DIR"
if ! printf '%s' "$SMOKE" | grep -q "listening on"; then
  die "冒烟测试失败：$(printf '%s' "$SMOKE" | head -1)"
fi
say "冒烟测试通过"

# ---- 5. 备份 ----
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

# ---- 6. 换二进制 + 重启 ----
PREV="$DIR/zcode-gateway.prev"
cp -a "$BIN" "$PREV" 2>/dev/null || true
say "停止 $SERVICE ..."
systemctl stop "$SERVICE"
if ! install -m 755 "$TMP/x/zcode-gateway" "$BIN"; then
  say "ERROR: 安装失败，回滚"
  install -m 755 "$PREV" "$BIN"; systemctl start "$SERVICE"; exit 1
fi
systemctl start "$SERVICE"

# ---- 7. 健康检查 ----
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
  printf '%s\n' "$TAG" > "$VERSION_FILE"
  # 发布包留档（含 contrib/ deploy/ 与文档），只留最近几份
  mkdir -p "$DIST" && rm -rf "$DIST/$TAG" && cp -a "$TMP/x" "$DIST/$TAG"
  cp -a "$TMP/x/README.md" "$TMP/x/OPERATIONS.md" "$DIR/" 2>/dev/null || true
  cp -a "$PREV" "$DIR/zcode-gateway.prev-${TAG}" 2>/dev/null || true
  ls -1dt "$DIR"/zcode-gateway.prev-* 2>/dev/null | tail -n +3 | xargs -r rm -f
  ls -1dt "$DIST"/*/ 2>/dev/null | tail -n +$((KEEP_DIST+1)) | xargs -r rm -rf
  say "✅ 更新成功：$CUR → $TAG（服务 active，账号 $N 个）"
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
