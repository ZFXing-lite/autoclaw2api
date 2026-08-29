#!/usr/bin/env bash
# autoclaw2api 手动维护：全部账号执行 积分刷新 + token 预刷新 + 沙箱确保/唤醒。
# 相当于调度器的一轮，便于手动触发排查。
# 用法：./maintain.sh [-region cn] [-auths ./auths]
set -euo pipefail

REGION="cn"
AUTH_DIR="${AUTH_DIR:-./auths}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    -region|--region) REGION="$2"; shift 2 ;;
    -auths|--auths) AUTH_DIR="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 1 ;;
  esac
done

if command -v autoclaw-maintain >/dev/null 2>&1; then
  autoclaw-maintain -region "$REGION" -auths "$AUTH_DIR"
elif command -v go >/dev/null 2>&1; then
  go run ./cmd/maintain -region "$REGION" -auths "$AUTH_DIR"
else
  echo "error: autoclaw-maintain binary not found and go not available" >&2
  exit 1
fi