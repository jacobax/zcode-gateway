#!/usr/bin/env bash
# 扫已跟踪文件里有没有凭据 / 私人信息。
#
# 通用规则写死在这里；个人词条（账号昵称、邮箱、手机号等）放到仓库根的
# .private-terms（已 gitignore，不会被提交），一行一个，# 开头为注释。
#
# 用法：scripts/scan-secrets.sh
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"
fail=0
files=$(git ls-files)

check() { # $1=模式  $2=说明
  local hits
  hits=$(printf '%s\n' "$files" | xargs -r grep -InE -- "$1" 2>/dev/null | head -20 || true)
  if [ -n "$hits" ]; then
    echo "ERROR: 命中 $2（/$1/）："
    printf '%s\n' "$hits" | sed 's/^/    /'
    fail=1
  fi
}

# ---- 通用凭据形态 ----
check 'ghp_[A-Za-z0-9]{30,}'                        'GitHub 个人访问令牌'
check 'github_pat_[A-Za-z0-9_]{20,}'                'GitHub fine-grained 令牌'
check 'gho_[A-Za-z0-9]{30,}'                        'GitHub OAuth 令牌'
check 'sk-[A-Za-z0-9-]{20,}'                        'API key'
check '-----BEGIN [A-Z ]*PRIVATE KEY-----'          '私钥'
check '[0-9]{11}@phone\.local'                      '手机号形态的账号'
check 'eyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.' 'JWT'

# ---- 个人词条（可选）----
if [ -f .private-terms ]; then
  while IFS= read -r term; do
    term=${term%%$'\r'}
    [ -z "$term" ] && continue
    case "$term" in \#*) continue ;; esac
    check "$(printf '%s' "$term" | sed 's/[][\\.*^$+?(){}|]/\\&/g')" "私人词条「$term」"
  done < .private-terms
fi

if [ "$fail" != 0 ]; then
  echo
  echo "提示：确认是误报就把文件加进白名单逻辑，是真凭据就轮换并清历史。"
  exit 1
fi
echo "OK: 未发现凭据或私人信息（扫描 $(printf '%s\n' "$files" | wc -l) 个已跟踪文件）"
