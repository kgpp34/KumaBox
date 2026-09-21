# KumaBox 与 Cocoon 能力对照

> 参考基线：Cocoon `27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d`
>
> 状态：living reference。每个功能切片完成时更新。

| 能力 | Cocoon 实现 | KumaBox 状态 | 结论 |
|---|---|---|---|
| CLI 生命周期 | `vm create/run/start/stop/list/inspect/console/exec/logs/rm/status` | 已有 create/start/stop/ps/inspect/console/exec/logs/rm | 命令行为逐项对齐；KumaBox 当前不加 `vm` 中间层 |
| OCI image | OCI layer 转换、direct boot artifacts | registry/OCI/Docker save、digest/diffID/EROFS/boot 验证 | 已对齐机制；KumaBox 内容校验更严格 |
| Boot layout | RO EROFS layers + ext4 COW + overlay initramfs | `overlay-v1`，相同设备/层序机制 | 机制对齐，协议名使用 `kumabox.*` |
| 状态 | created/running/stopped/error 与转换 generation | creating/created/starting/running/stopping/stopped/error/deleting | 对齐用户状态；KumaBox 显式持久中间意图 |
| 进程身份 | PID/starttime、受管 socket/dir、收敛器 | 另加 boot ID、binary、generation，Linux pidfd | KumaBox 保留更强身份验证 |
| Cloud Hypervisor stop | OCI path: API shutdown → TERM → 5s → KILL | 相同主路径，可配置 grace | 已对齐 |
| Console | PTY relay、resize、escape detach | PTY relay、resize、`^].` detach | 已对齐当前需要的交互合同 |
| Guest exec | hybrid-vsock + bounded NDJSON frames | 独立实现兼容 exec 子集 | 已对齐子集；clone/reseed 消息未实现 |
| Logs | persistent per-VM log，tail/follow/reopen，delete 清理 log dir | backend stream、tail/follow、truncate/reopen、rm cleanup | 已对齐；KumaBox 用同步有界轮询避免 watcher goroutine 泄漏 |
| Registry/backends | Cloud Hypervisor + Firecracker | 显式 Registry；只有 Cloud Hypervisor | Firecracker 后续实现相同基础合同 |
| Cgroup | per-VM scope、CPU policy、cleanup/GC | per-sandbox scope、基础 CPU limit、cleanup | 基础对齐；完整 policy/GC 后续 |
| CNI network | reserve → netns → NIC intents → ADD/TAP/TC；stop quiesce/start recover/rm DEL | 未实现 | 下一条主线进入网络切片 |
| Run | create + start，网络和资源 flags 一次确定 | 未实现 | 网络持久模型完成后实现，避免返工 |
| Status/reconcile | durable + observed state、watch/event、dead process convergence | `ps` 只读 metadata | run 后增加显式 status/recovery |
| Snapshot/clone | capture/restore/hibernate/clone、lease 和 identity reseed | 未实现 | 网络稳定后按相同故障边界分阶段实现 |
| Metadata | JSON/SQLite engines | transaction contract + SQLite | 不复制双后端；保留模块合同测试 |
| Error model | sentinel/wrapped errors | stable code/class/context/committed/action | KumaBox 保留更完整公共错误合同 |
| Package layout | 能力包与 cmd handler 直接编排较多 | `cli → core → modules` | 不复制结构；行为和失败边界对齐 |

## 网络对齐重点

Cocoon 已验证的关键点必须进入 KumaBox 首版网络：

1. sandbox/VM identity 在创建 netns 和 TAP 前持久 reserve；
2. 每个 NIC 在 CNI ADD 前写 cleanup intent；
3. partial ADD/DEL 保留记录，允许同命令或 GC 重试；
4. VMM 在目标 netns 中启动；
5. stop quiesce host veth，避免 down TAP 引发软中断开销；
6. start recover netns/TAP/IP identity 并 unquiesce；
7. rm 只有在逐 NIC DEL、TAP 和 netns 清理完成后才释放资源记录。

KumaBox 第一版先实现 CNI，不同时实现 bridge 与 hot resize。接口在真实第二实现出现时扩展，不预先复制全部能力。
