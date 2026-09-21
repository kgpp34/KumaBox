# S3 Sandbox 主线

> 状态：基础生命周期与 `logs` 已实现；下一阶段进入网络切片。

## 已实现闭环

```text
import image
  → create sandbox + ext4 COW
  → start Cloud Hypervisor
  → console / guest exec
  → stop
  → restart or rm
```

已实现命令：`create`、`start`、`stop`、`ps`、`inspect`、`console`、`exec`、`logs`、`rm`。

已实现核心合同：

- image alias 在 create 时解析为完整 manifest digest 并持久 pin；
- `creating/starting/stopping/deleting` 是可恢复的持久意图；
- VMM process identity 防 PID reuse 和 host reboot；
- cgroup 和 runtime 只在进程已确认退出后清理；
- guest exec 使用 bounded NDJSON frame，不把断线冒充 exit 0；
- stdout/stderr、JSON 和 CLI exit code 有真实 binary 测试。

## 已完成的 `logs` 切片

Cloud Hypervisor adapter 已在 `/var/log/kumabox/sandboxes/<id>/vmm.log` 持久化 stdout/stderr。新增命令只公开受控读取能力：

```text
CLI resolve name/ID
  → core validates sandbox ownership
  → VMM backend opens owned log stream
  → tail/follow renderer copies to stdout
```

合同：

- `logs SANDBOX` 输出全部；`--tail N` 从最后 N 行开始；`-f` 等待增长。
- VMM restart 截断日志后 follower 从新文件头继续，不能卡在旧 offset。
- stop 后仍可读；从未 start 返回明确 unavailable/not-found 诊断。
- cancel 关闭 watcher/file；不泄漏 goroutine 或 FD。
- CLI 不拼接 log path，`core` 不实现 tail 算法，VMM 模块拥有其日志。
- `rm` 在最终释放 metadata/name/image pin 前删除 backend 拥有的 log dir；失败保留 `deleting` 并允许相同命令重试。

实现使用同步轮询跟随受管文件，不创建 watcher goroutine。文件 inode 替换时重新打开；同一 inode 被截断时通过 size 和稳定头部签名回到 offset 0。`rm` 已在 metadata finalize 前执行 backend log cleanup，失败保留 `deleting`。

## 为什么网络在 `run` 之前

参考实现的 create/run 流程先 reserve identity，再配置网络，最后把 network facts 交给 VMM。start、stop 和 rm 都依赖相同事实。如果 KumaBox 现在先做无网络 `run`，之后必须再次修改：

- create/run flags 和 request；
- sandbox metadata schema；
- launch plan 和 Cloud Hypervisor argv；
- start rollback、stop quiesce、rm cleanup；
- JSON 输出和 runbook。

因此当前直接进入网络；网络闭环验收后实现 `run`。

## 网络第一版边界

- 只做 CNI backend；不做 bridge 和 hot resize。
- 支持 0 或多个 NIC，具体默认 NIC 数在开工前再次核对 Cocoon 当前 CLI 默认。
- MAC、IP、gateway、DNS、conflist、ifname、queue 数和 cleanup intent 持久化。
- netns/TAP/TC/CNI 操作属于 `network` 模块；`core.SandboxService` 编排其与 catalog/VMM 的顺序。
- `types` 保存跨模块 NetworkConfig 值；不保存 Network 接口。
- create 在 reserve 后配网；start recover/unquiesce；stop quiesce；rm 全量 cleanup。
- partial failure 必须可重试，不能因为 CNI DEL 失败就忘掉 NIC record。

## `run` 与 `status`

网络完成后，`run` 组合现有 create/start，不复制它们。随后实现一次性 `status`，将持久 state 与 VMM/process/cgroup/network observed facts 并列展示。watch/event 和节点级后台收敛留到一次性合同稳定后。

## 验收

每个切片必须通过 `make verify`、`make race`、`make lint`。`logs` 可在本地用受管文件验证；网络必须增加 Linux CNI runbook，并与 Cocoon 的同资源配置对比 create/start/stop/rm 行为。
