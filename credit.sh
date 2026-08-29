#!/usr/bin/env bash
# autoclaw2api 积分查询脚本（自动化脚本用，输出纯文本 table）。
# 用法：
#   ./credit.sh                # 全部 cn 账号
#   ./credit.sh -region global # 海外区
#   ./credit.sh -json          # 机器可读 JSON
set -euo pipefail

REGION="cn"
AUTH_DIR="${AUTH_DIR:-./auths}"
JSON_FLAG=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    -region|--region) REGION="$2"; shift 2 ;;
    -json) JSON_FLAG="-json"; shift ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

if command -v autoclaw-credit >/dev/null 2>&1; then
  autoclaw-credit -region "$REGION" -auths "$AUTH_DIR" $JSON_FLAG
elif command -v go >/dev/null 2>&1; then
  go run ./cmd/credit -region "$REGION" -auths "$AUTH_DIR" $JSON_FLAG
else
  echo "error: autoclaw-credit binary not found and go not available" >&2
  exit 1
fi