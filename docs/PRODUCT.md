# KumaBox 产品范围

> 状态：normative

KumaBox 是面向 AI agent 工作负载的 daemonless microVM sandbox runtime。每条 CLI 命令独立打开持久化数据、取得资源锁、完成操作并退出；运行中的 sandbox 由 VMM 进程承载。

## 当前目标

KumaBox 当前主线先完成单机 Linux 上的 OCI microVM 闭环：

1. 从 registry、OCI layout/archive 或 Docker save archive 导入镜像。
2. 将镜像层转换为只读 EROFS，并提取直接内核启动所需的 kernel/initramfs。
3. 创建带私有 sparse ext4 COW 的 sandbox。
4. 使用 Cloud Hypervisor 启动、停止和重新启动 sandbox。
5. 通过 console 与 guest agent 进入 guest。
6. 在完整生命周期稳定后接入 CNI 网络，再做 snapshot、clone 和第二 VMM backend。

## 对齐基线

Cocoon commit `27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` 是能力与行为基线。每个切片开始前都要核对其真实实现、测试和失败恢复，而不是只比命令名称。

默认对齐：

- Docker 风格的镜像和 VM 命令面；
- OCI layer + ext4 COW + direct boot 机制；
- `created`、`running`、`stopped` 等用户可理解的生命周期；
- PID/starttime/boot ID 进程身份保护；
- guest exec 的 NDJSON 消息语义；
- Cloud Hypervisor shutdown 后 TERM→KILL 的停止路径；
- CNI netns/TAP、持久网络身份、stop quiesce、start recover、delete cleanup；
- snapshot、clone、status/reconcile 等后续能力的失败边界。

允许有意不同：

- KumaBox 使用根级模块和 `core` 应用服务，不复制 Cocoon 的包结构。
- `types` 只放共享数据和值对象，不收集接口。
- `errdefs` 保留稳定错误码、提交状态和完整错误链。
- 镜像缓存必须验证 digest、diffID 和最终产物，不能退化为只检查文件存在。
- guest boot 参数和 profile 使用 `kumabox.*` 命名，不冒充其他产品协议。

所有差异记录在 [COCOON-MAP.md](COCOON-MAP.md)。

## 当前不承诺

以下能力尚未实现，不能在帮助、README 或输出中描述成可用：

- guest 网络和多网卡；
- snapshot、restore、hibernate、clone；
- Firecracker；
- cloud image、UEFI 和 Windows；
- 热插磁盘、文件系统或 NIC；
- 跨节点控制面、daemon 或远程 API；
- 生产级全节点 GC、admission 和容量调度。

## 用户合同

- stdout 只承载命令结果，stderr 承载进度和诊断。
- JSON 使用稳定字段名和缩进格式。
- 状态只在相应宿主事实完成后提交。
- 失败后保留足够的持久意图，使同一命令可以安全重试。
- 不向未经身份确认的宿主进程发送信号，不删除归属不确定的资源。
