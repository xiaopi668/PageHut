#!/usr/bin/env bash
# 交叉编译发布产物（纯 Go，无 CGO），输出到 dist/。
set -euo pipefail
cd "$(dirname "$0")/.."
mkdir -p dist
for target in linux/amd64 linux/arm64; do
  os=${target%/*}; arch=${target#*/}
  echo "==> building pagehut-${os}-${arch}"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" \
    go build -trimpath -ldflags "-s -w" -o "dist/pagehut-${os}-${arch}" .
done
ls -lh dist/
