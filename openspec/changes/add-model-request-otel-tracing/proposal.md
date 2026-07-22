# Proposal

## 为什么

当前项目分别使用 Access Log、Usage Log、Ops Error Log 和 Prompt Audit 记录请求元数据、成功用量、失败信息或输入提示词，但没有一条能把一次模型请求的客户端输入、协议转换、上游尝试、流式输出、用量成本和最终结果关联起来的统一 Trace，也没有把这些数据发送到自部署 Langfuse 的 OTEL 能力。发生协议转换、重试、账号切换或流式中断时，管理员需要跨多套记录人工拼接，且无法在 Langfuse 中直接查看完整 Prompt/Response 和阶段耗时。

本变更为所有实际执行推理或生成的模型请求增加统一、旁路的 Trace 能力，并通过标准 OTLP 发送到管理员指定的自部署 Langfuse。该能力服务于故障与性能排查、用户用量分析以及 Prompt/Response 复核；观测链路故障不得改变模型请求结果。

## 变更内容

- 为所有已识别用户/API Key 的模型推理与生成请求建立“一次 HTTP 模型请求或一次 WebSocket `response.create` 回合对应一个 Trace”的可观察契约，覆盖成功、身份建立后的拒绝、流式、非流式、多模态和客户端取消；缺失或未知凭据的匿名失败继续使用现有 ingress telemetry，不创建模型 Trace。
- 在同一 Trace 中区分客户端原始输入/输出、转换后上游输入/输出，以及每次重试、账号切换和故障转移尝试。
- 记录 request ID、用户、API Key 标识、分组、模型、供应商、账号、阶段耗时、错误、Token 和成本；显式会话标识存在时将多个 Trace 归入同一 Langfuse Session。
- 默认记录未脱敏的文本和结构化 Prompt/Response；认证凭据永不进入 Trace。内容支持有界截断，多模态默认只记录元数据并可显式开启原始媒体记录。
- 支持部署配置与管理员运行时配置同一个自部署 Langfuse 目标，运行时配置优先，部署配置作为默认与回退；配置读取不得回显秘密凭据。
- 通过 OTLP 将 Trace 发送到自部署 Langfuse；非回环目标必须使用 HTTPS，仅回环目标允许 HTTP。Langfuse、网络或配置异常时模型请求继续执行，MVP 不保证补送，也不提供模块运行状态、连接测试或发送/丢失量面板。
- 新能力默认关闭，不改变现有模型 API 的成功响应、错误响应、流式帧或计费语义；没有外部 API breaking change。

## 能力

### 新增能力

- `model-request-tracing`：定义模型请求 Trace 的覆盖范围、生命周期、客户端与上游观察、重试层级、内容边界、身份/会话关联、用量成本和异常语义。
- `langfuse-otel-export`：定义自部署 Langfuse OTLP 目标、部署与运行时配置优先级、秘密保护、旁路失败行为和管理配置契约。

### 修改能力

无。仓库当前没有已归档的 OpenSpec capability；现有 Access Log、Usage Log、Ops Error Log、Prompt Audit 和模型 API 行为作为兼容基线，不改变其既有语义。

## 影响范围

- **管理员与运维人员**：可以在一个自部署 Langfuse 项目中按 request ID、用户、API Key、分组、模型和 Session 查看完整模型调用链。
- **模型网关**：所有已识别用户/API Key 的实际推理/生成入口均纳入 Trace；缺失或未知凭据的匿名失败沿用现有 ingress telemetry，模型列表、用量查询、健康检查、管理、登录和支付等非模型调用不在本次范围。
- **管理配置**：新增全局 OTEL/Langfuse 配置读取与更新能力；管理员运行时配置覆盖部署默认值。
- **外部依赖**：依赖自部署 Langfuse 提供兼容的 OTLP/HTTP 接收能力和有效项目凭据；不发送到 Langfuse Cloud。
- **数据与隐私**：Prompt/Response 可包含未脱敏业务内容；认证 Header、Cookie、用户 API Key、上游密钥和 OAuth Token 永不记录。数据保留、备份和 Langfuse 内部访问控制由自部署环境负责。
- **可靠性**：观测链路为 fail-open 旁路；发送失败可能永久丢失 Trace，但不得使模型请求失败或改变客户端可观察结果。
