# 环境与踩坑速查

## 端口

| 端口 | 服务 | 来源 compose |
|------|------|-------------|
| 443 | 本地确定性 Gemini TLS fixture（File API + Batch API） | `upstream-fixture.go` + 本地一次性 CA + `--add-host` |
| 3000 | Langfuse web UI + OTLP endpoint | langfuse-compose.yml |
| 3030 | Langfuse worker | langfuse-compose.yml |
| 8123 | ClickHouse HTTP | langfuse-compose.yml |
| 9000 | ClickHouse native | langfuse-compose.yml |
| 9090 | MinIO API | langfuse-compose.yml |
| 9091 | MinIO console | langfuse-compose.yml |
| 6379 | Langfuse Redis（内部） | langfuse-compose.yml |
| 5432 | Langfuse Postgres（内部） | langfuse-compose.yml |
| 15432 | sub2api 专用 Postgres | sub2api-deps-compose.yml |
| 16379 | sub2api 专用 Redis | sub2api-deps-compose.yml |
| 8080 | sub2api server（容器内） | run_e2e.sh `--network host` |
| 18080 | 预留 sub2api 宿主映射（未使用） | — |
| 18081 | 本地确定性 Anthropic fixture（`/fail` 429、`/ok` 200 SSE） | `upstream-fixture.go` + `run_e2e.sh --network host` |

实际从宿主访问 sub2api 用 `http://localhost:8080`，因为 `--network host` + Colima 端口转发。

## 凭据

| 用途 | 值 | 来源 |
|------|-----|------|
| Langfuse 登录 | `admin@local.dev` / `admin123456` | `LANGFUSE_INIT_USER_*` |
| Langfuse 项目 public key | `pk-lf-local` | `LANGFUSE_INIT_PROJECT_PUBLIC_KEY` |
| Langfuse 项目 secret key | `sk-lf-local` | `LANGFUSE_INIT_PROJECT_SECRET_KEY` |
| sub2api admin | `admin@e2e.local` / `admin12345` | `ADMIN_EMAIL` / `ADMIN_PASSWORD` |
| sub2api API Key | `sk-e2e-<16字节hex>` | `run_e2e.sh` `openssl rand -hex 16` |
| fixture 上游凭据 | 每次 fresh run 随机生成，仅注入本地容器 | `GEMINI_BATCH_API_KEY` / batch API Key；完整值不得输出 |
| sub2api Postgres | `sub2api` / `sub2api` | `POSTGRES_USER` / `POSTGRES_PASSWORD` |

**所有凭据仅本地，不得写入提交、文档（除本 reference）、PR、log。日志输出时遮蔽为 `sk-e2e-***`。**

## 镜像版本

| 镜像 | 版本 | 备注 |
|------|------|------|
| `golang` | `1.26.5`（非 alpine） | 编译 sub2api，必须带 git 因为 `go mod` 需要 |
| `langfuse/langfuse` | `3` 浮动标签；脚本必须从 `/api/public/health` 取得语义版本并断言 `>= 3.22.0` | 同时记录运行容器对应 image digest、OCI revision/version；2026-07-22 两次 fresh 观测先后为 `3.222.0`、`3.223.0`，证明同一标签会漂移 |
| `langfuse/langfuse-worker` | `3` 浮动标签 | 与 web 使用同一 compose 启动；版本门禁以公开 health + web 镜像 OCI 标签为准 |
| `clickhouse/clickhouse-server` | `latest` | Langfuse compose 默认 |
| `postgres` | `17` | Langfuse 与 sub2api 都用 |
| `redis` | `7` | 同上 |
| `minio/minio` | `latest` | Langfuse 对象存储 |

docker hub 偶发拉取超时（`EOF` / `failed to fetch anonymous token`），重试即可；不要改用第三方镜像源以免版本漂移。

`run_e2e.sh` 不靠 `:3` 标签猜版本：health JSON 的 `version` 是运行时公开证据，OCI `org.opencontainers.image.version` 必须与之相同；脚本还记录 `RepoDigest` 与 `org.opencontainers.image.revision`，任何一项缺失或版本低于 `3.22.0` 都失败。

## 远端 Linux 构建与持久部署

`run_e2e.sh` 当前是 Colima 专用测试 harness：它调用 `colima ssh`、使用 legacy `docker-compose`，并固定编译 `linux/arm64`。在原生 Linux/x86_64 服务器上，不要原样执行该脚本；应复用本 reference 的端口、loopback endpoint、版本门禁和 ClickHouse 断言作为部署契约。

### Slash 分支与源码校验

分支名包含 `/`，必须把完整 ref 传给 Git。GitHub 的 `/tree/otel/model-trace` 页面可能被解释为分支 `otel` 下的 `model-trace` 路径，不能作为 branch 解析依据：

```bash
git clone --branch 'otel/model-trace' --single-branch https://github.com/Vitus213/sub2api.git
git rev-parse HEAD
```

部署记录必须保存实际 commit SHA；不得只记录网页 URL 或浮动 branch 名。

### Git、Docker 与 BuildKit 代理

先分别验证代理和目标站点，避免把 DNS 污染、出口阻断误判为镜像不存在：

```bash
PROXY_URL=http://proxy.example:7890
curl -x "$PROXY_URL" -fsSI https://github.com
REGISTRY_STATUS=$(curl -x "$PROXY_URL" -sS -o /dev/null -w '%{http_code}' https://registry-1.docker.io/v2/)
test "$REGISTRY_STATUS" = 401
curl -x "$PROXY_URL" -fsSI https://registry.npmjs.org/pnpm
```

Docker Registry 返回 `401 Unauthorized` 是匿名 Registry v2 的正常挑战，说明网络已通。Git 可直接设置 `http.proxy`/`https.proxy`；Docker daemon 必须通过 systemd drop-in 设置 `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY`，随后执行 `systemctl daemon-reload && systemctl restart docker`，并以 `docker info` 的 Proxy 字段作为生效证据。不要只在当前 shell export：daemon 拉镜像不会继承客户端 shell 环境。

Ubuntu 的 `docker.io` 包可能没有 Buildx。出现 legacy builder 的 `failed to parse platform`、`${BUILDPLATFORM}` 为空或 `invalid OS component` 时，安装 `docker-buildx`，确认 `docker buildx ls` 中 builder 为 `running`，再使用 `docker buildx build --load`。

BuildKit 在正式构建前仍会解析 Dockerfile frontend 和各 `FROM` 的 registry metadata。代理不稳定时先显式拉取，之后重试构建：

```bash
docker pull docker.io/docker/dockerfile:1.7
docker pull docker.io/library/node:24-alpine
docker pull docker.io/library/golang:1.26.5-alpine
docker pull docker.io/library/postgres:18-alpine
docker pull docker.io/library/alpine:3.21
```

### Node 24/Corepack 代理

`HTTP_PROXY`/`HTTPS_PROXY` build args 对基础镜像拉取生效，不代表 Node 24 的内置 `fetch` 会读取它们。若 `corepack prepare pnpm@9 --activate` 报 `Error when performing the request to https://registry.npmjs.org/pnpm`，在部署用 Dockerfile 的 `frontend-builder` stage 内启用 Node 环境代理：

```dockerfile
FROM --platform=${BUILDPLATFORM} ${NODE_IMAGE} AS frontend-builder
ARG NPM_CONFIG_REGISTRY
ENV NODE_USE_ENV_PROXY=1
```

`ENV` 必须位于第一个 `FROM` 之后；放在全局 `ARG` 区会报 `no build stage in current context`。`NPM_CONFIG_REGISTRY` 只控制后续 pnpm registry，不能替代 `NODE_USE_ENV_PROXY=1` 解决 Corepack 自身联网。远端构建时同时传递大小写 proxy build args，以兼容不同工具：

```bash
docker buildx build --load \
  --build-arg HTTP_PROXY="$PROXY_URL" --build-arg HTTPS_PROXY="$PROXY_URL" \
  --build-arg http_proxy="$PROXY_URL" --build-arg https_proxy="$PROXY_URL" \
  --build-arg NPM_CONFIG_REGISTRY=https://registry.npmmirror.com \
  -t sub2api:model-trace .
```

### 持久部署的版本与网络契约

E2E compose 故意使用 `langfuse:3` 做 fresh compatibility smoke；持久部署不能继续依赖该浮动标签。启动后必须同时核对 health version、OCI version/revision 和 RepoDigest，再把 Web/Worker digest 写入部署 compose。本次已验证的 `3.223.0` 仅是观测证据，不代表未来 `:3` 仍指向同一镜像。

明文 OTLP endpoint 仍受 loopback 规则约束。原生 Linux 部署可让 Sub2API 使用 `network_mode: host`，Langfuse Web 仅映射宿主 `127.0.0.1:3000:3000`，然后配置 `MODEL_TRACING_ENDPOINT=http://127.0.0.1:3000`；外部 UI 必须经带认证的 HTTPS 反向代理访问，不得把明文 `3000` 暴露到所有接口。Sub2API 的 PostgreSQL/Redis 应映射到独立回环端口（参考 `15432`/`16379`），避免与 Langfuse 的 `5432`/`6379` 冲突。

新初始化管理员的余额可能为 0。已识别请求若返回 `403 INSUFFICIENT_BALANCE`，说明请求尚未到达“无上游账号”的 503 路径，不能据此判断 tracing 失败。部署 smoke 可以临时提高测试用户余额，但必须在 trap 中恢复原值，并删除测试 API key/group；随后仍需以 503、唯一根 Span 和 ClickHouse 实际行作为通过条件。

## Endpoint 校验规则

sub2api 的 `config.validModelTracingEndpoint`（`backend/internal/config/config.go`）强制：
- `https://` 永远允许
- `http://` 仅允许 host 为 `localhost` 或字面量回环 IP（`127.0.0.1`/`::1`）
- 其他 scheme 拒绝
- **不**提供 `InsecureSkipVerify` 或跳过证书校验开关

因此 e2e 必须用 `MODEL_TRACING_ENDPOINT=http://127.0.0.1:3000`，**禁止** `http://host.docker.internal:3000`（非字面回环会被拒）或 `http://sub2api-langfuse-langfuse-web-1:3000`（非 loopback）。

这就是 `run_e2e.sh` 用 `--network host` + `127.0.0.1` 的原因：让 sub2api 容器内 127.0.0.1 等于 VM 的 127.0.0.1，从而既符合 endpoint 校验又能访问 Langfuse 暴露的 3000 端口。

## 默认内容上限门禁

sub2api 启动时只设置 tracing endpoint、公钥、秘密和 `capture_media_content=false`，**不设置**三个 `MODEL_TRACING_*_MAX_BYTES` 环境变量。管理员 GET 首次必须返回部署来源且 `prompt_max_bytes`、`response_max_bytes`、`media_max_bytes` 均为 `1048576`。

完整 smoke 随后按 CAS 版本执行两次运行时切换：
1. 切到 `prompt=2048`、`response=3072`、`media=4096`，验证超限文本的确定性截断标记、默认媒体 descriptor 和 credential/media canary 零泄露。
2. 切回三个 `1048576`，生成 1,040,000 字节正文（完整 JSON 请求体仍不超过 1 MiB），经真实 OTLP 写入 Langfuse；ClickHouse 必须同时保留 head/tail canary、存储长度在 `[1040000, 1048576]`，且不存在截断标记。

这两步验证的是“部署默认可观察 + 运行时小上限真实生效 + 恢复默认后接近上限可真实接收”，不能用静态配置或 fake exporter 代替。

## ClickHouse 表结构（Langfuse v3）

### `traces` 表关键列

| 列名 | 类型 | 说明 |
|------|------|------|
| `id` | String | trace id |
| `name` | String | Trace 当前显示名；同步根通常为 `model.request`，异步续接后可更新为 `model.async.execution`；唯一性按 `id` 判断 |
| `user_id` | String | Langfuse user id（应为 `1`） |
| `session_id` | String | Langfuse session id（应等于请求 body `session_id`） |
| `metadata` | Map(LowCardinality(String), String) | **Map 类型**，不是 JSON 字符串，访问用 `metadata['key']` |
| `input` | Nullable(String) | 客户端请求 body JSON |
| `output` | Nullable(String) | 客户端响应 body JSON |
| `tags` | Array(String) | trace 标签 |

常见错误：`SELECT JSONExtractString(metadata, 'key')` 报 `illegal type: Map`，因为 metadata 是 Map 不是 JSON。正确写法：`metadata['key']`。

异步续接会以同一个 Trace ID 写入更新事件；直接查询底层表可能看到同 ID 的多行版本。脚本用 `uniqExact(id)` 断言只有一个逻辑 Trace，而不是把更新事件误报成多条 Trace。

### `observations` 表关键列

| 列名 | 类型 | 说明 |
|------|------|------|
| `id` | String | observation id |
| `trace_id` | String | 关联 trace |
| `name` | String | span name：同步根 `model.request`、上游尝试 `upstream.attempt.N`、异步 item `model.async.execution` |
| `type` | LowCardinality(String) | `SPAN` / `GENERATION`；真实 outbound attempt 和异步模型执行必须为 `GENERATION` |
| `parent_observation_id` | Nullable(String) | 父 observation；根 span 为 NULL，同步 attempt/异步 item Generation 均直接指向提交根 |
| `provided_model_name` | Nullable(String) | 同步 fixture 为实际 `claude-e2e`；batch fixture 为 `gemini-2.5-flash-image`，均不是未经映射猜出的模型 |
| `input` | Nullable(String) | 该 observation 的输入 |
| `output` | Nullable(String) | 该 observation 的输出 |
| `start_time` / `end_time` | DateTime64(3) | 起止时间 |

## 踩坑速查

| 现象 | 原因 | 解法 |
|------|------|------|
| `config file creation failed: open /data/config.yaml: no such file or directory` | `DATA_DIR` 没挂卷 | `docker volume create sub2api-e2e-data` + `-v sub2api-e2e-data:/data` |
| `invalid model_tracing deployment config; tracing disabled` | endpoint 非 loopback | 改用 `http://127.0.0.1:3000` + `--network host` |
| `database connection failed: pq: password authentication failed for user "postgres"` | AUTO_SETUP 用 `DATABASE_*` 环境变量，不是 `DB_*` | 全部用 `DATABASE_HOST`/`DATABASE_PORT`/`DATABASE_USER`/... |
| `NeedsSetup=false` 跳过 AUTO_SETUP | 上次写的 config.yaml 还在 /data 卷里 | `docker volume rm sub2api-e2e-data` 或脚本里 `DROP SCHEMA public CASCADE` 重置 |
| API Key 响应里 `key: "[openai_token_redacted]"` | sub2api 对 OpenAI 平台 group 的 key 做了 redact 展示 | 直接 `docker exec` 进 PG 用 `UPDATE api_keys SET key='sk-...'` 改成可鉴权值 |
| `/v1/chat/completions` 返回 503 | Key 所属 group 尚未配置上游账号 | 身份/截断/1 MiB 场景的**预期行为**；failover 和 SSE 场景随后挂载本地 fixture 账号并必须返回 200 |
| Langfuse Postgres `traces` 表 0 条 | Langfuse v3 用 ClickHouse 存 traces，PG 只是 metadata | 查 `docker exec sub2api-langfuse-clickhouse-1 clickhouse-client ...` |
| ClickHouse `traces` 表 0 条 | BatchSpanProcessor 默认 5 秒批次 + 网络 | `sleep 6` 后再查 |
| `docker compose` 报 `unknown command` | Colima 内 docker 是老版本 | 用 `docker-compose`（带横线） |
| `docker-compose` 报 `pull access denied for registry.cn-hangzhou.aliyuncs.com/...` | 中间尝试过第三方镜像但没权限 | 回到 `docker.io/library/...` 标准镜像，重试拉取 |
| `git clone` 报 `GnuTLS recv error (-110)` 或 GitHub 443 timeout | 远端 DNS/出口不可用，不是 slash 分支不存在 | 用 `curl -x "$PROXY_URL"` 分别验证 GitHub、Docker Registry、npm；配置 Git 与 Docker daemon 代理后重新 clone/pull |
| legacy builder 报 `${BUILDPLATFORM}` 为空或 `invalid OS component` | Ubuntu `docker.io` 未安装 Buildx，Dockerfile 被旧 builder 解析 | 安装 `docker-buildx`，确认 `docker buildx ls` 为 running，再用 `docker buildx build --load` |
| BuildKit 在 `docker/dockerfile:1.7` 或 `FROM` metadata 阶段 timeout | frontend/base image metadata 仍需访问 Docker Hub | 先显式 `docker pull` Dockerfile frontend 与全部基础镜像，再重试；不要删除已下载 cache |
| Corepack 报 `Error when performing the request to registry.npmjs.org/pnpm` | Node 24 `fetch` 默认不读取 proxy env；`NPM_CONFIG_REGISTRY` 不控制 Corepack 自身 | 在 `frontend-builder` stage 设置 `ENV NODE_USE_ENV_PROXY=1`，并传递大小写 proxy build args |
| Dockerfile 加了 `NODE_USE_ENV_PROXY` 后报 `no build stage in current context` | `ENV` 错放在第一个 `FROM` 之前 | 把 `ENV NODE_USE_ENV_PROXY=1` 移到 `frontend-builder` 的 `FROM`/`ARG` 之后 |
| fresh admin 的已识别请求返回 `403 INSUFFICIENT_BALANCE` | 余额门禁先于上游选择和预期 503 | 临时补测试余额并在 trap 恢复；只有请求到达 503 且真实 Trace 落库才算链路通过 |
| Colima 端口从宿主访问不通 | 某些端口只绑 `127.0.0.1`（VM 内） | sub2api 用 `--network host` 绕过；Langfuse web 暴露 `0.0.0.0:3000` |
| `pricing_service` 报 TLS handshake timeout | GitHub raw 偶发不通 | 非致命，pricing 服务降级；等 retry 或离线跑 |
| SSE 客户端有内容但没有 `[DONE]` | fixture 缺 `message_stop` 或协议转换未识别终态 | fixture 必须发送完整 `message_start → content_block_start/delta/stop → message_delta → message_stop`；脚本同时断言客户端终帧和 ClickHouse `modeltrace.stream.status=completed` |
| root metadata 中找不到 `modeltrace.stream.status` | Langfuse 把非标准 OTEL 属性收进 `metadata['attributes']` JSON | 用 `JSONExtractString(metadata['attributes'], 'modeltrace.stream.status')` 查询，不要直接访问 `metadata['modeltrace.stream.status']` |
| batch submit 500 且 Postgres 报 `trace_continuation does not exist` | 当前 AUTO_SETUP 基线未创建 Ent 已要求的异步续接列 | harness 在 health 后执行幂等 `ALTER TABLE ... ADD COLUMN IF NOT EXISTS trace_continuation jsonb` 并查询 `information_schema` 断言存在；不改生产代码 |
| batch 客户端状态完成但 Langfuse 无异步 item | worker 尚未轮询或 OTEL 尚未 flush | 先断言 fixture 的 upload/create/get/download 计数与 batch `completed`，再等 flush；按 `metadata['task_id']` 找 Trace，检查两个 `model.async.execution` Generation 的 parent 和 continuation 属性 |
| batch 输出含 Base64 fixture 图片 | 这是媒体泄露门禁的故意 canary，不应进入 Trace | 同时对原文和 Base64 执行 ClickHouse 零命中断言；只允许对象存储结果存在，日志与最终报告不得输出完整 canary |

## 与 sub2api-admin skill 的关系

- `sub2api-admin`：用 CLI 管理 sub2api admin API（账号、分组、兑换码等），面向**生产运维**。
- `sub2api-model-trace-e2e`（本 skill）：本地**测试**模型追踪链路，部署 Langfuse + sub2api 临时环境，面向**开发验证**。

两者不重叠：本 skill 不调用 sub2api-admin CLI，直接用 curl + docker exec 完成所有操作。