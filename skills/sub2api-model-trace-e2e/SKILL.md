---
name: sub2api-model-trace-e2e
description: |
  运行 sub2api 模型请求 OTEL→Langfuse 端到端验证：可选择真实 Langfuse v3 黑盒 smoke，或包含后端/前端全套测试、race、生产构建、200-item 异步批量、真实追踪断言和性能基线的全规模验收。

  触发条件（满足任一）：
  1. 用户要跑模型追踪端到端测试（如"跑一下 otel e2e""验证 langfuse 收到 trace""端到端 smoke"）。
  2. 用户改了 backend/internal/modeltrace、OpenSpec add-model-request-otel-tracing 或网关 OTEL 相关代码后要验证。
  3. 用户明确点名本 skill（如"用 sub2api-model-trace-e2e"）。

  不触发（留给对应流程）：
  - 只跑 Go 单测（`go test ./internal/modeltrace/...`）不涉及 Langfuse 部署时，由主 agent 直接跑。
  - 部署真实 Langfuse 生产环境，留给运维流程。
  - 改动 sub2api 业务代码本身而非验证追踪，留给 tdd 或 reviewing-code-changes。

  默认路由：普通追踪改动运行 `run_e2e.sh`；用户要求“全部/全规模/完整验收”时运行 `run_full_e2e.sh`，其中真实 Langfuse 黑盒固定以 200 个异步 item 验证配置上限。
---

# Sub2API 模型追踪端到端 Smoke

## 核心规则（最高优先级）

1. **真实可观察证据高于“命令跑完”**。黑盒 smoke 必须验证 sub2api/Langfuse 健康、身份边界、配置闭环、失败/成功/failover/SSE/batch Trace 层级、状态、会话、截断和秘密零泄漏。全规模验收还必须完整通过后端与前端测试、相关 race、生产构建、200-item 异步批量、真实 Langfuse 查询、性能基线与生成物检查；任一阶段失败均不得以其他阶段成功代替。
2. **本地隔离优先**。Langfuse 端点、sub2api DB、API Key 全部用脚本内本地测试值（如 `pk-lf-local`/`sk-lf-local`）；禁止写进提交、PR 或日志。查看产物时凭据字段必须遮蔽。
3. **远端用 GitHub**。本仓库 `origin` = `git@github.com:Vitus213/sub2api.git`，跨 fork PR 用 `gh pr create --repo Wei-Shaw/sub2api`。**禁止**触发 `antcode-skill`，本仓库与 AntCode 无关。
4. **不主动提交**。脚本只启动容器、编译、发请求、查 ClickHouse；不 `git commit`、不 `git push`、不 `gh pr create`，除非用户明确要求。
5. **503 是预期 HTTP 状态**。e2e 环境不配真实上游 LLM 账号，因此 `/v1/chat/completions` 返回 503 属正常；trace 仍要落 Langfuse，这是 fail-open 旁路的验证点，不要尝试修复 503。
6. **不可写只读路径**。`scripts/run_e2e.sh` 编译 sub2api 时只挂载 `backend/` 只读到容器，不允许修改仓库代码；只允许写 `.e2e-bin/`、`.e2e-tmp/` 和 docker volume。
7. **配置闭环必须先于 Trace 验证**。完整 smoke 必须先验证部署来源公开响应不含 secret、远端明文 HTTP 更新被拒绝、有效运行时整体更新成功、非秘密更新保留 secret、过期 config version 返回 409 且旧配置不变；任一断言失败不得继续用最终 Trace 掩盖。

优先级：可观察证据 > 本地隔离与凭据保护 > GitHub 远端规约 > 不主动写入。低优先级不得绕过高优先级。

## 依赖清单

| 类型 | 名称 | 用途 | 必须/可选 |
|------|------|------|-----------|
| 容器 | Colima profile `swebench` | 本地 Docker 引擎与端口转发 | 必须 |
| CLI | `docker`、legacy `docker-compose` | 启动 Langfuse 与 sub2api 依赖；当前 runner 直接调用带横线命令 | 必须 |
| CLI | `colima` | 跨 VM SSH 与端口转发 | 必须 |
| CLI | `curl`、`jq`、`openssl`、`nc` | API 调用、JSON 解析、生成随机 key、端口占用检查 | 必须 |
| 镜像 | `golang:1.26.5` | 编译 sub2api linux/arm64 二进制（非 alpine，带 git） | 必须 |
| 镜像 | `langfuse/langfuse:3`、`langfuse-worker:3`、`clickhouse-server`、`postgres:17`、`redis:7`、`minio/minio` | Langfuse 全栈与 sub2api 依赖 | 必须 |
| 代码 | 仓库 `backend/` 目录 + `-tags embed` 前端 dist | 编译带嵌入前端的二进制 | 必须 |
| 文件 | `assets/langfuse-compose.yml` | Langfuse 全栈编排 | 必须 |
| 文件 | `assets/sub2api-deps-compose.yml` | sub2api 专用 pg+redis | 必须 |
| 脚本 | `scripts/run_e2e.sh` | 一键跑通 e2e smoke | 必须 |
| 脚本 | `scripts/run_full_e2e.sh` | 串行运行完整后端/前端测试、race、生产构建、200-item 真实 Langfuse 黑盒、性能基线与生成物门禁 | 全规模验收必须 |
| 脚本 | `scripts/teardown.sh` | 清理容器与卷 | 可选 |

## 场景与执行 SOP

### 场景一：跑端到端 smoke

**触发**：用户要验证模型追踪链路、改了 modeltrace 代码、或点名本 skill。

**步骤**：

1. 确认 sub2api 仓库根目录、Colima profile `swebench` 已启动：
   ```bash
   colima status swebench
   ```
   失败则停止并提示 `colima start swebench`，不自动启动。
2. 确认宿主端口 3000、15432、16379、8080 未被占用；占用时停止并报告哪个端口占用了什么，不自动 kill。
3. 运行一键脚本，**不要**并行执行其他写容器操作：
   ```bash
   bash skills/sub2api-model-trace-e2e/scripts/run_e2e.sh
   ```
4. 脚本内部顺序为：启动 Langfuse 与 sub2api 依赖 → 编译并启动服务 → 验证部署/运行时配置闭环 → 建立身份和本地 provider fixtures → 覆盖匿名/未知/控制面、503 单根、截断与媒体、disabled、1 MiB 边界、session/cache-key、429→200、所有尝试失败、SSE、进行中配置快照、500/慢 exporter fail-open、Gemini batch → 查询真实 Langfuse ClickHouse 并执行 cardinality、父子、状态和秘密零泄漏断言。
5. 成功的合并命令输出必须包含 `trace_id`、整数 `1`、`VERIFY_OK` 和末尾的 `full-scale e2e passed`；失败时 stderr 含 `[e2e][ERROR]` 行，按行内容定位失败阶段，不自动缩窄覆盖重试。
6. 验证通过后向用户报告：
   - 配置来源、版本、secret 不回显/保留、CAS 冲突与远端 HTTP 拒绝结果
   - trace_id、根 span name、user_id、session_id、请求/身份元数据
   - 匿名、未知 Key、控制面均为 0 Trace；disabled Key 为 1 个 ERROR 根 Span
   - 明确标注 HTTP 503/401 均为预期，且身份建立前不导出 Prompt/Response
7. 用户确认后再决定是否 `scripts/teardown.sh`；不自动清理。

**成功信号**：退出码为 0，合并命令输出同时包含 `VERIFY_OK`、`batch trace passed at configured scale`、`sensitive-content gate passed` 和 `full-scale e2e passed`。其中 `VERIFY_OK` 写入 stdout，其余诊断日志写入 stderr；任一缺失均视为失败。

**失败分支**：
- Langfuse health 不通：检查 `docker-compose -f .e2e-tmp/langfuse/docker-compose.yml logs langfuse-web`。本地偶发镜像拉取超时可重试；远端 Linux 出现 DNS、代理、Buildx 或 Corepack 错误时，按 `references/environment.md` 的“远端 Linux 构建与持久部署”逐层验证，不要直接换不可信镜像。
- sub2api 启动失败：`docker logs sub2api-e2e` 看 `Failed to initialize application`，常见是 DB 连接（检查 15432 占用）或 `invalid model_tracing deployment config`（检查 endpoint 必须是符合 loopback 规则的 URL）。
- trace 不落库：检查 sub2api 日志中的 `modeltrace` error 和 ClickHouse `system.errors`；脚本自身负责 bounded wait，不用手工 sleep 冒充稳定性。
- ClickHouse 查询语法错：用 `docker exec sub2api-langfuse-clickhouse-1 clickhouse-client -u clickhouse --password clickhouse -q "DESCRIBE traces"` 核对列名；Langfuse v3 的 `metadata` 是 `Map`，`session_id` 是 `Nullable(String)`。

### 场景二：跑全规模验收

**触发**：用户要求“全部”“全规模”“完整验收”，或准备对模型追踪交付作最终完成声明。

**步骤**：

1. 从仓库根目录串行运行：
   ```bash
   bash skills/sub2api-model-trace-e2e/scripts/run_full_e2e.sh
   ```
2. 脚本依次执行 9 个门禁：harness/deploy 契约 → 全后端测试 → modeltrace/handler/service/routes race → 前端 lint+typecheck → 全前端测试 → production build → `BATCH_ITEM_COUNT=200` 真实 Langfuse 黑盒 → request-path benchmark → 生成物检查。
3. 只有退出码为 0、stdout 含 `FULL_E2E_OK`，且最后报告 `batch_items=200` 才通过。任一阶段非零立即失败，不缩窄后重试来冒充全规模通过。
4. 性能阶段记录当前机器基线，不设跨机器绝对阈值；报告必须引用实际 `ns/op`、`B/op`、`allocs/op`，不得把一次本机 benchmark 推断为生产性能结论。

**成功信号**：`FULL_E2E_OK` 以及 `full-scale acceptance passed: backend=all frontend=all race=modeltrace build=production langfuse=real batch_items=200`。

**失败分支**：按 `phase N/9` 定位第一个失败门禁并修复；不得跳过失败阶段继续形成整体通过结论。


### 场景三：保留环境只重跑请求

**触发**：Langfuse 已运行、sub2api 已启动、只想再发一次请求验证。

**步骤**：

1. 确认 `sub2api-e2e` 容器仍在：
   ```bash
   docker ps --filter name=sub2api-e2e --format '{{.Status}}'
   ```
2. 从 sub2api DB 读出最近一把 API Key（或在脚本第一次运行后保留 `APIKEY` 环境变量）：
   ```bash
   APIKEY=$(docker exec sub2api-deps-postgres-1 psql -U sub2api -d sub2api -tAc "SELECT key FROM api_keys WHERE name='e2e-key' LIMIT 1")
   ```
3. 直接发请求并查 ClickHouse：
   ```bash
   SESSION_ID="e2e-$(date +%s)"
   curl -X POST http://localhost:8080/v1/chat/completions \
     -H "Content-Type: application/json" \
     -H "Authorization: Bearer $APIKEY" \
     -d "{\"model\":\"gpt-4\",\"messages\":[{\"role\":\"user\",\"content\":\"re-run $SESSION_ID\"}],\"session_id\":\"$SESSION_ID\"}"
   sleep 6
   docker exec sub2api-langfuse-clickhouse-1 clickhouse-client -u clickhouse --password clickhouse \
     -q "SELECT id, name, user_id FROM traces WHERE session_id='$SESSION_ID' FORMAT TabSeparated"
   ```
4. 只要 `traces` 表返回 1 行且 `observations` 恰有 1 个根 SPAN、没有 GENERATION 即通过；此快速路径不替代完整脚本的匿名/未知 Key/控制面/disabled Key 边界断言。

### 场景四：清理环境

**触发**：用户要回收资源、切换分支前、或卡住要重来。

**步骤**：

1. 运行：
   ```bash
   bash skills/sub2api-model-trace-e2e/scripts/teardown.sh
   ```
2. 脚本停止 sub2api-e2e、sub2api-deps、langfuse 全栈，删 docker volume `sub2api-e2e-data`。
3. `skills/` 目录、`.e2e-bin/sub2api` 二进制、`.e2e-tmp/` compose 副本保留，下次 `run_e2e.sh` 会复用或重建。
4. 完全清理（含二进制）：
   ```bash
   rm -rf .e2e-bin .e2e-tmp
   docker volume prune -f
   ```

### 场景五：负向路由

**触发**：用户只想跑 Go 单元测试、评审 modeltrace 代码、或部署生产 Langfuse。

**步骤**：

1. 只跑单元测试：不启动脚本，主 agent 直接：
   ```bash
   colima ssh --profile swebench -- docker run --rm -e GOPROXY=https://goproxy.cn,direct \
     -v sub2api-go-mod-cache:/go/pkg/mod -v sub2api-go-build-cache:/root/.cache/go-build \
     -v "$PWD/backend:/app" -w /app golang:1.26.5 \
     go test ./internal/modeltrace/... -count=1
   ```
2. 评审 modeltrace 代码：路由到 `reviewing-code-changes`，不接管。
3. 部署生产 Langfuse：提示用户生产 Langfuse 需要 HTTPS、备份、auth、保留策略，非本 skill 范围。

## 命令速查

所有命令从仓库根目录运行，`SKILL_DIR=skills/sub2api-model-trace-e2e`。

| 需求 | 完整命令 |
|------|----------|
| 跑完整 e2e smoke | `bash $SKILL_DIR/scripts/run_e2e.sh` |
| 跑全规模完整验收 | `bash $SKILL_DIR/scripts/run_full_e2e.sh` |
| 清理环境 | `bash $SKILL_DIR/scripts/teardown.sh` |
| 查 Langfuse 健康 | `curl -s -o /dev/null -w '%{http_code}' http://localhost:3000/api/public/health` |
| 查 sub2api 健康 | `curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/health` |
| 查 Langfuse traces | `docker exec sub2api-langfuse-clickhouse-1 clickhouse-client -u clickhouse --password clickhouse -q "SELECT id, name, user_id, session_id FROM traces ORDER BY timestamp DESC LIMIT 5 FORMAT TabSeparated"` |
| 查 observations | `docker exec sub2api-langfuse-clickhouse-1 clickhouse-client -u clickhouse --password clickhouse -q "SELECT name, type, provided_model_name, parent_observation_id FROM observations ORDER BY start_time DESC LIMIT 10 FORMAT TabSeparated"` |
| 看 sub2api 日志 | `docker logs sub2api-e2e 2>&1 \| tail -50` |
| 看 Langfuse 日志 | `docker-compose -f .e2e-tmp/langfuse/docker-compose.yml logs langfuse-web 2>&1 \| tail -50` |
| 跑 Go 单测（不启动 Langfuse） | 见场景五 |
| 切回主分支 | `git checkout main`（**不**主动执行，除非用户要求） |

## 参考文档（按需读取）

| 场景 | 文件 |
|------|------|
| 端口、凭据、镜像版本、远端 Linux 构建/代理、Endpoint 校验、ClickHouse 表结构、踩坑速查 | `references/environment.md` |
| OpenSpec 规格、failed trace 场景、503 产生原因、fail-open 设计依据 | `references/otel-spec-mapping.md` |

只在对应场景命中时读取；主 SOP 以本文件的核心规则与成功信号为最高优先级。

## 测试 prompt

见 `test-prompts.json`，覆盖典型成功路径、503 失败也追踪、端口占用失败路径。