# Spec Delta

spec 只描述可观察行为、输入、输出、状态变化和错误条件。每项 Requirement 使用 SHALL 或 MUST，并至少有一个严格四级 Scenario；内部锁、缓存、类、表与部署方案写入 design.md。

## ADDED Requirements

在本节写全新的行为。每项使用以下固定标题与场景格式；为正常、错误和边界路径分别写 Scenario。

### Requirement: 使用稳定且可识别的需求名称

紧接标题写完整的规范性行为，不写实现步骤。

#### Scenario: 使用可区分的场景名称

- **WHEN** 写出可观察的触发条件
- **THEN** 写出可观察的结果

## MODIFIED Requirements

从基线 spec 复制需要修改的整个 Requirement 块，包括全部 Scenario，再编辑为完整的新行为。只写增量片段会在归档时丢失旧内容。没有修改时保留本节并写“无”。

## REMOVED Requirements

写出被移除 Requirement 的准确名称，并为每项提供 `**Reason**` 与 `**Migration**`。没有移除时保留本节并写“无”。

## RENAMED Requirements

仅在行为不变、名称变化时使用 `FROM: Requirement: 原名称` 和 `TO: Requirement: 新名称`。行为也变化时改用 MODIFIED。没有重命名时保留本节并写“无”。
