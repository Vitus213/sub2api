#!/usr/bin/env bash
# scripts/teardown.sh — 停止并清理 e2e 临时环境，保留仓库代码与 skills 不动。
# 按容器名清理，不依赖 compose project 名；兼容跨次运行的容器命名漂移。
set -euo pipefail
SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(cd "$SKILL_DIR/../.." && pwd)"

COLIMA_PROFILE="${COLIMA_PROFILE:-swebench}"
export DOCKER_HOST="unix://${DOCKER_HOST_SOCK:-$HOME/.config/colima/${COLIMA_PROFILE}/docker.sock}"

log() { printf '[teardown] %s\n' "$*" >&2; }

log "stopping sub2api + all known e2e containers (by name)"
docker rm -f \
  sub2api-e2e \
  sub2api-e2e-postgres-1 sub2api-e2e-redis-1 \
  sub2api-langfuse-langfuse-web-1 sub2api-langfuse-langfuse-worker-1 \
  sub2api-langfuse-postgres-1 sub2api-langfuse-redis-1 \
  sub2api-langfuse-clickhouse-1 sub2api-langfuse-minio-1 \
  langfuse-langfuse-web-1 langfuse-langfuse-worker-1 \
  langfuse-postgres-1 langfuse-redis-1 langfuse-clickhouse-1 langfuse-minio-1 \
  >/dev/null 2>&1 || true

log "removing volumes"
docker volume rm -f \
  sub2api-e2e-data \
  langfuse_postgres_data langfuse_clickhouse_data langfuse_clickhouse_logs langfuse_minio_data langfuse_redis_data \
  sub2api-langfuse_postgres_data sub2api-langfuse_clickhouse_data sub2api-langfuse_clickhouse_logs sub2api-langfuse_minio_data sub2api-langfuse_redis_data \
  >/dev/null 2>&1 || true

log "teardown complete (binary still at $REPO_ROOT/.e2e-bin/sub2api)"
echo "OK"