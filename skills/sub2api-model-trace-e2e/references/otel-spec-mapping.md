# OTEL Spec 映射与 503 设计依据

## OpenSpec 来源

完整规格在仓库 `openspec/changes/add-model-request-otel-tracing/`：
- `proposal.md` — 变更动机与范围
- `design.md` — 14 个设计决策，含 `internal/modeltrace`、根 Trace + Generation、fail-open、endpoint 校验和 Langfuse 属性映射
- `specs/model-request-tracing/spec.md` — 模型请求追踪能力需求
- `specs/langfuse-otel-export/spec.md` — Langfuse OTLP 导出能力需求
- `tasks.md` — 真源中只有 1.1 / 2.1 / 2.2 已勾选；3.1 及以后仍未勾选。本 reference 只记录 e2e harness 已覆盖的行为，不代替或推断 OpenSpec task 完成状态。

## 本 e2e 验证的 spec 映射

| Spec Requirement | Scenario | e2e 验证点 |
|---|---|---|
| 所有模型推理与生成请求必须具有唯一 Trace | 已识别请求在真实发送前失败 | HTTP 503；按唯一 session/request ID 只有 1 条 Trace、1 个根 SPAN、0 GENERATION |
| 身份建立前不得创建 Trace | 匿名请求、未知 API Key | HTTP 401，按唯一 request ID 查询均为 0 Trace |
| 已识别身份的鉴权失败必须追踪 | disabled API Key | HTTP 401；1 个 ERROR 根 SPAN、0 GENERATION，且 user/API Key/group 数值身份正确 |
| 控制面不得创建模型 Trace | 已鉴权 `GET /v1/models` | HTTP 200，按唯一 request ID 查询为 0 Trace |
| 一个 Trace 必须保留完整尝试层级 | 本地 fixture 固定 429 后切换账号 200 | 一个根 Trace；`upstream.attempt.1/2` 两个真实 GENERATION 均为根的直接子项，账号和 ERROR/成功状态可区分 |
| Trace 必须区分客户端与上游内容视角 | OpenAI Chat Completions → Anthropic Messages | 根保存客户端 input/output，attempt 保存转换后 input 与 Anthropic SSE output；上游响应 credential 字段只出现 `[REDACTED]` |
| 有界内容、多模态和秘密隔离 | 2 KiB Prompt 上限 + Base64 图片 + 分离 canary | 有确定性截断标记和媒体 fingerprint/approx_bytes；请求字段 secret、媒体正文、用户/上游 API Key、上游响应 secret 在真实 Langfuse 中命中数为 0 |
| 默认内容上限可部署并真实接收 | 启动不覆盖默认值；运行时恢复 1 MiB 后发送 1,040,000 字节正文 | admin GET 的 prompt/response/media 均为 1048576；Langfuse root input 保留 head/tail canary、长度接近上限且无截断标记 |
| 流式请求正常完成 | 单成功账号返回完整 Anthropic SSE | 客户端 HTTP 200、`text/event-stream`、content/finish/[DONE] 帧；Langfuse 只有一个根 Trace，真实 attempt GENERATION 是根的直接子项，根状态为 `completed` |
| 异步/批量模型执行必须续接提交 Trace | Gemini Batch API 200 item（一个成功、199 个 provider 失败） | API 200、worker `completed`；同一逻辑 Trace 下 1 个提交根 SPAN、200 个直系 `model.async.execution` GENERATION，API 与 Langfuse 的 `item_id` 均为精确且唯一的 `item-0..199` 集合，fingerprint 均匹配，终态 1 个 `completed` / 199 个 `failed`；媒体 canary 零泄露 |
| 认证凭据永远不得进入 Trace | 配置 API、客户端 Header、上游请求/响应 | 配置响应不回显 secret；ClickHouse 对本次所有 observation/trace 执行完整 canary 零命中查询 |
| Trace 必须发送到唯一的自部署 Langfuse 项目 | 本地目标可用 | 只配置 `http://127.0.0.1:3000`；公开 health 版本必须 `>=3.22.0`，并记录 OCI version/revision/digest |
| 部署与运行时配置必须遵守确定优先级 | 部署默认、运行时小上限、恢复默认、secret 保留与 CAS | 先验证 deployment version 0，再运行时 version 1/2/3；远端明文 HTTP 被拒且不改旧配置，空 secret 保留，过期 version 返回 409 |
| 观测故障必须 fail-open 且 MVP 不保证补送 | OTLP 端点固定 500 与 1.5 秒延迟 | 两次业务请求均在约 50ms 内返回 200；fixture 分别观测到 `otlp_500>=1`、`slow_exports>=1`，证明 exporter 错误与阻塞不拖住业务路径 |

## 503、failover、SSE 与异步 batch 的本地执行链

1. Candidate middleware 在模型执行路由安装延迟身份 Hook；匿名/未知 Key 阶段不创建 Span。
2. API Key 仓储解析出真实身份后创建唯一根 Trace。
3. 原始 `e2e-key` group 在身份、截断和 1 MiB 场景尚无账号，因此 Handler 确定性返回 503；这些 Trace 只有根 SPAN，不得虚构 attempt。
4. 独立 failover group 挂两个本地 Anthropic 账号：priority 1 指向 `/fail` 固定 429，priority 2 指向 `/ok` 固定 200 完整 SSE；一次非流式客户端请求产生两个真实 attempt GENERATION。
5. 完成 503 场景后，原始 group 只挂一个 `/ok` 账号；`stream=true` 请求经协议转换把本地 Anthropic SSE 作为 OpenAI SSE 返回，客户端必须收到内容帧、成功终态和 `[DONE]`。
6. 独立 Gemini batch group 通过本地一次性 CA 把 `generativelanguage.googleapis.com:443` 定向到 TLS fixture；提交 API 保存最小 trace continuation，真实 queue worker 完成 upload/create/poll/download 后，为配置上限 200 个 item 在原提交根下分别结束 1 个成功与 199 个失败 Generation。
7. 根 finalizer 捕获最终客户端状态与安全 input/output；BatchSpanProcessor flush 后脚本在 Langfuse ClickHouse 断言 cardinality、父子关系、续接 fingerprint、状态和 canary。

这四类路径分别证明“未发送不造 attempt”“真实 429→200 保留全部 attempt”“正常 SSE 保留帧与 completed 终态”“真实 queue/worker 续接同一 Trace 且不泄露媒体”，不能互相替代。

## 为什么不调用真实外部 LLM

1. **成本与凭据**：不使用生产/个人 LLM Key，不产生外部费用。
2. **确定性**：本地 Go fixture 固定提供 Anthropic 健康 200、失败 429、完整 SSE，以及 Gemini File/Batch API 的 upload/create/poll/download；不受限流、网络和模型版本漂移影响。
3. **覆盖边界**：无账号 503、故障转移 429→200、非流式协议转换、真实 SSE 正常完成和真实 queue/worker 异步续接都在同一隔离环境可复现。
4. **不等价声明**：本地 fixture 证明网关、异步 worker 与 Langfuse 链路，不证明任一外部厂商服务当前可用，也不证明生产网络、证书或配额。

## 当前 e2e harness 对 OpenSpec tasks 的覆盖

下表是脚本行为覆盖，不修改 `tasks.md` checkbox，也不等于完整 task 已完成。

| Task | 当前真实 smoke 覆盖 | 仍未由本脚本覆盖 |
|---|---|---|
| 3.1 | 匿名/未知/控制面 0 Trace；已识别无账号 503 和 disabled 401 各单根 Trace | 全部受限 Key、协议校验和控制面矩阵 |
| 4.1 | Chat Completions→Anthropic；确定性 429→200 与所有尝试均失败；每个真实 attempt 的账号、状态、父子和安全 output | 更多协议转换组合 |
| 5.1 | 小上限截断、默认媒体 descriptor、系统 secret/media canary 零命中；部署默认 1 MiB 可观察且接近上限 Prompt 真正落 Langfuse | 原始媒体 opt-in 及更多媒体类型 |
| 6.1 | 正常 SSE 的客户端帧、真实 Generation 层级和 `completed` 终态；真实慢/500 exporter fail-open | 客户端断连和上游中途错误仍未由真实 Langfuse 黑盒覆盖，因此不得标为完整 6.1 验收 |
| 9.1 | 真实 Gemini Batch API 提交、queue/worker、配置上限 200 item 成败终态；API 与 Langfuse 的 item ID 精确唯一集合、持久化 continuation fingerprint 匹配，并在同一 Trace 根下生成 200 个直系 GENERATION；默认媒体 canary 零泄露 | 异步图片入口、fingerprint 错配/目标切换/关闭、item 重试和任务取消；因此不得标为完整 9.1 验收 |

## 尚未由本 e2e 完整验收的 OpenSpec tasks / 场景

| Task | 缺口 |
|---|---|
| 6.1 | 正常 SSE、慢 exporter 与 exporter 500 有真实 smoke；客户端断连、上游部分失败仍缺真实 Langfuse 黑盒 |
| 6.2 | Responses WebSocket 多回合、断连和回合间配置切换未在本脚本执行 |
| 7.1 | 本脚本未逐字段比对最终 Usage Log、token/cost；未知 Usage 不伪造由完整后端测试保护 |
| 7.2 | 当前 smoke 验证显式同 session 归组及 `prompt_cache_key` 排除；完整 allowlist 和跨协议矩阵未执行 |
| 8.1 | 全同步协议/入口矩阵未执行 |
| 9.1 | 已在真实 queue/worker 验证配置上限 200 item 的精确唯一身份集合；异步图片、fingerprint 错配与 OTel Link、目标切换/关闭、进程重启、item 重试和取消仍缺 |
| 10.1 | 已真实验证进行中请求采用旧配置完成、关闭后新请求不追踪、500/慢 exporter fail-open；目标切换和进程崩溃隔离未由黑盒脚本执行 |
| 11.1 | `run_full_e2e.sh` 执行全后端/前端测试、相关 race、production build、200-item 黑盒与本机 benchmark；benchmark 只记录当前基线，不证明生产性能或跨机器阈值 |

## 与 sub2api 仓库规约的关系

项目 `AGENTS.md` 规定：
- 远端用 GitHub（`origin = Vitus213/sub2api`），跨 fork PR 用 `gh pr create --repo Wei-Shaw/sub2api`
- **禁止**触发 `antcode-skill`
- OTEL/e2e 工作分支用 `otel/` 前缀
- 本地测试凭据不进提交

本 skill 遵守该规约：脚本不 `git commit`/`git push`/`gh pr create`，只跑容器和 HTTP。