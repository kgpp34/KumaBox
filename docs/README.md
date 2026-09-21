# KumaBox 文档

本目录随代码进入 Git。行为、架构、配置或验收方式发生变化时，相关文档必须在同一提交中更新。

## 当前规范

| 文档 | 内容 |
|---|---|
| [PRODUCT.md](PRODUCT.md) | 产品范围、能力基线与明确不做的内容 |
| [ARCHITECTURE.md](ARCHITECTURE.md) | 包边界、依赖方向、事实归属和关键流程 |
| [BEHAVIOR.md](BEHAVIOR.md) | 当前命令、状态、输出、错误与恢复语义 |
| [CONFIGURATION.md](CONFIGURATION.md) | 配置文件、环境变量、flags、默认值与优先级 |
| [HOST.md](HOST.md) | 主机依赖与 `doctor` 检查范围 |
| [PERFORMANCE.md](PERFORMANCE.md) | 性能口径、基准场景和优化约束 |
| [ROADMAP.md](ROADMAP.md) | 已完成能力、下一条命令以及网络等后续阶段 |
| [DECISIONS.md](DECISIONS.md) | 仍然有效的架构和产品决策 |
| [COCOON-MAP.md](COCOON-MAP.md) | 与参考实现的能力对齐状态和有意差异 |

## 实施资料

- [proposals/s3-sandbox.md](proposals/s3-sandbox.md)：当前沙箱主线的已实现范围与后续切片。
- [runbooks/s2-oci.md](runbooks/s2-oci.md)：Linux 镜像导入验收。
- [runbooks/s3-create.md](runbooks/s3-create.md)：Linux 沙箱生命周期验收。
- [architecture-diagrams.md](architecture-diagrams.md)：当前代码对应的简图。
- [releasing.md](releasing.md)：发布流程。

[REFACTORING.md](REFACTORING.md) 记录 2026-09 的结构整理及验收结果。该轮结束后它不再决定功能开发顺序；后续主线以 `ROADMAP.md`、当前行为规范和逐功能评审为准。

## 文档语言

设计规范以中文维护，代码注释、公共 Go API 注释、命令帮助和根 README 使用英文。面向国际用户的完整英文文档可在功能面稳定后补充，中文规范仍是当前设计评审的事实来源。
