# KumaBox 决策记录

> 状态：normative。新决策追加；替代旧决策时明确写出关系。

## D001 — daemonless CLI 是当前执行模型

每条命令独立加载配置、打开 store、获取跨进程锁、完成操作并退出。当前不提供 daemon、gRPC 或本地控制 socket。运行中的 VMM 自身是独立进程。

## D002 — 使用根级能力包

不使用 `internal`、通用 `pkg`、`utils` 或按声明种类拆包。`core` 是应用服务和 adapter 组装层，不放在 `cmd` 下。

## D003 — `types` 只保存共享数据和值对象

接口属于拥有能力的模块，或其真实消费方。CLI DTO、SQL encoding、Cobra flags 和 terminal 状态不进入 `types`。

## D004 — SQLite 是当前 metadata 实现

`metadata` 提供 transaction contract，`metadata/sqlite` 实现它。资源事实以短事务提交，慢文件和进程操作不持有写事务。跨进程互斥依赖实体 flock 和 generation CAS。

## D005 — 镜像使用内容寻址和完整校验

导入必须验证 source digest、diffID、EROFS 和 boot artifacts。缓存命中不能只检查路径或大小。发布先进入 staging，在 digest 锁内复查并原子替换，最后提交 metadata。

## D006 — OCI boot contract 使用 `overlay-v1`

可启动镜像显式声明 `io.kumabox.boot.profile=overlay-v1`。kernel 参数和 disk serial 使用 `kumabox.*` 命名。未声明 profile 的旧镜像可以导入和 inspect，但不能 start。

## D007 — `created` 与 `stopped` 分离

`created` 表示资源已准备且从未启动；`stopped` 表示 VMM 曾运行并已退出。中间状态是持久恢复意图，不是短暂展示值。

## D008 — VMM 使用基础 Backend + 显式 Registry

`vmm.Backend` 保留所有 VMM 都需要的进程级生命周期能力。Registry 在构造时拒绝 nil、重复和类型错误，之后不可变。snapshot/pause 等只有真实实现出现时才增加可选接口。

## D009 — 进程操作验证完整身份

PID 不构成所有权。信号和清理必须核对 starttime、host boot ID、sandbox ID、generation、binary 和受管 endpoints，并在 Linux 使用 pidfd 固定目标。

## D010 — Agent exec 对齐参考实现的 NDJSON 子集

消息语义兼容 `exec/stdin/stdin_close/started/stdout/stderr/exit/error`，传输使用 private hybrid-vsock。KumaBox 不复用参考产品名称、boot ABI 或源码。

## D011 — 网络必须进入 sandbox 生命周期

网络不能只是 VMM argv 的附加字段。sandbox identity 先 reserve，随后建立可回收的逐 NIC intent 和 host plumbing；start/stop/rm 分别负责 recover、quiesce 和 cleanup。网络在 `run` 之前实现，避免重复设计持久模型和补偿。

## D012 — Cocoon 是能力基线，不是包结构模板

每项主线功能核对固定 commit 的命令、流程、状态、失败恢复和测试。默认保持行为对齐；KumaBox 已有更强的内容完整性、错误分类或模块边界时保留，并在对照表记录差异。

## D013 — 规范文档进入 Git

当前规范、决策、活动提案和 Linux runbook 与代码一起审查。过时设计稿不进入仓库。中文是当前设计规范语言；代码注释、公共 API、CLI help 和根 README 使用英文。
