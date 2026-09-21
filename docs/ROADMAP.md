# KumaBox 主线路线图

> 状态：normative

本路线图从实际代码和 Cocoon commit `27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` 的对应实现出发。每个切片单独设计、实现、验收和提交。

## 已完成

### 基础工程

- daemonless CLI、doctor、配置加载、稳定错误码和退出码；
- SQLite transaction contract、flock、受管路径和原子发布；
- 根级模块、`core` 应用服务、显式 VMM registry；
- 公共进度 renderer 和真实 CLI binary 测试。

### 镜像

- registry pull；
- OCI layout/archive 和 Docker save import；
- digest、diffID、whiteout、boot artifact 验证；
- EROFS conversion、cache reuse、list/inspect/verify/remove；
- `overlay-v1` boot profile 与官方 Ubuntu guest image。

### Sandbox 基础生命周期

- `create`、`ps`、`inspect`、`rm`；
- `start`、`stop`、`console`、`exec`；
- `logs` 全量/tail/follow、truncate/reopen 恢复和删除时日志清理；
- sparse ext4 COW、cgroup v2、Cloud Hypervisor direct boot；
- PID/starttime/boot ID/binary/socket identity；
- guest agent NDJSON exec 子集。

这些功能已完成代码和跨平台门禁。Linux/KVM 行为仍必须在发布前按 runbook 重验。

## 下一步：网络基础

网络在 `run` 前实现。原因是 Cocoon 的网络身份在 sandbox reserve 之后、VMM create/start 之前建立，并贯穿 start/stop/rm；先做无网络 `run` 会重复修改命令、metadata 和补偿流程。

第一版范围：

1. 根级 `network` 能力与 `network/cni` adapter；
2. `types.NetworkConfig` 和 sandbox 持久网络事实；
3. CNI conflist/bin 配置、netns、TAP、TC redirect 和逐 NIC intent；
4. `create --network NAME --nics N`，默认行为在开工评审时与 Cocoon 当前默认再次确认；
5. Cloud Hypervisor net devices、kernel IP/DNS/hostname 参数；
6. stop quiesce、start recover/unquiesce、rm CNI DEL/netns cleanup；
7. 中断恢复、generation fence 和 Linux runbook。

第一版不做 bridge backend、NIC hot-resize 或 Firecracker；接口必须允许这些真实第二实现以后加入。

## 网络之后：`run`

`run` 复用 `create` 和 `start` 的应用服务，不复制流程：

```text
kumabox run IMAGE --name NAME [resource/network flags]
kumabox run IMAGE --name NAME -- COMMAND [ARGS...]
```

默认等待 guest agent。带命令时透传 stdout/stderr 和 exit code，command 退出后 sandbox 保持运行。创建已提交但后续启动或 agent wait 失败时，错误必须说明 sandbox 已存在并可 inspect/stop/rm。

## 然后：`status` 与恢复

Cocoon 的 `list`/`status` 会把持久状态与进程事实合并，并支持 watch/event。KumaBox 当前 `ps` 有意保持只读 metadata 视图。

计划分两步：

1. 一次性 `status [SANDBOX...] [--json]`：观察 VMM/process/cgroup/network，展示 durable state 与 observed state；
2. `--watch`/`--event`：只有在事件和轮询语义确定后加入。

同时提供显式的实体恢复入口，处理遗留 `creating/starting/stopping/deleting`。不能让普通 `ps` 在查询时偷偷修改状态。

## 后续能力

1. snapshot、restore、hibernate；
2. clone 与 guest identity reseed；
3. NIC resize、额外磁盘和 filesystem attach；
4. Firecracker backend；
5. 节点级 reconcile、GC、admission 和容量统计；
6. 跨节点与可选常驻服务。

每个阶段继续以 Cocoon 的真实流程、失败恢复和测试为基线；KumaBox 的模块边界、错误模型和完整性保证保留。
