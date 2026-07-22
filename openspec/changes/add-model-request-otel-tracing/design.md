# Design

## 上下文

### 当前实现

sub2api 后端使用 Go、Gin 和 Wire。模型入口集中注册在 `backend/internal/server/routes/gateway.go`，但请求处理跨 `handler`、`service`、协议转换器、账号调度、计费和多个上游 HTTP/WebSocket 实现：

- `ClientRequestID`、`OpsErrorLoggerMiddleware`、入口标准化和 API Key 鉴权按路由中间件顺序执行。
- `backend/internal/server/middleware/logger.go` 只生成 HTTP access log，不记录 Prompt/Response。
- `backend/internal/handler/ops_error_logger.go` 只把错误和被重试掩盖的上游失败写入 Ops Error Log。
- `backend/internal/service/usage_log.go` 在请求完成后保存 Token、成本、模型和账号等成功用量事实，但不保存完整输入输出。
- `backend/internal/securityaudit/` 能抽取输入 Prompt，但用途是内容审计，不记录模型输出，也不产生 OTEL Trace。
- Chat Completions、Responses、Messages 和 Gemini 之间存在双向协议转换；一次客户端请求可能包含多次账号尝试和故障转移。
- 异步图片任务会在 HTTP 接受响应后使用脱离请求生命周期的上下文继续执行；批量任务可能为一个提交生成多个模型调用。

`backend/go.mod` 当前只有由其他依赖带入的 OTel API/SDK 间接依赖，没有 `TracerProvider`、OTLP exporter 或统一 instrumentation 初始化。现有 OTel exporter 版本与 SDK 间接版本也不应被视为可直接使用的稳定依赖集合。

### Langfuse 契约

Langfuse 自部署版 `>= v3.22.0` 可在 `/api/public/otel` 接收 OTLP/HTTP Trace。Langfuse 将 `langfuse.user.id`、`langfuse.session.id`、`langfuse.observation.input/output`、`gen_ai.request/response.model`、`gen_ai.usage.*` 和成本属性映射为 Trace、Session 和模型观察。需要筛选的 Trace 属性必须存在于相关 Span 上；不能只写在根 Span 后假设自动继承。

本设计使用官方 OpenTelemetry Go SDK 直接导出，不引入 Langfuse Cloud，也不要求额外部署 Collector。OTEL 是应用生成和传输 Trace 的标准；Langfuse 是接收、存储和展示这些 Trace 的后端。

## 目标/非目标

### 目标

- 对应 `model-request-tracing` 的全部 Requirement，为每个 HTTP 模型请求或 WebSocket `response.create` 回合建立一个根 Trace，并把协议阶段、上游尝试、输入输出、用量成本和终态组织成可查询层级。
- 在开启状态下覆盖同步、SSE、Responses WebSocket、异步图片、批量图片和视频生成，不把模型列表、任务轮询等控制面请求误标成模型执行。
- 在有限内存中记录客户端与上游两个视角；流式链路只复制配置上限内的字节，不在热路径构造无界副本。
- 对应 `langfuse-otel-export` 的全部 Requirement，将 OTLP/HTTP Trace 发送到一个管理员指定的自部署 Langfuse 项目。
- 支持部署默认配置与加密持久化的运行时配置，运行时配置原子覆盖且无需重启。
- 所有 Trace 构造和导出失败均 fail-open；模型请求不得同步等待 Langfuse。
- 通过 allowlist 和 canary 测试保证系统认证凭据不进入 Trace、日志、管理 API 或前端状态。

### 非目标

- 不采集普通业务 HTTP、管理 API、健康检查、模型列表、用量查询、任务轮询、结果下载或取消请求。
- 不新增 OTEL Metrics/Logs pipeline，不替换现有 Access Log、Usage Log、Ops Error Log 或 Prompt Audit。
- 不实现持久导出队列、故障恢复补送、跨进程 exactly-once 或 Trace 历史回填。
- 不实现多个 Langfuse 项目、租户路由、按用户/分组选择目标或向多个后端 fan-out。
- 不提供连接测试、运行状态、最后成功时间、发送/丢失量管理 API 或管理页面。
- 不负责 Langfuse 内部保留期限、备份、访问控制和数据删除。
- 不根据 Prompt 内容或 user ID 自动推断 Session。
- 不在本 change 中重构现有协议转换、账号调度、计费或错误日志架构。

## 决策

### 1. 使用独立 `internal/modeltrace` 垂直模块

新增 `backend/internal/modeltrace/`，建议职责如下：

```text
backend/internal/modeltrace/
├── config.go            # 部署/存储/公开/有效配置与校验
├── config_manager.go    # 运行时快照、持久化、优先级和热切换
├── exporter.go          # OTLP/HTTP exporter 与 TracerProvider generation
├── middleware.go        # 根 Trace、客户端响应捕获、终态
├── recorder.go          # 阶段、尝试、Usage、身份和 Session API
├── capture.go           # 有界文本/JSON/媒体捕获
├── attributes.go        # Langfuse/GenAI 属性 allowlist
├── admin_handler.go     # GET/PUT config
├── module.go            # 生命周期与 Wire provider
└── *_test.go
```

模块对现有网关只暴露小接口：

```go
type Manager interface {
    Acquire() (*Snapshot, func())
    PublicConfig(context.Context) (PublicConfig, error)
    Update(context.Context, UpdateRequest, Actor) (PublicConfig, error)
    Shutdown(context.Context) error
}

type Recorder interface {
    EnrichIdentity(Identity)
    RecordClientInput(Content)
    BeginStage(name string) Stage
    BeginAttempt(AttemptMetadata, Content) Attempt
    RecordUsage(Usage)
}
```

禁用或无有效配置时返回零分配 no-op Recorder，现有 Handler 不需要分支复制。

**备选：把实现加入 `server/middleware/logger.go`。** 放弃。全局 Access Log 不知道协议转换后的请求、每次上游尝试、Usage 和异步任务生命周期，也会误覆盖管理接口。

**备选：扩展 `securityaudit`。** 放弃。Prompt Audit 的风险分类、阻断和持久事件与模型可观测性语义不同，合并会让内容审计承担输出捕获和外部 Trace 生命周期。

### 2. 候选模型路由先标记，真实身份建立后才创建根 Trace

模型执行路由先挂载不创建 Span 的 `modeltrace.CandidateMiddleware`，并维持现有 request ID、BodyLimit、Ops Error、endpoint 标准化和 API Key 鉴权职责。逻辑顺序为：

```text
ClientRequestID → ModelTraceCandidate(no span) → RequestBodyLimit → OpsErrorLogger
→ EndpointNorm → APIKeyAuth(identity gate) → GroupGuard → Handler
```

Candidate 只为显式模型执行路由安装惰性 starter/finalizer，不获取 exporter generation、不包装响应、不分配 Prompt/Response buffer。缺失凭据、未知 API Key 和 `invalid_auth_rate_limited` 在身份建立前结束，不调用 starter，因此不会产生模型 Trace 或 OTLP 工作，继续由现有 ingress telemetry 覆盖。

`APIKeyAuth` 成功查到真实 API Key 并设置 fallback 身份后，通过 Gin Context 中的通用 hook 启动根 Trace，再执行 Key/用户/分组状态、IP 限制和后续鉴权检查；hook 避免鉴权包直接依赖 OTel 实现。这样已识别但随后被拒绝的请求仍有身份化失败 Trace。`RequestBodyLimit` 仍先安装有界 reader，但业务读取发生在身份建立和 Trace 启动之后，因此请求体超限、协议解析和参数校验失败可以进入 Trace，匿名请求体不会被 Trace capture。

惰性 starter 负责获取不可变配置 generation、创建根 Trace、把 Recorder 放入 `context.Context`/Gin Context并包装客户端 ResponseWriter；Candidate 的 finalizer 仅在 starter 实际运行后设置终态和结束 Trace。身份、协议、模型、账号、计费和上游内容只能由拥有真实事实的现有 Handler/Service 调用 Recorder 补全，不得从日志文本反向解析或在 middleware 猜测。

模型路由使用显式矩阵维护，不按 `/v1/*` 粗粒度匹配。矩阵包含实际推理/生成/编辑/Embedding/Search、异步和批量提交以及 Responses WebSocket；明确排除 `/models`、`/usage`、`count_tokens` 中纯本地路径、任务 GET、下载和取消。若 `count_tokens` 实际向上游发起模型调用，则只在真实身份建立且即将发生该上游尝试时启动 Trace；纯本地估算不建模型 Trace。路由契约测试必须在新增模型执行入口却未标注时失败。

Responses WebSocket 是惰性启动的特例：HTTP 握手和连接生命周期不创建模型 Trace，Candidate 只向 Handler 提供 turn recorder factory。每个语法有效且类型为 `response.create` 的帧在语义校验、安全审计和账号选择前调用 `StartTurn`，独立获取当时的 config generation、生成唯一 turn request ID，并在该回合 `AfterTurn` 或错误/关闭路径结束。无效 JSON、非 `response.create` 帧、握手失败和连接空闲不创建 Trace。每个回合写入共同的 `connection_request_id` 与单调 `turn_index` 作为 metadata，但不得把连接 ID 自动当成 `langfuse.session.id`；只有客户端显式会话标识可以归入 Session。运行时配置在两个回合之间变化时，后续回合使用新快照，既有回合不迁移。

**备选：在模型路由入口立即创建根 Trace。** 放弃。匿名/无效凭据流量可绕过用户配额持续占用 exporter 队列和 Langfuse 存储，即使现有无效鉴权限流已经返回 429，仍会为每个请求产生 OTLP 工作。

**备选：只在成功写 Usage Log 时创建 Trace。** 放弃。它无法覆盖已识别身份后的鉴权、校验、路由和上游失败，也丢失实时阶段时序。

### 3. 一个根 Trace，下挂阶段 Span 与 Generation Span

Trace 结构固定为：

```text
model.request                         # 根：客户端视角
├── auth / route / account.select     # 内部阶段
├── upstream.attempt.1                # Generation：实际转换后输入/上游输出
├── upstream.attempt.2                # 重试或故障转移 Generation
├── billing / usage                   # 计费与最终用量事实
└── client.response                   # 客户端转换后输出/终态
```

每次实际向模型供应商发送请求前创建一个独立 Generation Span；账号、供应商、代理、入口模型、上游模型和 endpoint 在当次尝试固定。失败尝试正常结束并标 error，后续尝试作为同级子 Span。根 Trace 的最终状态只反映客户端最终结果，因此“首次失败、故障转移成功”会表现为成功根 Trace 下包含失败尝试。

协议转换阶段使用内部 Span，并把客户端原始输入和当次上游输入分别写入根/Generation 的 `langfuse.observation.input`。上游原始输出写入 Generation output，最终客户端输出写入根或 `client.response` output。

Trace 级身份元数据由模块内部的 `TraceMetadataSpanProcessor` 复制到该 Trace 的每个相关 Span，而不是写入 OTel Baggage。这样满足 Langfuse 聚合要求，同时避免把 user/group 元数据作为 `baggage` Header 传播到第三方上游。

### 4. 异步与批量任务持久化 SpanContext 和非秘密 generation fingerprint

异步图片提交在返回 202 前创建并结束提交根 Trace。任务元数据保存 W3C SpanContext、task/item ID，以及提交 generation 的单向 fingerprint；fingerprint 覆盖规范化目标、public/secret 凭据、capture policy、配置来源和版本，但只持久化固定长度摘要，绝不持久化原字段、Prompt、Response 或 Langfuse secret。批量任务的每个 item 保存同一 submission 关联和稳定 item ID。

后台执行开始时获取当前 generation，并以常量时间比较 fingerprint：完全匹配且追踪启用时，使用保存的远端父 SpanContext 在原 Trace 下创建 Generation/attempt；不匹配但当前 generation 有效且启用时，必须创建新的根 Trace，并用 OTel Link 及 `submission_trace_id`、task/item ID metadata 关联，绝不能把原 Trace ID 的 Span 发往新目标；当前 generation 关闭或无效时使用 no-op，业务任务继续。任务轮询、列表、下载和取消自身不创建模型 Trace，任务取消只结束当时实际存在的后台 Span并标 cancelled。

fingerprint 只用于等值判断，不用于重建 exporter 或认证。实现不得把旧 exporter 快照或加密 Langfuse secret 复制到任务记录中。这样同进程、跨实例和重启后的行为一致：只有当前 generation 与提交 generation 完全相同时续接原 Trace，否则安全降级为独立关联 Trace或不追踪。

**备选：让 HTTP 根 Span 一直保持打开直到异步任务完成。** 放弃。进程重启会遗留 Span，长时间打开的根 Span也会延迟 Langfuse 展示。

**备选：把旧 exporter 配置随任务持久化。** 放弃。它会复制 Langfuse secret、扩大轮换和清理边界，并使后台任务绕过管理员当前关闭配置。

### 5. 使用有界 capture，不复制无界流

`capture.Buffer` 维护：

- `originalBytes`：观察到的总字节数；
- `storedBytes`：实际复制字节数；
- `truncated`：是否超过上限；
- `contentType` 和媒体元数据；
- 上限内的预分配字节缓冲。

客户端响应通过 Gin `ResponseWriter` 包装器在真实写出后复制最多 N 字节；上游流通过 `io.ReadCloser` 包装器在业务读取时复制最多 N 字节。捕获失败只关闭该 observation 的内容记录，不影响原读写返回值。流式请求不拼接第二份无限增长的完整字符串。

Handler 已经读取的请求体直接传给 Recorder；模块不得再次读取 `http.Request.Body`。转换后的上游请求在发送前交给 Attempt。非流式上游响应在现有读取边界内交给 Attempt；SSE/WebSocket 在解析事件的同时增量追加。

默认值在实现前按“每个 observation 建议不超过自部署 Langfuse 实际接收限制”门禁确定。初始候选为 Prompt 1 MiB、Response 1 MiB、媒体 1 MiB；管理员可配置正数上限，但不得超过 Gateway 已有请求/响应上限。最终默认值必须通过目标 Langfuse 版本的真实 OTLP smoke test 后固定，不能只依赖文档推断。

### 6. 多模态默认元数据，显式 opt-in 才写原始内容

媒体内容识别覆盖 data URL、Base64 content block、multipart 文件、远程 URL和二进制响应。默认规范化为：媒体类型、数量、原始大小、URL/对象 ID、Hash（若已有字节）和是否省略。不得为了计算 Hash 主动下载远程媒体。

`capture_media_content=true` 时，模块把上限内内容编码成 Langfuse 支持的多模态 JSON 表示；超过上限仍只保存有界前缀/元数据并标记 truncated。该开关不允许绕过认证凭据过滤。

### 7. 认证信息采用 allowlist，而不是通用 Header 序列化后删除

Trace 只从明确的结构字段构造属性。允许身份字段包括 user ID、API Key ID/名称、group ID/名称、account ID/名称、provider、model、request ID、endpoint、protocol 和显式 session ID。禁止把整个 `http.Request`、Header map、Cookie jar、account credential 或代理 URL query 序列化进 Trace。

Prompt/Response 正文按已批准需求不做内容脱敏；因此用户主动写入正文的密钥样字符串会被记录。系统凭据则在进入 capture 前永不加入候选数据。测试使用不同 canary 验证 Authorization、Cookie、用户 API Key、上游 token、OAuth token 和 Langfuse secret 均不出现。

### 8. 配置使用部署默认 + settings 整体运行时快照

部署配置新增 `model_tracing` 节：

```yaml
model_tracing:
  enabled: false
  endpoint: ""
  public_key: ""
  secret_key: ""
  prompt_max_bytes: 1048576
  response_max_bytes: 1048576
  media_max_bytes: 1048576
  capture_media_content: false
```

字段通过 Viper 支持同构环境变量。不得提供 cloud.langfuse.com 默认值。

endpoint 校验在部署配置和运行时配置中共用同一实现：`https://` 目标使用 Go 标准 TLS 服务端证书校验；`http://` 只允许 host 为 `localhost` 或 `net.ParseIP(host).IsLoopback()` 的字面量回环地址。不得提供 `InsecureSkipVerify`、自定义跳过证书校验或任意远端明文放行开关。非回环 HTTP 部署配置视为无效；非回环 HTTP 运行时更新整体拒绝并保留旧 generation。

运行时配置存入现有 `settings` 的单一 JSON key `model_trace_config`，结构含 `configured`、`enabled`、endpoint、public key、加密 secret、三个上限、media 开关、`config_version`、updated_at/updated_by。整个 JSON 是一个版本化快照，不做逐字段来源混合：

1. 存在且可解析/解密的运行时快照时整体生效，包括显式 disabled；
2. 没有运行时快照或快照损坏/无法解密时整体回退部署配置；
3. 运行时 endpoint 暂时不可达不触发部署目标回退，避免 Trace 静默发送到另一个项目；
4. 两者都无效时使用 disabled no-op。

管理员 API 为 `GET/PUT /api/v1/admin/model-tracing/config`，复用管理员鉴权、管理操作审计和 `expected_config_version` CAS。读取只返回 `has_secret`；更新支持保留、替换、显式清除 secret。持久化 secret 复用现有 `service.SecretEncryptor`；部署 secret 只保留在进程配置内。若部署使用的是不具备重启稳定性的自动生成加密密钥，运行时 API必须拒绝首次持久化新 secret，避免重启后无法解密。

前端在管理员设置中增加“模型追踪”配置区，只提供上述字段、保存和重置；不提供 probe、runtime、last error、发送量或丢失量。

### 9. 配置热切换使用不可变 generation 与引用计数

`ConfigManager` 的原子指针指向不可变 `Generation`：有效公开配置、capture policy、TracerProvider/exporter、版本和单向 fingerprint。同步请求或 WebSocket 回合开始时 `Acquire()` 增加 generation 引用并在结束时释放；异步后台任务按第 4 节比较当前 fingerprint 后决定续接、独立关联或 no-op。因此单个正在执行的 Trace 使用同一目标和上限，且不会因跨进程任务把原 Trace ID 发送到新目标。

成功保存新配置后先完整构造新 generation，再原子替换。旧 generation 在引用归零后异步执行有界 `ForceFlush/Shutdown`；超时直接放弃，不能阻塞请求或管理保存。无效更新在构造阶段失败，不替换当前 generation。多实例通过与现有 Prompt Audit 相同的 settings 版本轮询/失效通知模式刷新；每个实例最终切到同一 config version。

**备选：更新时修改全局 OTel Provider。** 放弃。全局 provider 会影响依赖库的其他 instrumentation，且进行中 Trace 可能被拆到新目标。

### 10. 使用独立 OTel SDK Provider 和非阻塞 OTLP/HTTP Batch exporter

模块显式声明并对齐官方 OpenTelemetry Go API、SDK、OTLP/HTTP exporter 与语义约定依赖版本，不依赖当前间接版本偶然组合。每个 generation 使用独立 `sdktrace.TracerProvider`，不调用 `otel.SetTracerProvider` 覆盖进程全局。

Exporter 配置：

- OTLP/HTTP protobuf；目标为管理员 endpoint 下的 `/api/public/otel/v1/traces` 或等价 trace endpoint；非回环目标必须使用 HTTPS，仅 `localhost` 或字面量回环 IP 允许 HTTP；
- HTTPS 使用 Go 标准服务端证书校验且不允许跳过；Basic Auth 值由 Langfuse public/secret key 构造；加入 `x-langfuse-ingestion-version: 4`；
- `BatchSpanProcessor` 使用有界队列和批次，不开启“队列满时阻塞请求”；
- export timeout、queue size、batch size 和 flush timeout 使用代码内保守默认值，MVP 不暴露运行时调优 UI；
- exporter 错误通过限频结构化日志记录，只含目标 host、配置版本和稳定错误码，不含 path query、Authorization 或 payload；
- 应用关闭时使用服务器现有 5 秒 shutdown context 做 best-effort flush。

应用主动向 Langfuse 发起出站 HTTP；本模块不监听新的 OTEL 端口。用户所说的 IP/端口是自部署 Langfuse 的接收地址。

**备选：部署 OpenTelemetry Collector。** MVP 放弃。Collector 能做多后端路由和持久队列，但增加独立部署、配置和监控面，且用户已选择单项目、允许丢失的 fail-open MVP。

**备选：直接写 Langfuse 私有 ingestion API。** 放弃。OTLP 是已确认契约，也避免后端绑定 Langfuse 私有事件格式。

### 11. Langfuse 属性使用 GenAI 语义约定加显式 Langfuse 映射

根 Trace 设置：

- `langfuse.trace.name=model.request`；
- `langfuse.user.id`、`langfuse.session.id`（仅显式 session）；
- `langfuse.trace.metadata.request_id/api_key_id/group_id/...`；
- `langfuse.trace.tags`：入口协议、供应商、最终状态；
- 根 `langfuse.observation.input/output`：客户端视角。

每个 Generation Span 设置：

- `langfuse.observation.type=generation`；
- `langfuse.observation.input/output`：该次上游实际内容；
- `gen_ai.operation.name`、`gen_ai.provider.name`、`gen_ai.request.model`、`gen_ai.response.model`；
- `langfuse.observation.usage_details` 与对应 `gen_ai.usage.*`；
- `langfuse.observation.cost_details` 与已知成本；
- `langfuse.observation.completion_start_time`：首 Token/首输出时间；
- attempt index、account、endpoint、status/error 作为 observation metadata。

Usage 只从上游解析和最终 Usage Log 的同一事实对象映射；未知值不写属性，不用 0 代替。金额保持现有计费精度，在序列化前不使用低精度 float 做二次计算。

### 12. Session 只消费已有显式标识

新增独立的 Langfuse Session extractor，不复用 `OpenAIGatewayService.ExtractSessionID`，因为后者会把 `prompt_cache_key` 当作上游缓存/粘性调度信号。Langfuse extractor 仅接受语义明确的 `session_id`、`conversation_id`、仅适用于 Grok 身份的 `x-grok-conv-id`，以及协议结构化 metadata 中明确命名且可正确解析的 `session_id`（例如已解析的 `metadata.user_id.session_id`）。明确排除 `prompt_cache_key`、`GenerateSessionHash`、粘性路由 hash、内容 fallback 和为上游伪装而生成的 session 值，避免缓存或调度实现把无关请求误归组。

原始 session ID 不是认证凭据，但可能包含用户信息；按用户已确认的“不脱敏”边界发送到自部署 Langfuse。

### 13. fail-open 在每个边界独立成立

所有 Recorder 方法不得向业务调用方返回会改变流程的错误。内部错误处理：

- 配置无效：no-op 或保留上一 generation；
- capture 分配/编码失败：丢弃当前内容属性，保留其余 Span；
- exporter 队列满：丢弃 Span；
- 网络/认证/Langfuse 5xx：Batch exporter 超时后丢弃；
- panic：Recorder 边界 recover 并结束/丢弃 Trace；
- shutdown flush 超时：记录限频错误并退出。

模型请求不能同步调用 exporter，也不能因 flush 等待。对照测试使用故意阻塞和 panic 的 fake exporter，比较追踪关闭与异常开启时的状态码、响应字节、流式帧序列、上游次数和 Usage Log。

### 14. 测试按可观察垂直路径组织

- `internal/modeltrace`：配置优先级、secret 三态、generation 一致性、有界 capture、属性映射、fail-open 和 OTLP fake server。
- `internal/server/routes`：模型执行路由矩阵；认证/BodyLimit 前也创建 Trace，控制面路由不创建。
- `internal/handler`/`internal/service`：Chat/Responses/Messages/Gemini/Embedding/媒体、协议转换、重试、Usage、SSE、WS 和异步上下文传播。
- 前端：管理员配置读取、保存、secret 保留/替换/清除、默认/运行时来源展示；断言没有 probe/runtime UI。
- 端到端：使用 `httptest.Server` 模拟自部署 Langfuse OTLP endpoint，解码 OTLP protobuf 并验证 Trace ID、层级、属性、内容截断和 canary secret 排除。

## 风险/取舍

- **原始 Prompt/Response 含敏感业务数据** → 仅发送到管理员配置的自部署目标；系统凭据使用严格 allowlist 排除；管理界面明确提示自部署存储责任。按用户决策不对正文做自动脱敏。
- **未脱敏内容和 Langfuse 项目凭据经明文网络泄露** → 非回环目标强制 HTTPS 并执行标准证书校验；HTTP 仅允许 `localhost` 或字面量回环 IP，且不提供跳过证书校验或远端明文放行开关。
- **OTLP/Langfuse 对大属性或请求有大小限制** → 每个 observation 有独立可配置上限，Batch 使用小批次；实现前必须在实际 Langfuse 版本验证默认值。
- **重试和批量任务放大 Trace 大小** → 每次 attempt/item 独立受限，capture 只保留有界内容；不为了“完整 Trace”构造无界聚合 JSON。
- **运行时目标不可达时 Trace 静默丢失** → 这是已批准 MVP 取舍；仅输出限频安全日志，不新增状态面板或可靠队列。
- **运行时配置可能把数据发送到错误内网目标** → 管理 API 仅管理员可用并写管理操作审计；读取展示有效 host/source；MVP 不做连接 probe。目标归属由管理员负责。
- **全量追踪增加 CPU、内存和网络** → 禁用路径 no-op；启用路径有界预分配、增量复制和异步 Batch；上线前进行代表性流式与长上下文 benchmark。当前没有获批数值 SLO，结果必须作为灰度门禁而非虚构通过。
- **异步任务跨进程或跨配置 generation** → 任务只持久化 SpanContext、task/item ID 和非秘密 fingerprint；仅 fingerprint 匹配时续接，错配时使用当前配置创建独立关联 Trace 或 no-op，禁止同一 Trace ID 跨目标。
- **OTel GenAI 语义约定仍在演进** → 同时写 Langfuse 明确支持的 `langfuse.*` 属性与版本化 `gen_ai.*` 属性；属性集中在一个映射文件并用 OTLP 合约测试固定。
- **多实例运行时配置传播存在窗口** → 保存 API 返回的实例立即切换，其他实例按失效通知/有界刷新收敛；config version 写入 Trace 便于识别窗口。

## 迁移与回滚

1. 注册并对齐 OTel 直接依赖，新增 modeltrace 模块、配置结构和测试；功能保持默认关闭。
2. 新增 settings key 和管理员 GET/PUT API；不需要数据库 schema migration，现有 settings 表可直接保存新 key。
3. 接入一个代表性 Chat Completions 垂直切片并使用本地 fake OTLP endpoint 验证完整链路。
4. 逐协议扩展到 Responses、Messages、Gemini、Embedding/Search、SSE/WS 和媒体；每次以路由矩阵与 no-side-effect 测试守住覆盖。
5. 增加管理员前端配置区和 i18n；仍不提供 probe/runtime 状态。
6. 在本机通过官方自部署方式启动隔离的 Langfuse `>= v3.22.0` 测试实例，仅绑定回环地址；记录实际版本、OTLP path、项目 public/secret key、payload 限制和容器资源状态，不使用生产 Langfuse 或生产凭据。
7. 使用该本地实例执行真实 OTLP smoke test，核对代表性成功、失败、重试、流式、截断 Trace，并据实固定 Prompt/Response/媒体默认上限；随后再在生产保持 disabled 的状态下完成部署配置检查。
8. 回滚优先保存 `enabled=false` 运行时配置；若管理配置不可用则移除运行时配置并关闭部署开关。代码回滚不需要数据迁移，Langfuse 中已接收 Trace 保留并由其自身策略管理。

回滚触发条件：模型响应字节或状态发生差异、流式延迟出现不可接受回归、认证凭据泄露、内存无界增长、OTLP 故障传播到业务请求。凭据泄露时除立即关闭外，必须轮换受影响凭据并按 Langfuse 自部署流程清理数据。

## 未决问题

1. **默认内容上限**：负责人为实现者；在首个 OTLP 合约切片前，用目标自部署 Langfuse 版本验证单 Observation 与批量 payload 限制，在不超过 1 MiB 候选值的范围内固定 Prompt/Response/媒体默认上限并回写本 design。该结论不改变“可配置、超限标记”的 specs。
2. **性能灰度阈值**：负责人为项目维护者；实现完成后用现有代表性非流式、SSE 和长上下文 benchmark 记录关闭/开启对比。当前没有用户批准的数值 SLO，不得把未测量值写成达标；任何可观察响应变化或同步等待即阻塞发布。
3. **本地 Langfuse 测试实例信息**：负责人为实现者；按用户决定在本机自部署隔离的 Langfuse `>= v3.22.0`，仅绑定回环地址。首次真实 smoke 前记录实际版本、最终 endpoint、测试项目 public/secret key、反向代理请求上限和容器资源状态；测试凭据不得复用生产凭据。MVP 不通过应用内 probe 解决该问题。
