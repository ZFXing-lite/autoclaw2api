#!/usr/bin/env bash
# autoclaw2api 登录脚本：发短信验证码 → 登录 → 落盘凭证 → 重启容器加载新账号。
# 用法：
#   ./login.sh                      # 交互输入手机号与验证码
#   ./login.sh <手机号> <验证码>  # 非交互（验证码可能已回显，慎用）
#   ./login.sh -region global       # 海外区
#   AUTH_DIR=/data/auths ./login.sh # 自定义凭证目录（容器内）
set -euo pipefail

REGION="cn"
AUTH_DIR="${AUTH_DIR:-./auths}"
PHONE="${1:-}"
CODE="${2:-}"

ARGS=()
if [[ -n "$PHONE" ]]; then ARGS+=("-phone" "$PHONE"); fi
if [[ -n "$CODE" ]]; then ARGS+=("-code" "$CODE"); fi
if [[ "$1" == "-region" || "$1" == "--region" ]]; then REGION="$2"; ARGS=("-region" "$REGION"); fi

# 优先用容器内编译的二进制；否则用本地 go run
if command -v autoclaw-login >/dev/null 2>&1; then
  autoclaw-login -region "$REGION" -out "$AUTH_DIR" "${ARGS[@]}"
elif command -v go >/dev/null 2>&1; then
  go run ./cmd/login -region "$REGION" -out "$AUTH_DIR" "${ARGS[@]}"
else
  echo "error: autoclaw-login binary not found and go not available" >&2
  exit 1
fi

# 重启容器（docker compose 场景）。容器内无 docker 则跳过提示。
if command -v docker >/dev/null 2>&1 && [[ ! -f /.dockerenv ]]; then
  CONTAINER="$(docker ps --format '{{.Names}}' | grep -i autoclaw || true)"
  if [[ -n "$CONTAINER" ]]; then
    echo "restarting container $CONTAINER ..."
    docker restart "$CONTAINER"
  else
    echo "container not found; run 'docker rebuild && docker restart' yourself after adding the account."
  fi
fi
echo "done."