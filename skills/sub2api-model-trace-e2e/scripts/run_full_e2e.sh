#!/usr/bin/env bash
# 全规模模型追踪验收：完整后端/前端/竞态/构建门禁 + 真实 Langfuse 黑盒链路 + 性能基线。
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"
export PATH="/opt/homebrew/bin:$PATH"

log() { printf '[modeltrace-full-e2e] %s\n' "$*" >&2; }
fail() { printf '[modeltrace-full-e2e][ERROR] %s\n' "$*" >&2; exit 1; }
need_cmd() { command -v "$1" >/dev/null 2>&1 || fail "missing command: $1"; }

need_cmd bash
need_cmd go
need_cmd make
need_cmd pnpm

[[ -f "$REPO_ROOT/backend/go.mod" ]] || fail "repository root not found: $REPO_ROOT"

log "phase 1/9: validating harness and deployment contracts"
bash -n "$SCRIPT_DIR/run_e2e.sh"
bash -n "$SCRIPT_DIR/teardown.sh"
bash "$REPO_ROOT/deploy/tests/model-tracing-compose-test.sh"

log "phase 2/9: running complete backend test suite"
(
  cd "$REPO_ROOT/backend"
  go test ./... -count=1
)

log "phase 3/9: running model-tracing race suite"
(
  cd "$REPO_ROOT/backend"
  go test -race \
    ./internal/modeltrace/... \
    ./internal/handler/... \
    ./internal/service/... \
    ./internal/server/routes/... \
    -count=1
)

log "phase 4/9: running frontend lint and typecheck"
pnpm --dir "$REPO_ROOT/frontend" run lint:check
pnpm --dir "$REPO_ROOT/frontend" run typecheck

log "phase 5/9: running complete frontend test suite"
pnpm --dir "$REPO_ROOT/frontend" run test:run

log "phase 6/9: building production backend and frontend artifacts"
make -C "$REPO_ROOT" build

log "phase 7/9: running real Langfuse full-scale black-box suite"
BATCH_ITEM_COUNT=200 bash "$SCRIPT_DIR/run_e2e.sh"

log "phase 8/9: recording request-path allocation and latency baseline"
(
  cd "$REPO_ROOT/backend"
  go test ./internal/modeltrace \
    -run '^$' \
    -bench '^BenchmarkModelTraceRequestOverhead$' \
    -benchmem \
    -count=3
)

log "phase 9/9: verifying generated artifacts"
[[ -x "$REPO_ROOT/backend/bin/server" ]] || fail "backend binary missing: backend/bin/server"
[[ -f "$REPO_ROOT/backend/internal/web/dist/index.html" ]] || fail "embedded frontend artifact missing"

echo "FULL_E2E_OK"
log "full-scale acceptance passed: backend=all frontend=all race=modeltrace build=production langfuse=real batch_items=200"
