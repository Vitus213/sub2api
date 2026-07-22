# Spec Delta

## ADDED Requirements

### Requirement: 所有模型推理与生成请求必须具有唯一 Trace

系统 SHALL 在模型追踪启用且有效导出目标可用时，为每个进入受支持推理或生成入口且已识别到真实用户/API Key 的 HTTP 模型请求或 Responses WebSocket `response.create` 回合生成唯一 Trace。覆盖范围 MUST 包含 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages、Gemini 生成、Embeddings、文本搜索、图像/视频生成与编辑、异步或批量媒体生成以及 Responses WebSocket 中实际触发模型执行的轮次，并 MUST 覆盖成功、身份建立后的鉴权或客户端错误、上游错误和内部错误。缺失凭据、未知 API Key 和无效鉴权限流等尚未建立任何用户/API Key 身份的匿名失败 MUST NOT 生成模型 Trace；模型列表、用量查询、任务轮询、结果下载、取消、健康检查、登录、管理和支付等不触发模型执行的请求也 MUST NOT 因本能力生成模型 Trace。

#### Scenario: 成功的同步模型请求

- **WHEN** 客户端向受支持的同步推理或生成入口发送请求且请求成功完成
- **THEN** Langfuse MUST 出现且仅出现一个以该客户端请求为根的 Trace
- **THEN** Trace MUST 能通过服务生成的 request ID 检索

#### Scenario: 已识别身份的请求在鉴权或参数校验阶段失败

- **WHEN** 客户端命中受支持模型入口，系统已从真实 API Key 建立用户/API Key 身份，但请求随后在 Key/用户/分组状态、IP 限制、请求体限制、协议解析或参数校验阶段失败
- **THEN** 系统 MUST 结束该请求对应的 Trace 并记录已知身份、失败阶段、状态和可安全记录的输入
- **THEN** 系统 MUST NOT 为未知字段伪造身份或上游尝试

#### Scenario: 匿名请求在身份建立前失败

- **WHEN** 客户端因缺失凭据、未知 API Key、无效鉴权限流或其他未建立用户/API Key 身份的原因被拒绝
- **THEN** 系统 MUST NOT 为该请求创建模型 Trace 或发送 Prompt/Response 到 Langfuse
- **THEN** 该拒绝 MUST 继续由现有 ingress telemetry 记录

#### Scenario: 非模型控制面请求

- **WHEN** 客户端调用模型列表、用量查询、任务轮询、结果下载、取消或其他不触发模型执行的入口
- **THEN** 系统 MUST NOT 为该请求创建模型 Trace

#### Scenario: 异步或批量模型执行

- **WHEN** 一个已接受的异步或批量请求在返回接受响应后继续执行一个或多个实际模型调用
- **THEN** 后续模型尝试 MUST 与原始提交请求的 Trace 保持可关联
- **THEN** 每个任务或批次项的结果 MUST 能在续接原 Trace 或独立关联 Trace 中区分，并 MUST 保留 task/item ID

#### Scenario: 异步执行时原配置 generation 已不可用

- **WHEN** 后台任务开始执行时，当前有效配置与提交时持久化的非秘密 generation fingerprint 不一致，或原配置已关闭/不可恢复
- **THEN** 系统 MUST NOT 使用原 Trace ID 把后续 Span 发送到不同 Langfuse 目标
- **THEN** 当前配置有效且启用时，系统 MUST 创建独立 Trace，并通过 OTel Link 或 `submission_trace_id`、task/item ID 与提交 Trace 关联
- **THEN** 当前配置已关闭或无效时，系统 MAY 不生成后台 Trace，但业务任务 MUST 继续执行
- **THEN** 系统 MUST NOT 为保证续接而把 Langfuse secret、Prompt 或 Response 持久化到任务元数据

#### Scenario: 一个 WebSocket 连接包含多个模型回合

- **WHEN** 已识别身份的 Responses WebSocket 连接依次提交两个语法有效的 `response.create` 回合
- **THEN** 系统 MUST 为每个回合创建并结束一个独立 Trace，而不是为整个连接创建长生命周期模型 Trace
- **THEN** 两个 Trace MUST 具有不同的 turn request ID，并通过共同的 connection request ID 和 turn index 关联
- **THEN** WebSocket 握手、空闲等待、无效 JSON 或非 `response.create` 帧本身 MUST NOT 创建模型 Trace
- **THEN** connection request ID MUST NOT 在没有显式会话标识时被映射为 Langfuse Session ID

### Requirement: 一个 Trace 必须保留完整尝试层级

一次 HTTP 模型请求或一次 WebSocket `response.create` 回合 SHALL 对应一个 Trace。鉴权、路由、账号选择、计费检查、每次上游调用、重试、账号切换、故障转移和客户端响应 MUST 在该 Trace 中保持确定的父子关系；每次上游尝试 MUST 独立记录其账号、供应商、目标模型、耗时和结果。

#### Scenario: 首次上游失败后故障转移成功

- **WHEN** 第一次上游尝试失败且系统切换账号或供应商后成功返回
- **THEN** Langfuse MUST 只显示一个根 Trace
- **THEN** Trace MUST 同时保留失败尝试和成功尝试，且顺序、账号、错误和耗时可区分
- **THEN** 根 Trace MUST 以最终客户端结果标记为成功

#### Scenario: 所有上游尝试均失败

- **WHEN** 请求经历多个上游尝试且最终仍失败
- **THEN** Trace MUST 保留全部已发生尝试并以最终错误结束
- **THEN** 最终错误 MUST 能与具体失败阶段和最后一次尝试关联

#### Scenario: 请求未到达上游

- **WHEN** 请求在鉴权、路由、账号选择或计费检查阶段结束且未发送上游请求
- **THEN** Trace MUST 明确显示未发生上游调用
- **THEN** Trace MUST NOT 创建虚构的上游尝试

### Requirement: Trace 必须区分客户端与上游内容视角

系统 SHALL 在同一 Trace 中分别记录客户端原始请求、最终客户端响应、每次尝试实际发送给上游的转换后请求以及上游原始响应。四类内容 MUST 使用可区分的观察名称或属性，不得相互覆盖；文本与结构化 JSON 默认按原文记录且不执行内容脱敏。

#### Scenario: 请求和响应发生协议转换

- **WHEN** 客户端协议与实际上游协议不同
- **THEN** Trace MUST 同时展示客户端输入和转换后的上游输入
- **THEN** Trace MUST 同时展示上游原始输出和最终客户端输出
- **THEN** 管理员 MUST 能判断差异发生在客户端输入、协议转换、上游返回还是响应转换阶段

#### Scenario: 内容未超过配置上限

- **WHEN** 某一类 Prompt 或 Response 的原始内容大小不超过对应配置上限
- **THEN** Trace MUST 保存该内容的完整原文
- **THEN** Trace MUST NOT 标记该内容为已截断

#### Scenario: 内容超过配置上限

- **WHEN** 某一类 Prompt 或 Response 超过对应配置上限
- **THEN** Trace MUST 保存不超过上限的内容并明确标记 `truncated=true` 或等价状态
- **THEN** Trace MUST 记录原始大小和实际保存大小
- **THEN** 截断 MUST NOT 改变转发给上游或返回给客户端的真实内容

### Requirement: 多模态内容必须遵守可配置的记录边界

系统 SHALL 默认只记录图片、音频、视频和文件的媒体类型、数量、大小、URL/对象标识及其他非秘密元数据，并 MUST 支持管理员显式开启原始媒体内容记录。无论采用哪种模式，媒体记录 MUST 受独立大小上限约束。

#### Scenario: 默认处理包含 Base64 图片的请求

- **WHEN** 模型请求包含 Base64 图片且原始媒体记录未开启
- **THEN** Trace MUST 记录图片类型、可得大小和稳定标识
- **THEN** Trace MUST NOT 保存 Base64 正文

#### Scenario: 管理员开启原始媒体记录

- **WHEN** 管理员已开启原始媒体记录且媒体内容未超过上限
- **THEN** Trace MUST 保存完整媒体内容或 Langfuse 可识别的等价多模态表示
- **THEN** 超过上限时 MUST 使用与文本相同的截断标记和大小信息

### Requirement: 认证凭据永远不得进入 Trace

系统 MUST 从所有 Trace、Span、Observation、事件、属性和错误中排除 Authorization、Cookie、用户 API Key 明文、上游账号密钥、OAuth Token、签名材料及其他可用于认证的秘密。该禁止规则 MUST 不受“Prompt/Response 不脱敏”或“原始媒体记录开启”配置影响。

#### Scenario: 请求和上游调用都携带秘密凭据

- **WHEN** 客户端请求 Header 和上游请求包含不同的认证凭据
- **THEN** Trace MAY 记录 API Key ID、名称、不可逆标识和账号 ID
- **THEN** Trace MUST NOT 包含任一凭据的完整值或可直接重放的等价值

#### Scenario: Prompt 本身包含类似密钥的业务文本

- **WHEN** 用户主动把类似密钥的字符串写入 Prompt 正文
- **THEN** 系统 MUST 按“不脱敏 Prompt/Response”的已确认规则记录该正文，受内容大小上限约束
- **THEN** 系统凭据字段仍 MUST 被排除

### Requirement: 流式与取消请求必须保留已产生的现场

系统 SHALL 记录流式请求的首字节时间、已产生的客户端输出和最终结束状态。客户端断开、主动取消或上游流中断后，Trace MUST 保留截至中断时已经观察到的部分 Response，并明确区分 `cancelled`、`client_disconnected`、`stream_error` 和正常完成。

#### Scenario: 流式请求正常完成

- **WHEN** 流式模型请求成功发送全部事件并正常结束
- **THEN** Trace MUST 记录首个输出到达时间、总耗时和完整或按上限截断的客户端 Response
- **THEN** Trace MUST 标记为成功完成

#### Scenario: 客户端收到部分内容后断开

- **WHEN** 客户端在收到部分流式内容后断开连接
- **THEN** Trace MUST 保存已产生的部分客户端 Response
- **THEN** Trace MUST 标记 `client_disconnected` 或等价状态，而不是成功完成

#### Scenario: 上游流中途报错

- **WHEN** 上游在输出部分内容后返回流错误或连接中断
- **THEN** Trace MUST 保留上游与客户端已经产生的部分内容
- **THEN** Trace MUST 记录 `stream_error`、错误阶段和已知上游错误信息

### Requirement: Trace 必须支持身份、会话和请求关联

系统 SHALL 在身份可得时记录 user ID、API Key ID/名称、group ID/名称、供应商、账号 ID/名称、入口端点、入口协议、请求模型、上游模型和 request ID。客户端显式提供 `session_id`、`conversation_id`、适用平台的明确会话 Header，或结构化 metadata 中明确命名的 `session_id` 时，系统 MUST 将对应 Trace 归入同一 Langfuse Session；没有上述显式标识时 MUST 保持独立，不得根据用户、内容、缓存键或调度键自动推断。

#### Scenario: 两个请求携带相同显式会话标识

- **WHEN** 同一用户的两个模型请求携带相同且有效的显式会话标识
- **THEN** 两个请求 MUST 产生不同 Trace
- **THEN** 两个 Trace MUST 在 Langfuse 中属于同一 Session

#### Scenario: 请求没有显式会话标识

- **WHEN** 模型请求没有任何受支持的显式会话标识
- **THEN** Trace MUST 不设置推断的 Session ID
- **THEN** 同一用户的无关请求 MUST NOT 因用户 ID 或 Prompt 相似而自动合并

#### Scenario: 请求只携带缓存或调度键

- **WHEN** 模型请求只携带 `prompt_cache_key`、粘性路由 hash、内容派生值或其他未明确声明为会话标识的键
- **THEN** Trace MUST 不设置 Langfuse Session ID
- **THEN** 这些键 MUST NOT 因现有调度代码把它们视为会话信号而改变 Langfuse 归组语义

### Requirement: Trace 必须记录可得的用量、成本和阶段耗时

系统 SHALL 在数据可得时记录输入、输出、缓存、推理及图片相关 Token，用量成本、实际计费成本、请求模型、上游模型、总耗时、上游耗时、首 Token 时间和各阶段耗时。数据不可得时 MUST 保持缺失或明确标记未知，不得用零值伪装真实测量。

#### Scenario: 成功请求返回完整 Usage

- **WHEN** 上游与计费链路提供 Token 和成本事实
- **THEN** Langfuse 中对应模型观察 MUST 展示这些用量和成本
- **THEN** Trace 中的值 MUST 与该请求最终 Usage Log 使用的事实一致

#### Scenario: 请求失败且没有 Usage

- **WHEN** 请求在获得有效 Usage 前失败
- **THEN** Trace MUST 保留失败阶段和已测得耗时
- **THEN** 未获得的 Token 和成本 MUST 标记为未知或保持缺失

## MODIFIED Requirements

无。

## REMOVED Requirements

无。

## RENAMED Requirements

无。
