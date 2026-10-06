#!/usr/bin/env bash
# 交叉编译发布产物（纯 Go，无 CGO），输出到 dist/。
# 版本号注入顺序：环境变量 VERSION > git 描述 > dev。
set -euo pipefail
cd "$(dirname "$0")/.."
VERSION="${VERSION:-$(git describe --tags --always 2>/dev/null || echo dev)}"
LDFLAGS="-s -w -X main.version=${VERSION}"
mkdir -p dist
for target in linux/amd64 linux/arm64; do
  os=${target%/*}; arch=${target#*/}
  echo "==> building pagehut-${os}-${arch} (${VERSION})"
  CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" \
    go build -trimpath -ldflags "${LDFLAGS}" -o "dist/pagehut-${os}-${arch}" .
done
ls -lh dist/
