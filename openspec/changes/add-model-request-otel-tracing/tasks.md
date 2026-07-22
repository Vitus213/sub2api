# Tasks

每项 checkbox 都是可独立验收的垂直切片。每个切片从可观察行为测试 RED 开始，完成最小实现后得到 GREEN；若实现暴露行为歧义，停止该切片并先修订 specs/design。

## 1. 建立首个端到端 OTLP Trace

- [x] 1.1 [Requirement: 所有模型推理与生成请求必须具有唯一 Trace；Scenario: 成功的同步模型请求；Requirement: Trace 必须发送到唯一的自部署 Langfuse 项目；Scenario: 有效的自部署目标可用；Scenario: 未配置任何目标；Requirement: Langfuse 中的 Trace 字段必须可筛选并保持标准映射；Scenario: 管理员查看模型观察] 以 Chat Completions 为 tracer bullet：先用本地 OTLP/HTTP fake server 写出“启用部署配置后一个请求产生一个可解码 Trace、禁用/缺失目标时不产生 Trace”的 RED 合约测试，再建立 `internal/modeltrace`、显式 OTel SDK/OTLP 依赖、启动/关闭生命周期和最小网关接线，使 fake server 能验证 Trace ID、根/Generation 层级、客户端与上游 input/output、模型和 request ID；验证：`cd backend && go test ./internal/modeltrace/... ./internal/handler/... -run 'TestModelTraceOTLPChatCompletions|TestModelTraceDisabledWithoutTarget' -count=1`，预期目标测试全部通过且 fake server 只收到目标项目的一个 Trace。

## 2. 交付部署与运行时配置闭环

- [x] 2.1 [Requirement: 部署与运行时配置必须遵守确定优先级；Scenario: 只有部署配置；Scenario: 运行时配置覆盖部署配置；Scenario: 管理员运行时关闭；Scenario: 保存的运行时配置无法解析或解密；Requirement: 管理员必须能够无重启读取和更新运行时配置；Scenario: 管理员读取配置；Scenario: 管理员保存有效配置；Scenario: 非管理员尝试读取或更新；Scenario: 管理员请求 MVP 不支持的运行状态；Requirement: 配置更新必须保护秘密并保持原子语义；Scenario: 更新非秘密字段且保留秘密；Scenario: 替换项目秘密；Scenario: 无效更新] 先为部署默认、运行时整体覆盖、显式关闭、损坏回退、管理员权限、secret 保留/替换/清除、CAS 冲突和无 probe/runtime surface 写 RED 测试，再贯通后端配置管理、加密 settings、管理操作审计、`GET/PUT /api/v1/admin/model-tracing/config` 与管理员前端配置区；验证：`cd backend && go test ./internal/modeltrace/... ./internal/handler/admin/... ./internal/server/... -run 'TestModelTraceConfig|TestModelTraceAdminConfig' -count=1 && pnpm --dir frontend exec vitest run src/features/model-tracing/__tests__`，预期后端配置合约和前端保存流程通过，响应与 DOM 均不含 secret，且不存在连接测试/运行状态控件。

- [x] 2.2 [Requirement: OTLP 传输必须保护内容与项目凭据；Scenario: 管理员配置非回环 HTTP 目标；Scenario: 管理员配置 HTTPS 或回环 HTTP 目标；Scenario: 部署配置使用不安全的远端 HTTP 目标] 先为部署配置与运行时 API 写传输矩阵 RED 测试，覆盖远端 HTTPS、`localhost`/IPv4/IPv6 回环 HTTP、远端域名/IP HTTP、正常证书失败和无跳过证书校验入口；再让两类配置复用同一 endpoint validator，并确保无效运行时更新保留旧 generation、无效部署配置退化为 disabled；验证：`cd backend && go test ./internal/modeltrace/... ./internal/handler/admin/... -run 'TestModelTraceEndpointTransport|TestModelTraceRejectsRemoteHTTP|TestModelTraceTLSVerification' -count=1`，预期仅 HTTPS 与回环 HTTP 组合通过，远端明文目标不产生任何 OTLP 请求。

## 3. 固化模型路由边界和身份门禁

- [x] 3.1 [Requirement:

## 4. 保留协议转换、重试与故障转移现场

- [x] 4.1 [Requirement:

## 5. 交付有界内容、多模态和秘密隔离

- [x] 5.1 [Requirement:

## 6. 交付流式、取消和部分 Response Trace

- [x] 6.1 [Requirement:

- [x] 6.2 [Requirement:

## 7. 交付身份、Session、Usage 和成本映射

- [x] 7.1 [Requirement:

- [x] 7.2 [Requirement:

## 8. 扩展全部同步协议与入口

- [x] 8.1 [Requirement:

## 9. 关联异步图片、批量图片和长任务

- [x] 9.1 [Requirement:

## 10. 保证热切换和导出故障不影响业务

- [x] 10.1 [Requirement:

## 11. 完成全量门禁与 MVP 验收

- [x] 11.1 [Requirement:
