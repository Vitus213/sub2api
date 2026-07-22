# Sub2API 项目规约

本文件是 sub2api 仓库的项目级规约，补充全局 `~/.omp/agent/AGENTS.md`。冲突时以全局规约为准，但本文件的 GitHub 相关约定在本仓库内生效。

## 远端与分支

- 本仓库远端是 **GitHub**：`origin` = `git@github.com:Vitus213/sub2api.git`（个人 fork）。
- 分支命名：功能分支 `<scope>/<short-slug>`。模型请求 OTEL→Langfuse 透传追踪这个特性（`backend/internal/modeltrace/`、`openspec/changes/add-model-request-otel-tracing/`、`skills/sub2api-model-trace-e2e/`）归在 `otel/` 前缀下，如 `otel/model-trace`。OTel 是功能特性名，不是分支体系。
- **禁止**：把本仓库路由给 `antcode-skill`；本仓库与 AntCode 无关。涉及 PR / Issue / 分支 / pipeline 时使用 `gh` CLI 或 `git`。
- 提交前 `git status --porcelain` + `git diff --cached --name-only` 自检；未授权不提交。

## 工具偏好

- GitHub 操作：优先 `gh` CLI（`gh pr create`、`gh pr view`、`gh issue list` 等）；缺失时 fallback 到 `git push` + 浏览器。
- 代码搜索：优先 `grep` / `glob` / `lsp`，不用 antcode search。
- CI：本仓库走 GitHub Actions（`.github/workflows/`），不涉及 AntCode pipeline。

## OTEL 与 e2e 测试

- 模型请求 OTEL/Langfuse 追踪的实现与规格在 `openspec/changes/add-model-request-otel-tracing/`。
- 端到端测试流程沉淀在 `skills/sub2api-model-trace-e2e/`，每次涉及模型追踪代码的改动都应跑一次该 skill 的 smoke。
- 本地环境默认用 Colima profile `swebench` 运行 Docker；Langfuse 部署在 `http://localhost:3000`，预置凭据 `pk-lf-local`/`sk-lf-local`（仅本地）。

## 模型追踪实施记录

- `add-model-request-otel-tracing` 每个 OpenSpec task 的踩坑、根因、修复、持久化产物、验证证据和剩余边界，统一追加到 `docs/MODEL_TRACING_IMPLEMENTATION_LOG.md`。
- 进入下一 task 前必须先更新该记录；运行时配置、端口、容器、生成命令、provider 取值位置等会影响后续 agent 的持久化事实也必须记录。
- 禁止在记录中写入任何真实或本地测试凭据；只允许记录 provider 名称、配置键名和安全取值位置。

## 禁止

- 主动 `git commit` / `git push` / `gh pr create`：除非用户明确要求。
- 把本地测试凭据（`pk-lf-local`/`sk-lf-local`/`admin123456`）写进提交、文档或 PR。
- 在生产路径引入对本地 Langfuse endpoint 的硬编码。