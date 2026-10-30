#!/bin/bash
# 数据恢复（自动识别 Docker / 二进制两种运行方式）
# 用法: ./restore.sh fp-backup-20261030-1600.tar.gz
set -e
cd "$(dirname "$0")"
[ -f "$1" ] || { echo "用法: ./restore.sh <备份文件.tar.gz>"; exit 1; }

if docker volume inspect fp_cloud-data >/dev/null 2>&1; then
  echo "检测到 Docker 卷，恢复到卷中..."
  docker compose -f docker-compose.cloud.yml up -d cloud 2>/dev/null || docker compose up -d cloud
  sleep 3
  docker run --rm -v fp_cloud-data:/data -v "$PWD":/backup alpine \
    sh -c "rm -rf /data/* && tar xzf /backup/$1 -C /data"
  docker compose -f docker-compose.cloud.yml restart cloud 2>/dev/null || docker compose restart cloud
else
  echo "恢复到本地目录 cloud-go/data/（二进制运行模式）..."
  mkdir -p cloud-go
  tar xzf "$1" -C cloud-go/
fi
echo "✅ 恢复完成。若换了 VPS，请确认域名 DNS 已指向新 IP（TTL 建议 300 秒）"
