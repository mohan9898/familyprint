#!/bin/bash
# 一键交叉编译双端全部平台产物 → release/
# 产物命名：fp-cloud-<os>-<arch> / fp-agent-<os>-<arch>[.exe]
set -e
export PATH=$PATH:/usr/local/go/bin
cd "$(dirname "$0")"
# 版本号：优先用传入参数，其次用 git tag，最后退化为 dev
VER="${1:-$(git describe --tags --abbrev=0 2>/dev/null || echo dev)}"
OUT=release/$(date +%Y%m%d)
echo "构建版本: $VER"
mkdir -p "$OUT"
rm -rf release/tmp && mkdir -p release/tmp

PLATFORMS="linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64"

for p in $PLATFORMS; do
  GOOS=${p%/*}; GOARCH=${p#*/}
  EXT=""; [ "$GOOS" = "windows" ] && EXT=".exe"
  if [ "$GOARCH" = "arm" ]; then export GOARM=7; else unset GOARM; fi
  # -X main.buildVersion 让程序启动横幅能打印真实版本，便于线上排查
  (cd cloud-go && env CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build \
     -ldflags="-s -w -X main.buildVersion=$VER" -o ../release/tmp/fp-cloud-$GOOS-$GOARCH$EXT .)
  # 家庭端同样注入版本（GOARM 已 export，无需再传；原 $ARM 是笔误）
  (cd agent-go && env CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build \
     -ldflags="-s -w -X main.buildVersion=$VER" -o ../release/tmp/fp-agent-$GOOS-$GOARCH$EXT .)
  echo "✓ $GOOS/$GOARCH"
done
mv release/tmp/* "$OUT"/ && rmdir release/tmp
cd "$OUT" && sha256sum * > sha256sums.txt
echo "─── 产物清单（$OUT）───"
ls -lh
