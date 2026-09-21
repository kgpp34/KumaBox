# KumaBox 架构规范

> 状态：normative

## 1. 结构

KumaBox 使用按能力划分的根级 Go 包，不使用 `internal`、通用 `pkg`、`utils` 或按“接口/结构体”种类划分的包。

```text
cmd/kumabox ──► cli ──► core
                         │
             ┌───────────┼─────────────┐
             ▼           ▼             ▼
          images       sandbox        vmm
             │        catalog/disk      │
             ▼           │       cloudhypervisor
       metadata/sqlite ◄─┘             │
             ▲                         ▼
             └──────── types ◄──── cgroup/agent
```

- `cmd/kumabox`：信号、进程退出码和入口。
- `cli`：命令树、参数解析、展示和 stdout/stderr 分流。
- `core`：跨模块应用流程、补偿顺序和具体 adapter 装配。
- `types`：跨模块资源数据和值对象；不放接口、SQL 编码或 CLI DTO。
- `images`：镜像来源无关的导入、验证、启动文件选择和删除规则。
- `sandbox`、`disk`：sandbox 路径、锁和 writable disk。
- `vmm`：backend 合同、registry、launch plan、process identity。
- `vmm/cloudhypervisor`：Cloud Hypervisor 参数、进程、API 和 console adapter。
- `agent`：guest exec 消息和传输。
- `metadata`：事务合同；`metadata/sqlite` 是当前持久化实现。

`cli` 不编排跨模块事务。具体 adapter 不导入 `core` 或 `cli`。接口放在拥有能力的模块，或唯一消费该能力的包中。

## 2. 组装入口

每条命令加载一次不可变 `config.Config`。`core.OpenImages` 和 `core.OpenSandbox` 创建路径、SQLite store、catalog、disk、cgroup 和 VMM registry。模块收到自己的 Options 后不再读取环境或全局配置。

```text
defaults → explicit config file → environment → flags → Validate
                                                       ↓
                                               core assembly
```

## 3. 持久化与锁

SQLite 是资源事实源。文件系统保存大文件和运行产物，不替代 metadata 状态。

- image digest 锁串行化同一内容的发布和删除。
- sandbox 实体锁串行化 create/start/stop/rm；logs follow 不持锁。
- generation compare-and-swap 拒绝陈旧状态提交。
- 慢 I/O 不放在 SQLite 写事务中。
- 发布顺序为“持久意图 → 慢操作 → 验证宿主事实 → 短事务提交”。
- 取消后的补偿使用独立且有界的 context。

## 4. 镜像流程

```text
resolve source → verify source digest/diffID → convert staging
       → digest locks → recheck/reuse or atomic publish
       → final artifact verification → metadata commit
```

manifest 层序保持 base-to-top；VMM 磁盘也按该顺序附加。guest overlay lowerdir 按 top-to-base 使用。重复 source digest 可以复用存储，但不能从 manifest 设备序列中删除。

## 5. Sandbox 状态机

```text
creating → created → starting → running → stopping → stopped
    │                    │          │                    │
    └────── error ◄──────┴──────────┘                    │
                                                        start
created/stopped/error ── rm ──► deleting ── cleanup ──► removed
```

- `created` 表示资源已准备、从未启动。
- `stopped` 表示至少成功启动过一次且 VMM 已退出。
- `error` 保留失败 phase 和资源所有权。
- `deleting` 是可重试的清理意图，最终事务同时释放 name、record 和 image pin。

## 6. VMM 与进程身份

`vmm.Backend` 定义所有 backend 必须具备的基础生命周期能力。`vmm.Registry` 是构造后不可变的显式实例。未来 Firecracker 实现同一合同；只有 snapshot 等真实可选能力出现时才增加窄的可选接口。

进程身份至少包含 PID、`/proc` starttime、host boot ID、sandbox ID、generation、binary 和 API socket。观察、信号和清理必须验证完整身份。Linux 信号路径使用 pidfd 固定目标。

Cloud Hypervisor readiness 需要同时满足：进程身份仍一致、API 可连接、`vm.info` 为 `Running`。socket 文件存在不等于成功。

VMM backend 同时拥有持久进程日志的读取和删除能力。`core` 只按 sandbox 中持久化的 VMM 类型路由，CLI 不拼宿主路径。stop 只清理可重建 runtime/cgroup 并保留日志；rm 按 COW → backend logs → metadata finalize 的顺序回收，任何失败保留 `deleting`。

## 7. Guest agent

host 通过 Cloud Hypervisor hybrid-vsock UDS 连接 guest port 1024。应用协议是有大小上限的 NDJSON：`exec`、`stdin`、`stdin_close`、`started`、`stdout`、`stderr`、`exit`、`error`。guest EOF 没有 terminal frame 时是协议失败，不能当 exit 0。

当前协议行为与参考实现的 exec 子集兼容，但 boot profile、kernel 参数和 service 名称属于 KumaBox。

## 8. 网络接入原则

网络尚未实现。接入时沿用参考实现已经验证的顺序：

```text
reserve sandbox identity
  → prepare netns
  → persist per-NIC cleanup intent
  → CNI ADD + TAP/TC redirect
  → persist resolved MAC/IP/network identity
  → launch VMM inside netns
```

stop 保留 netns/TAP/IP，并 quiesce host veth；start 先 recover/unquiesce；rm 执行可重试 CNI DEL 和 netns 清理。任何失败都保留足以重试的逐 NIC 记录。网络事实进入共享 `types`，网络能力属于新的根级 `network` 包，编排仍在 `core.SandboxService`。

## 9. Cocoon 对齐规则

每项功能都检查参考实现的命令、持久事实、锁、进程/网络顺序、失败恢复和测试。默认采用经过验证的语义；若 KumaBox 选择不同方案，必须在对照表中记录原因和兼容影响。不得为了表面同名破坏现有更强的完整性或模块边界。

## 10. 测试层级

- 单元测试：值对象、解析、状态和纯计划。
- adapter 测试：真实 SQLite/文件系统、本地 socket、进程和取消。
- binary 测试：真实 `kumabox` 的输出、退出码和信号。
- Linux runbook：KVM、Cloud Hypervisor、cgroup v2、EROFS、ext4、vsock，未来包括 CNI。

`make verify`、`make race` 和 `make lint` 是提交门禁。Linux 专属行为不能用 mock 结果冒充真实验收。
