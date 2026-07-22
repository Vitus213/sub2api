# Spec Delta

## ADDED Requirements

### Requirement: Trace 必须发送到唯一的自部署 Langfuse 项目

系统 SHALL 只把本能力生成的 OTEL Trace 发送到当前有效配置指定的一个自部署 Langfuse 项目，并 MUST 使用该实例支持的 OTLP/HTTP Trace 接收契约。系统 MUST NOT 内置 Langfuse Cloud 默认目标，也 MUST NOT 同时复制到多个 Langfuse 实例或项目。

#### Scenario: 有效的自部署目标可用

- **WHEN** 模型追踪已启用、有效配置指向可达的自部署 Langfuse 且认证有效
- **THEN** Langfuse MUST 接收并展示该请求的 Trace、模型观察、Prompt/Response、用量和成本
- **THEN** Trace MUST 只出现在当前配置的项目中

#### Scenario: 未配置任何目标

- **WHEN** 部署配置和管理员运行时配置都没有形成有效目标
- **THEN** 模型追踪 MUST 保持关闭
- **THEN** 系统 MUST NOT 自动回退到 Langfuse Cloud 或其他预置地址

### Requirement: 部署与运行时配置必须遵守确定优先级

系统 SHALL 同时支持部署配置和管理员运行时配置。存在语法、凭据和边界均有效的运行时配置时，运行时配置 MUST 整体优先；不存在可用运行时配置时，系统 MUST 使用有效部署配置；两者都不可用时 MUST 关闭导出。运行时配置中的显式 `enabled=false` MUST 作为有效覆盖关闭导出，不得被部署配置重新开启。

#### Scenario: 只有部署配置

- **WHEN** 部署配置有效且管理员从未保存运行时配置
- **THEN** 系统 MUST 使用部署配置作为当前有效配置
- **THEN** 管理读取结果 MUST 表明有效来源为部署配置

#### Scenario: 运行时配置覆盖部署配置

- **WHEN** 部署配置和管理员运行时配置都有效且运行时配置已启用
- **THEN** 新开始的模型请求 MUST 只使用运行时配置指定的目标和内容边界
- **THEN** 系统 MUST NOT 把同一 Trace 同时发送到部署目标

#### Scenario: 管理员运行时关闭

- **WHEN** 部署配置为启用但管理员保存有效的 `enabled=false` 运行时配置
- **THEN** 后续新请求 MUST 停止生成和发送本能力的 Trace
- **THEN** 系统 MUST NOT 回退到部署配置重新开启

#### Scenario: 保存的运行时配置无法解析或解密

- **WHEN** 启动或刷新时发现保存的运行时配置损坏、无法解析或凭据无法解密
- **THEN** 系统 MUST 回退到有效部署配置；没有有效部署配置时关闭导出
- **THEN** 模型请求 MUST 继续执行

### Requirement: 管理员必须能够无重启读取和更新运行时配置

系统 SHALL 向已认证管理员提供全局模型追踪配置的读取和更新能力。运行时配置 MUST 包含启停、自部署 Langfuse 地址、项目公钥、项目秘密状态、Prompt 上限、Response 上限、媒体上限和是否记录原始媒体；成功更新后 MUST 对后续新请求生效，无需重启服务。MVP MUST NOT 提供连接测试、运行状态、最后成功时间、发送量或丢失量接口与页面。

#### Scenario: 管理员读取配置

- **WHEN** 已认证管理员读取模型追踪配置
- **THEN** 响应 MUST 返回运行时配置、有效来源和可公开的部署默认信息
- **THEN** 响应 MUST 只返回项目秘密是否已配置，不得返回秘密明文或密文

#### Scenario: 管理员保存有效配置

- **WHEN** 已认证管理员保存字段完整且边界有效的运行时配置
- **THEN** 系统 MUST 持久化配置并使后续新请求使用该配置
- **THEN** 保存响应 MUST 返回规范化后的公开配置和有效来源

#### Scenario: 非管理员尝试读取或更新

- **WHEN** 未认证用户或非管理员访问模型追踪配置
- **THEN** 系统 MUST 拒绝请求
- **THEN** 响应 MUST NOT 暴露目标、项目标识或秘密状态

#### Scenario: 管理员请求 MVP 不支持的运行状态

- **WHEN** 管理员尝试访问模型追踪连接测试或运行状态能力
- **THEN** 系统 MUST 不声称该能力存在或返回伪造健康状态

### Requirement: 配置更新必须保护秘密并保持原子语义

系统 MUST 加密持久化 Langfuse 项目秘密，并 SHALL 支持“空值保留原秘密、提供新值替换、显式清除”三种更新语义。无效更新 MUST 被整体拒绝并保留之前的运行时配置和当前有效导出器，不得形成目标、凭据和大小上限来自不同版本的混合配置。

#### Scenario: 更新非秘密字段且保留秘密

- **WHEN** 管理员更新地址或内容上限但秘密输入为空且未请求清除
- **THEN** 系统 MUST 保留原有加密秘密
- **THEN** 读取响应仍 MUST 只显示秘密已配置状态

#### Scenario: 替换项目秘密

- **WHEN** 管理员提交新的项目秘密
- **THEN** 后续新请求 MUST 使用新秘密认证
- **THEN** 新旧秘密的明文和密文 MUST NOT 出现在响应、日志或 Trace 中

#### Scenario: 无效更新

- **WHEN** 管理员提交无效 URL、不支持的协议、缺失必需凭据、非正数大小上限或其他非法组合
- **THEN** 系统 MUST 拒绝整个更新并返回可判定错误
- **THEN** 之前的有效配置 MUST 继续生效

### Requirement: OTLP 传输必须保护内容与项目凭据

系统 MUST 对所有非回环 Langfuse endpoint 要求 HTTPS，并 MUST 使用正常的服务端证书校验。系统 MAY 仅对 host 为 `localhost` 或字面量回环 IP 的 endpoint 允许 HTTP。部署配置与管理员运行时配置 MUST 使用相同校验规则，且系统 MUST NOT 提供跳过证书校验的配置。

#### Scenario: 管理员配置非回环 HTTP 目标

- **WHEN** 管理员提交 host 不是 `localhost` 或字面量回环 IP 的 `http://` endpoint
- **THEN** 系统 MUST 拒绝整个更新并返回可判定错误
- **THEN** 之前的有效配置 MUST 继续生效

#### Scenario: 管理员配置 HTTPS 或回环 HTTP 目标

- **WHEN** 管理员提交语法有效且凭据、边界完整的 `https://` endpoint，或 host 为 `localhost` 或字面量回环 IP 的 `http://` endpoint
- **THEN** 系统 MUST 接受该传输协议组合
- **THEN** HTTPS 连接 MUST 执行正常的服务端证书校验

#### Scenario: 部署配置使用不安全的远端 HTTP 目标

- **WHEN** 部署配置启用追踪但 endpoint 是非回环 `http://` 地址
- **THEN** 该部署配置 MUST 被视为无效
- **THEN** 系统 MUST NOT 向该目标发送 Prompt/Response 或 Langfuse 项目凭据

### Requirement: 配置切换必须为请求提供一致快照

每个模型请求 SHALL 在开始时绑定一个有效配置快照。管理员更新配置后，新请求 MUST 使用新快照；已经开始的请求 MUST 使用其原快照直至 Trace 结束，避免同一 Trace 被拆分到两个项目或使用两套内容边界。

#### Scenario: 请求进行中切换目标

- **WHEN** 请求已经开始且管理员随后保存新的 Langfuse 目标
- **THEN** 进行中的 Trace MUST 继续使用开始时的目标和限制
- **THEN** 更新后开始的新请求 MUST 使用新目标和限制

#### Scenario: 请求进行中关闭追踪

- **WHEN** 一个已追踪请求进行中且管理员保存 `enabled=false`
- **THEN** 该请求 MUST 按开始时快照完成 Trace
- **THEN** 后续新请求 MUST 不再生成 Trace

#### Scenario: WebSocket 连接的两个回合之间切换配置

- **WHEN** 一个 WebSocket 回合已经结束、管理员随后更新或关闭追踪配置、同一连接再提交新的 `response.create` 回合
- **THEN** 已结束回合 MUST 保持原配置快照
- **THEN** 新回合 MUST 在开始时重新获取当前配置快照，并使用新目标/限制或在已关闭时不生成 Trace

#### Scenario: 异步任务跨越目标配置切换

- **WHEN** 异步提交 Trace 已发送到旧目标，而后台任务开始时当前配置指向不同目标
- **THEN** 系统 MUST NOT 把原 Trace ID 的后续 Span 发送到新目标
- **THEN** 后台追踪如启用，MUST 使用新 Trace 并携带不含秘密的提交 Trace/task 关联信息

### Requirement: 观测故障必须 fail-open 且 MVP 不保证补送

Langfuse 拒绝、超时、网络中断、导出队列饱和、序列化错误、配置错误或内部 Trace 错误 MUST NOT 阻止、取消、延迟等待或改变模型请求的客户端状态码、响应体、流式帧、计费和上游重试决策。MVP MAY 丢弃无法发送的数据，并 MUST NOT 承诺在恢复后补送。

#### Scenario: Langfuse 在非流式请求期间不可用

- **WHEN** Langfuse 连接失败或拒绝 Trace
- **THEN** 模型请求 MUST 按原有业务链路完成
- **THEN** 客户端响应 MUST 与关闭模型追踪时等价
- **THEN** 对应 Trace MAY 永久缺失

#### Scenario: Langfuse 在流式请求期间变慢

- **WHEN** Langfuse 接收端阻塞、超时或处理变慢
- **THEN** 模型流 MUST 不等待 Langfuse 才向客户端发送下一帧
- **THEN** 首 Token 时间和流式传输 MUST 不因同步等待导出而改变

#### Scenario: 导出内部发生异常

- **WHEN** Trace 构造、内容序列化或提交发生内部错误
- **THEN** 系统 MUST 隔离该错误并继续模型请求
- **THEN** 错误 MUST NOT 作为模型 API 错误返回给客户端

### Requirement: Langfuse 中的 Trace 字段必须可筛选并保持标准映射

系统 SHALL 使用 Langfuse 可识别的 OTEL 属性表达 Trace 名称、user ID、Session ID、模型输入输出、模型名称、用量、成本、完成开始时间和请求元数据。需要在 Langfuse 中筛选或聚合的 Trace 级身份属性 MUST 在相关观察中保持一致。

#### Scenario: 管理员按身份筛选

- **WHEN** Langfuse 已接收一个身份信息完整的模型 Trace
- **THEN** 管理员 MUST 能按 user ID、Session ID、request ID、API Key ID、group ID、入口模型或上游模型筛选或定位

#### Scenario: 管理员查看模型观察

- **WHEN** 对应请求具有输入、输出、Usage 和成本
- **THEN** Langfuse MUST 将其展示为该 Trace 下可识别的模型观察
- **THEN** Prompt、Response、模型、Token 和成本 MUST 映射到对应展示字段，而不是只存在于不可解析的日志文本中

## MODIFIED Requirements

无。

## REMOVED Requirements

无。

## RENAMED Requirements

无。
