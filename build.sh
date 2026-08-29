#!/usr/bin/env bash
# autoclaw2api 全量构建 + 测试（含压缩二进制输出到 ./bin）。
# 用法：./build.sh
set -euo pipefail
cd "$(dirname "$0")"

export PATH="$PATH:/usr/local/go/bin"
mkdir -p bin

echo "==> vet"
go vet ./...

echo "==> test"
go test ./...

echo "==> build"
for t in server login credit maintain; do
  CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o "bin/autoclaw-$t" "./cmd/$t"
done

echo "==> bin:"
ls -lh bin/