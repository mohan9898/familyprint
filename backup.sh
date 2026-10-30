#!/bin/bash
# 数据备份（自动识别 Docker / 二进制两种运行方式）
# 产出 fp-backup-<日期>.tar.gz（账号、任务记录、配置），通常 <1MB
set -e
cd "$(dirname "$0")"
TS=$(date +%Y%m%d-%H%M)
OUT="fp-backup-$TS.tar.gz"

if docker volume inspect fp_cloud-data >/dev/null 2>&1; then
  echo "检测到 Docker 卷 fp_cloud-data，从卷中备份..."
  docker run --rm -v fp_cloud-data:/data -v "$PWD":/backup alpine \
    tar czf "/backup/$OUT" -C /data .
else
  SRC="${1:-cloud-go/data}"
  [ -d "$SRC" ] || { echo "未找到 Docker 卷，也未找到数据目录 $SRC"; echo "用法: ./backup.sh [数据目录]"; exit 1; }
  echo "从目录 $SRC 备份（二进制运行模式）..."
  tar czf "$OUT" -C "$(dirname "$SRC")" "$(basename "$SRC")"
fi
sha256sum "$OUT" > "$OUT.sha256"
echo "✅ 备份完成：$PWD/$OUT"
echo "   建议异地保存一份（NAS/网盘）；恢复用 ./restore.sh $OUT"
