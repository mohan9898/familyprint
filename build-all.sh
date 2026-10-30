#!/bin/bash
# 一键交叉编译双端全部平台产物 → release/
# 产物命名：fp-cloud-<os>-<arch> / fp-agent-<os>-<arch>[.exe]
set -e
export PATH=$PATH:/usr/local/go/bin
cd "$(dirname "$0")"
OUT=release/$(date +%Y%m%d)
mkdir -p "$OUT"
rm -rf release/tmp && mkdir -p release/tmp

PLATFORMS="linux/amd64 linux/arm64 linux/arm darwin/amd64 darwin/arm64 windows/amd64"

for p in $PLATFORMS; do
  GOOS=${p%/*}; GOARCH=${p#*/}
  EXT=""; [ "$GOOS" = "windows" ] && EXT=".exe"
  if [ "$GOARCH" = "arm" ]; then export GOARM=7; else unset GOARM; fi
  (cd cloud-go && env CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH go build -ldflags="-s -w" -o ../release/tmp/fp-cloud-$GOOS-$GOARCH$EXT .)
  (cd agent-go && CGO_ENABLED=0 GOOS=$GOOS GOARCH=$GOARCH $ARM go build -ldflags="-s -w" -o ../release/tmp/fp-agent-$GOOS-$GOARCH$EXT .)
  echo "✓ $GOOS/$GOARCH"
done
mv release/tmp/* "$OUT"/ && rmdir release/tmp
cd "$OUT" && sha256sum * > sha256sums.txt
echo "─── 产物清单（$OUT）───"
ls -lh
