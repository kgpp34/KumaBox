# KumaBox 行为规范

> 状态：normative

本文件只描述当前已实现行为。规划中的命令见 [ROADMAP.md](ROADMAP.md)。

## 1. 通用行为

- 资源引用接受完整 sandbox UUID 或精确名称，不接受模糊前缀。
- 表格输出包含标题；JSON 使用两个空格缩进并以换行结尾。
- stdout 只输出结果，stderr 输出进度和错误。
- TTY 上进度使用 spinner；重定向时使用有限的普通文本行，不写控制字符。
- SIGINT/SIGTERM 取消当前操作。已越过提交点的错误会明确保留资源并要求 inspect。

退出码：

| 状态 | 含义 |
|---:|---|
| 0 | 成功 |
| 1 | 未分类内部失败或 guest command 通用失败 |
| 2 | 命令、flag 或参数数量错误 |
| 3 | 资源不存在 |
| 4 | 名称、状态或引用冲突 |
| 5 | 参数、主机、镜像或内容不兼容/损坏 |
| 6 | 产物暂不可用或 metadata store 超时 |

`exec` 中 guest 进程的非零退出码直接成为本地退出码。

## 2. `doctor`

`kumabox doctor [--fix] [--upgrade] [--subnet=CIDR]` 将参数和标准流交给 `kumabox-check`。无修复参数时只检查。安装、升级或修改宿主机只在用户显式传入相应参数时发生。

## 3. 镜像命令

### `image pull REF`

从 registry 解析指定平台的 OCI manifest，验证 config、layer digest 和 diffID，转换并提交本地镜像。默认平台为当前架构对应的 `linux/amd64` 或 `linux/arm64`。

### `image import NAME PATH`

支持 OCI layout 目录、OCI archive 和 Docker save archive。`--format auto|oci|docker` 控制解析器；auto 按内容检测。Docker archive 多镜像时可用 `--source-tag` 选择。压缩与否不依赖扩展名。

导入满足：

1. source 内容完整校验；
2. layer 流式解包并转换为 EROFS；
3. boot whiteout/opaque/覆盖语义按 OCI 层序计算；
4. staging 产物在 digest 锁内原子发布；
5. 完整验证后才提交 metadata。

同一内容可复用已验证的受管产物。失败不会留下可见的成功 image record。

### 查询和删除

- `image list`，别名 `image ls`：表格；`--json` 输出完整数组。
- `image inspect IMAGE`：缩进 JSON。
- `image verify IMAGE`：重新验证 layer、EROFS 和 boot artifacts。
- `image remove IMAGE...`，别名 `image rm`：删除名称；最后一个引用消失后清理产物。

被 sandbox pin 的镜像不能删除。

## 4. Sandbox 命令

### `create IMAGE --name NAME`

可选资源 flags：`--cpus`、`--memory`、`--storage`、`--json`。默认 2 vCPU、1 GiB 内存和 10 GiB sparse COW；最小内存 512 MiB，最小存储 10 GiB。

流程：

```text
validate → lock → resolve and pin image → reserve creating record
         → create/format cow.raw → verify → CAS created
```

`created` 不代表 VMM 已启动。创建失败保留 `error` 记录和诊断，清理完成后可由 `rm` 删除。

### `start SANDBOX`

只接受 `created`、`stopped` 或可恢复的 `starting`。启动前验证 pinned image、`overlay-v1` profile、kernel/initramfs、COW、KVM、cgroup 和 VMM binary。

```text
lock → CAS starting → prepare runtime/cgroup → launch
     → persist process identity → wait vm.info Running → CAS running
```

启动失败会终止本次进程并清理 runtime；无法完整补偿时保留可诊断状态。重试同一 sandbox 不创建新身份或新 COW。

### `stop SANDBOX`

对 `created` 和 `stopped` 幂等成功。`running` 先提交 `stopping`，向 Cloud Hypervisor 请求 `vm.shutdown`，随后对完全匹配的进程执行 SIGTERM，超过配置窗口后 SIGKILL。确认进程退出并清理 runtime/cgroup 后才提交 `stopped`。

中断后保持 `stopping`，再次执行同一命令继续收敛。

### `ps`

默认只显示活动状态；`-a/--all` 包含所有持久记录。`--quiet` 只输出完整 UUID，`--json` 输出完整数组；二者互斥。查询不修改状态。

### `inspect SANDBOX`

始终输出缩进 JSON，包括 immutable 资源规格、image digest、VMM、state、generation、时间和可选 failure。generation 是状态提交的单调版本，用于阻止陈旧操作覆盖新状态。

### `console SANDBOX`

仅连接 `running` sandbox。命令验证当前 generation、process identity 和 VMM API 后打开 PTY；不会在 relay 期间持有实体锁。默认按 `Ctrl-]` 后 `.` 断开，可用 `--escape-char` 修改。断开 console 不停止 sandbox。

### `exec SANDBOX -- COMMAND`

参数直接交给 guest，不隐式插入 shell。`-e/--env KEY=VALUE` 可重复；`-i/--interactive` 才连接 stdin，否则立即发送 `stdin_close`。stdout/stderr 独立透传，guest exit code 原样返回。

### `logs [--tail N] [-f] SANDBOX`

按 name 或完整 ID 解析 sandbox，并由其持久 VMM backend 提供日志。默认 `--tail 0` 输出完整日志；正数从最后 N 行开始。`-f/--follow` 继续读取追加内容，VMM 重启导致文件截断或替换时从新文件头继续；取消 follow 正常退出。

日志内容只写 stdout。sandbox 从未启动、尚无日志时返回 `ARTIFACT_UNAVAILABLE`。stop 后日志保留并可继续读取；命令不要求 sandbox 处于 running，也不会在 follow 期间持有实体锁。

### `rm SANDBOX`

拒绝删除活动状态；VMM runtime 和 cgroup 必须先由 `stop` 收敛。命令提交 `deleting`，依次清理 COW 和 backend 拥有的持久日志，最后在一个事务中删除 record/name 并释放 image pin。任一步失败都保留 `deleting` 供相同命令重试；只有 metadata finalize 成功后 name 和 image pin 才释放。

## 5. 受管路径

默认路径：

```text
/var/lib/kumabox/metadata.db
/var/lib/kumabox/images/...
/var/lib/kumabox/sandboxes/<id>/cow.raw
/run/kumabox/locks/...
/run/kumabox/sandboxes/<id>/...
/var/log/kumabox/sandboxes/<id>/vmm.log
```

data、run、log roots 必须是绝对、互不重叠且不经过非系统 symlink 的路径。
