<p align="center">
  <img src="assets/logo.png" alt="KumaBox logo" width="180">
</p>

<p align="center">
  <a href="README.md">English</a> · <b>简体中文</b>
</p>

<p align="center">
  <a href="https://github.com/kgpp34/KumaBox/actions/workflows/ci.yml"><img src="https://github.com/kgpp34/KumaBox/actions/workflows/ci.yml/badge.svg" alt="CI"></a>
  <img src="https://img.shields.io/badge/Go-1.24.4%2B-00ADD8?logo=go&logoColor=white" alt="Go 1.24.4+">
  <img src="https://img.shields.io/badge/platform-Linux%20amd64%20%7C%20arm64-FCC624?logo=linux&logoColor=black" alt="Linux amd64 | arm64">
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-green.svg" alt="MIT License"></a>
  <a href="https://github.com/kgpp34/KumaBox/stargazers"><img src="https://img.shields.io/github/stars/kgpp34/KumaBox?style=flat-square&logo=github" alt="Stars"></a>
  <a href="https://github.com/kgpp34/KumaBox/network/members"><img src="https://img.shields.io/github/forks/kgpp34/KumaBox?style=flat-square&logo=github" alt="Forks"></a>
</p>

<p align="center">
  <a href="#快速开始">快速开始</a> ·
  <a href="#架构">架构</a> ·
  <a href="#与同类项目的对比">对比</a> ·
  <a href="#路线图">路线图</a>
</p>

AI Agent 会写代码、装依赖、发起网络连接，还会读写没人审过的文件。把这些放在共享内核上跑，
本质上是在赌运气。KumaBox 为每个任务分配一台独立的 **KVM microVM**：独立的内核、独立的磁盘、
独立的网络命名空间。从 OCI 镜像到一个可用的沙箱，只需要一条命令。

> [!NOTE]
> v0.1.0 是单机运行时的首个正式发布版本。项目仍处于 0.x 阶段，升级前请阅读
> Release Notes 和快照兼容性说明。E2B 协议适配仍为实验性能力。

## KumaBox 是什么

<p align="center"><img src="assets/readme/product.svg" alt="谁来驱动 KumaBox、如何驱动、每个沙箱能得到什么" width="100%"></p>

KumaBox 是一个**面向 AI Agent 和不可信工作负载的 microVM 沙箱运行时**。镜像、虚拟机生命周期、
网络、快照、设备和 guest 内命令执行，都由它一站式管理。你面对的是"沙箱"，而不是裸的 VMM。

任何能执行命令的程序现在都能驱动它，例如：
编程 Agent、Agent 框架里的工具调用、一次拉起成千上万次尝试的 RL / 评测框架、CI 任务，
或者坐在终端前的你。

每个沙箱都是一台真正的机器：

- **硬件级隔离。** 每个沙箱运行在 KVM 之上的独立 guest 内核里，每台 VM 对应一个独立的 Cloud Hypervisor 进程。
- **OCI 进，microVM 出。** 按 digest 固定的 OCI 镜像被转换成共享、只读的 EROFS 层，再为每台 VM 叠加一块私有的写时复制磁盘。
- **真实的网络。** 每台 VM 拥有独立的网络命名空间，通过 CNI 使用多队列 TAP 和 tc redirect。支持多网卡，也支持在线调整网卡。
- **无需 SSH 即可执行命令。** `exec` 走 vsock 通道，支持流式 stdout / stderr、stdin、环境变量、工作目录和用户选择，并返回真实的退出码。交互式终端通过 `console` 使用。
- **快照是一等公民。** 支持运行态快照、导出、导入、原地恢复、休眠，以及以全新身份克隆。
- **按需使用真实设备。** 运行中的 Cloud Hypervisor 沙箱可热插拔外部数据盘、virtio-fs 共享目录和 VFIO PCI 设备。
- **为脚本化而生。** 生命周期命令提供 `--json` 输出，`inspect` 返回带缩进的 JSON。

## 架构

<p align="center"><img src="assets/readme/architecture.svg" alt="KumaBox 架构" width="100%"></p>

**轻量的控制面。** 每次调用 `kumabox`，都会打开持久化状态、获取资源锁、执行操作并记录结果。
每台运行中的 VM 由各自独立的 Cloud Hypervisor 进程承载，一个沙箱出问题不会拖垮其他沙箱。

**持久化生命周期。** SQLite 保存沙箱与快照状态。多步骤操作先在私有位置准备产物，
完成后再发布；清理失败时保留资源归属，以便后续重试。

| 路径 | 用途 |
| --- | --- |
| `/var/lib/kumabox` | 镜像、VM 记录、快照、网络租约和内容存储 |
| `/run/kumabox` | PID 文件、API socket、操作锁 |
| `/var/log/kumabox` | VM 与运行时日志 |

## 一次预热，无限分叉

<p align="center"><img src="assets/readme/lifecycle.svg" alt="沙箱生命周期：构建、运行、预热、快照、克隆" width="100%"></p>

Agent 天生就会重试、分支和探索。准备环境的成本只需要付一次：启动、安装依赖、预热缓存。
然后对内存和磁盘打一个**运行态快照**，每次尝试都从它 `clone` 出一台新沙箱。
每个克隆都会获得自己的可写磁盘、网络身份和主机名。Cloud Hypervisor v53 可显式选择按需内存恢复；
更新的兼容版本可以使用写时复制内存恢复。

## 快速开始

需要一台 amd64 或 arm64 的 Linux 主机，能访问 `/dev/kvm`，并具备 root 权限。

Release 提供包含 `kumabox` 和 `kumabox-check` 的 Linux amd64 压缩包，以及
与其版本对应的 Ubuntu guest 镜像。从 [GitHub Release](https://github.com/kgpp34/KumaBox/releases)
下载压缩包和 `SHA256SUMS`，先运行 `sha256sum -c SHA256SUMS`，再解压并安装。
请使用该 Release 指定的 guest 镜像版本；宿主机程序和 guest 镜像应来自同一版本。
下面仍保留源码构建步骤。

```bash
# 1. 构建并安装
git clone https://github.com/kgpp34/KumaBox.git && cd KumaBox
make build
sudo install -m 0755 bin/kumabox /usr/local/bin/kumabox
sudo install -m 0755 bin/kumabox-check /usr/local/bin/kumabox-check

# 2. 一次性准备主机：Cloud Hypervisor、固件、CNI 插件、EROFS 工具
sudo kumabox-check --upgrade
sudo kumabox doctor

# 3. 拉取已发布的 guest 镜像
sudo kumabox image pull ghcr.io/kgpp34/kumabox/ubuntu:24.04

# 4. 启动一个沙箱并与之交互
sudo kumabox run ghcr.io/kgpp34/kumabox/ubuntu:24.04 \
  --name my-vm --cpus 2 --memory 1GiB --storage 10GiB
sudo kumabox exec my-vm -- uname -a

# 5. 一次预热，无限分叉
sudo kumabox snapshot save my-vm --name base
sudo kumabox clone base --name fresh
sudo kumabox net fresh --configure
sudo kumabox exec fresh -- hostname

# 6. 清理
sudo kumabox stop fresh
sudo kumabox stop my-vm
sudo kumabox rm fresh
sudo kumabox rm my-vm
sudo kumabox snapshot rm base
sudo kumabox image remove ghcr.io/kgpp34/kumabox/ubuntu:24.04
```

`stop` 默认请求 ACPI 正常关机，最多等待 30 秒。`stop my-vm --timeout 10`
指定等待秒数，`stop my-vm --force` 跳过正常关机窗口。`--timeout 0` 使用
`sandbox.stop_timeout` 配置，也可以通过 `KUMABOX_SANDBOX_STOP_TIMEOUT=10s` 设置。
guest 不处理 ACPI 或等待超时后，运行时会对确认身份的 VMM 发 TERM，再按
`vmm.cloud_hypervisor.stop_grace` 升级到 KILL。正常关机需要 guest 的电源事件
处理服务已就绪，agent 可执行命令并不代表该服务已经就绪。取消 stop 会保留可重试状态，
不会因取消而继续升级 KILL；需要时可用 `stop --force` 重试。
可写根盘和托管数据盘在 stop/start 之间保留，不需要制作快照；强停仍可能丢失 guest
尚未写入磁盘的数据。恢复、休眠、HTTP 删除及 E2B kill/过期清理显式跳过正常关机窗口，
不会因此增加 30 秒等待。

`clone` 和 `restore` 都支持 `--restore-mode auto|copy|ondemand|mmap`。
默认在已安装的 VMM 和快照内存类型都支持时使用 mmap，否则使用完整复制。
ondemand 按需加载需要显式选择，避免首次访问内存时在运行期配额下出现长尾。
显式选择延迟加载或 mmap 需要兼容的 VMM。大页或共享内存请求 mmap 时降级为完整复制，
ondemand 则要求普通私有内存；恢复前会先检查，
避免因模式不支持而停止原沙箱。恢复时的内存加载暂不受 guest CPU 配额限制，
在恢复 guest 执行前再施加正常配额。

`--from-dir` 的源目录必须保持不可变，使用期间不要覆盖里面的文件。
同一文件系统内通过硬链接共享不可变内存文件，可写磁盘和配置仍然独立；
跨文件系统时会复制内存文件。

`clone` 在虚拟机恢复运行后返回。随后执行 `net SANDBOX --configure`，把新主机名和
已分配的网卡配置应用到 guest；也可以使用 `clone --wait-network`，在返回前完成此步骤。
默认路径会在独立进程中更新 guest 熵和 machine ID。

主机端和 guest 端的产物需要配套使用。对可复现性有要求时，请固定带版本号的 guest 标签
（例如 `24.04-v0.1.0`）或 OCI digest。单独执行 `sudo kumabox-check` 会做一次只读的主机检查。

### Firecracker 后端（开发分支）

Cloud Hypervisor 仍然是默认后端。`feat/firecracker` 分支新增实验性的
Linux amd64 直启后端，实机验证使用官方 Firecracker v1.17.0。
这部分尚未包含在已发布的 v0.1.0 二进制中。需要从当前代码构建主程序，并重新构建
Ubuntu 镜像，让 initramfs 包含 MMIO 驱动，支持 `/dev/vdX` 启动参数。
具体构建命令见[英文 README](README.md#firecracker-backend-development-branch)。

创建或运行时指定 `--vmm firecracker`，后续 `start`、`stop`、`exec`、`ps`、
`inspect`、`logs`、`rm` 自动读取沙箱记录中的后端类型。如果 Firecracker 不在 PATH，
通过 `vmm.firecracker.binary` 或 `KUMABOX_VMM_FIRECRACKER_BINARY` 配置路径，后续命令也要保留该配置。

第一批支持 CNI 网络、每张网卡一对收发队列、创建时的数据盘，以及基于同一 agent 的
文件、进程和端口转发操作。停止再启动会保留根盘和数据盘。guest 中的磁盘按镜像层、COW、
数据盘的顺序排列，没有 virtio serial。共享内存、大页、KSM 和显式 `directio=on` 会被拒绝。
`snapshot save` 会暂停 VM，捕获内存、设备状态和所有受管可写磁盘，然后恢复运行。
`restore 沙箱名 快照名` 可恢复原沙箱的进程内存、根盘和数据盘。`auto`、`mmap` 和
`ondemand` 均使用 Firecracker 原生的私有文件映射；`copy` 会在停止沙箱前被拒绝。
`hibernate` 会在 VM 暂停期间发布快照，再停止 VMM；`restore` 恢复捕获的进程和磁盘。
`clone 快照名 --name 新名字` 使用独立可写盘和私有映射内存，源沙箱可以继续运行，也可以已被删除。
支持快照导出、导入和 `clone --from-dir 目录`，可迁移到另一组数据和运行时目录；目标端需要
相同镜像，以及兼容的 Firecracker 构建、CPU 和宿主机。目录恢复也可以通过 `--force` 重绑定
到资源形状兼容的目标沙箱。克隆必须保留捕获时的 CPU、内存、网卡数量和数据盘集合；
新增设备或改变这些配置会在创建克隆之前被拒绝。
使用 `clone --wait-network` 或 `net 沙箱名 --configure` 应用新的主机名、MAC 和 IP 后再使用网络；
旧版 guest agent 走已有的 exec 回退路径。磁盘路径重定向在私有 Linux 挂载 namespace 内完成，
需要 OverlayFS 以及挂载、chroot 权限，不会在宿主机创建原沙箱路径的占位文件。
`console 沙箱名` 通过私有串口中继连接，一次允许一个客户端；按 `Ctrl+]` 再按 `.` 退出，
之后可用相同命令重连。CLI 退出不会关闭中继，无人连接时也会持续记录串口日志；
VMM 退出后中继自动结束，运行时清理也会回收它。正常停止发送 CtrlAltDel；guest 内核没有相应处理器时，等待超时后终止
进程。本批不承诺 Firecracker 的启动延迟。进程复用现有 cgroup、网络 namespace 隔离和
上游 seccomp。克隆的私有路径视图不作为 jailer 安全边界，暂未集成 jailer。

### CPU 调度与 guest 内存

`create`、`run` 可以单独指定宿主机 CPU 策略，`--cpus` 仍表示 guest 看见的虚拟 CPU 数量：

```bash
sudo kumabox create ghcr.io/kgpp34/kumabox/ubuntu:24.04 --name limited \
  --cpus 2 --cpu-weight 100 --cpu-quota-us 150000 --cpu-period-us 100000 \
  --cpu-burst-us -1 --cpuset-cpus 2-3
```

这个例子把进程限制在宿主机 CPU 2、3 上，每 100ms 可合计使用 150ms CPU 时间，不积攒突发额度。
CPU 编号必须属于宿主机实际允许的集合；目前支持显式指定，不自动绑核。
默认权重等于 vCPU 数量，周期为 100000us，配额等于 vCPU 数量乘周期，突发额度等于配额。
这与之前没有突发额度的默认值不同；需要严格限额时使用 `--cpu-burst-us -1`。
旧内核缺少 burst 支持时，只有默认推导的突发额度允许降级，显式请求正值会报错（禁用 burst 不需要此内核功能）。
`clone` 支持相同的 CPU 参数，未指定时重新计算默认值，不继承源机器的 CPU 编号。
`restore` 保留目标沙箱的 CPU 策略：加载内存时保留权重和 CPU 位置限制，恢复 guest 执行前施加配额和突发限制。

以下内存选项在创建时确定，快照和克隆会保留：

| 参数 | 行为 |
| --- | --- |
| `--hugepages` | 从宿主机 hugetlb 大页池分配 RAM，需要管理员预留足够大页；不足时启动失败。快照内存使用完整复制恢复，显式请求 mmap 也会降级。 |
| `--mergeable` | 将私有 guest RAM 标记为可供 KSM 合并。不能与大页、共享内存同时使用；宿主机 KSM 扫描器由管理员开启和调节，运行时不会自动修改。 |
| `--no-balloon` | 不创建 balloon 设备。默认创建，初始目标回收配置内存的 25%，并开启内存不足时自动放气和空闲页上报。 |

保存快照前，会先让仍在运行的 guest 完成 balloon 回收。连续 2 秒没有进展，或总共超过 1 分钟，
会记录警告并按当前内存继续保存，不会无限等待。这些选项在 CPU 开销、内存密度、恢复速度之间做取舍，
并非打开后一定更快。HTTP 创建接口也支持 `cpu_policy`、`hugepages`、`mergeable`、`no_balloon` 字段。

### 通过 virtio-fs 共享宿主机目录

创建沙箱时必须指定 `--shared-memory`，运行后无法再开启此设置。Ubuntu 24.04 可安装
`virtiofsd`，让它通过 Unix socket 提供共享目录：

```bash
sudo apt-get install virtiofsd
sudo install -d /tmp/kumabox-share
sudo /usr/libexec/virtiofsd --socket-path=/tmp/kumabox-share.sock \
  --shared-dir=/tmp/kumabox-share --cache=never &

sudo kumabox run ghcr.io/kgpp34/kumabox/ubuntu:24.04 \
  --name share-vm --shared-memory
sudo kumabox fs attach share-vm --socket /tmp/kumabox-share.sock --tag data
sudo kumabox fs list share-vm --json
sudo kumabox exec share-vm -- sh -c \
  'mkdir -p /mnt/data && mount -t virtiofs data /mnt/data && echo hello >/mnt/data/hello'
sudo cat /tmp/kumabox-share/hello

sudo kumabox exec share-vm -- umount /mnt/data
sudo kumabox fs detach share-vm --tag data
sudo kumabox stop share-vm
sudo kumabox rm share-vm
```

`fs list` 和 `inspect` 展示当前 VMM 的挂载信息。共享设备只在这次 VM 运行期间有效；
停止或重启后，需要重新启动 `virtiofsd` 并再次 attach。执行 `snapshot save` 或
`hibernate` 前，要先在 guest 内卸载并 detach。

### 操作 guest 文件

以下命令需要运行中的沙箱和更新后的 guest agent；只更新宿主机二进制不会更新已有
guest。文件操作走 vsock，无网卡沙箱也可以使用。`fs attach/detach/list` 仍用于管理
virtio-fs 共享，`file` 管理 guest 文件内容。

```bash
sudo kumabox file mkdir my-vm /tmp/workspace --parents
printf 'hello\n' > /tmp/hello.txt
sudo kumabox file write my-vm /tmp/workspace/hello.txt /tmp/hello.txt --mode 0644
sudo kumabox file ls my-vm /tmp/workspace --json
sudo kumabox file stat my-vm /tmp/workspace/hello.txt
sudo kumabox file read my-vm /tmp/workspace/hello.txt
printf 'updated\n' | sudo kumabox file write my-vm /tmp/workspace/result.txt
sudo kumabox file mv my-vm /tmp/workspace/result.txt /tmp/workspace/final.txt
sudo kumabox file rm my-vm /tmp/workspace --recursive
```

路径必须是规范的 guest 绝对路径。读取只接受普通文件，可以跟随指向普通文件的符号链接；
`stat` 返回链接本身的信息。写入拒绝符号链接目标，先写同目录临时文件，传输完整后再原子替换，
传输未完成时中断会保留原文件。默认保留已有文件的权限，新文件使用 0600，也可用 `--mode` 指定权限；
新目录使用 0700。替换会生成新 inode，不保留旧文件的属主、扩展属性和硬链接关系，
也不保证 guest 崩溃后的 fsync 持久性。

文件分块传输，目录分批返回，不在内存中缓存全部内容。目录按文件系统顺序输出，失败时可能
已经输出部分内容，应检查退出码。上传完成响应丢失时，文件可能已经提交，重试前应检查目标。
递归删除需要显式传入 `--recursive`，不跟随最终符号链接，禁止以根目录为删除目标。
操作使用 guest agent 的权限，也能访问共享进 guest 的挂载点。本批未修改 SDK 和 HTTP API。

在 Linux/KVM 主机上，可针对已导入、使用 systemd 且包含最新 agent 的 Ubuntu 镜像运行端到端测试：

```bash
sudo python3 tests/e2e/guest_operations.py \
  --image my-updated-image --commit "$(git rev-parse --short HEAD)"
```

脚本会创建并清理自己的无网卡、单网卡测试沙箱，检查文件传输、上传取消、权限、目录分批返回、
符号链接、后台进程及 guest `sync` 后强制停止并重启的文件保留。`--commit` 应与镜像内 agent 的构建版本对应。
可用 `--root-dir`、`--run-dir`、`--log-dir` 隔离测试状态，此时须先将镜像导入对应的 root。
报告默认保存到 `/tmp/kumabox-guest-e2e.json`。

### 在 Agent 中调用

`exec` 直接输出 guest 命令的 stdout 和 stderr，并保留 guest 命令的退出码。

```python
import subprocess

def run_in_sandbox(vm: str, script: str) -> subprocess.CompletedProcess[str]:
    proc = subprocess.run(
        ["sudo", "kumabox", "exec", vm, "--", "sh", "-c", script],
        capture_output=True, text=True,
    )
    return proc

result = run_in_sandbox("fresh", "hostname")
print(result.returncode, result.stdout, result.stderr)
```

从同一个预热好的快照并行分叉出多个尝试：

```bash
for i in $(seq 1 8); do
  sudo kumabox clone base --name try-$i &
done
wait
sudo kumabox ps
```

官方的 Ubuntu guest 镜像刻意保持精简。如果需要预装自己的工具链（Python、Node、浏览器等），
可以在 [`oci-images/ubuntu/Dockerfile`](oci-images/ubuntu/Dockerfile) 的基础上扩展。
这个 Dockerfile 已经内置了配套的 `kumabox-agent`、内核和 initramfs。

如果 Linux guest 不依赖可加载的内核模块，可以从同一份源码构建单层快速启动镜像：

```bash
docker build -f oci-images/ubuntu/Dockerfile -t kumabox/ubuntu:24.04 .
docker build -f oci-images/fastboot/Dockerfile.boot -t kumabox/boot:fast .
docker build -f oci-images/fastboot/Dockerfile.image -t kumabox/ubuntu:fast .
docker save kumabox/ubuntu:fast -o /tmp/kumabox-ubuntu-fast.tar
sudo kumabox image import ubuntu-fast /tmp/kumabox-ubuntu-fast.tar --format docker
```

快速镜像使用内建驱动的独立内核和 initramfs；需要内核模块或直通驱动的负载仍使用
通用 Ubuntu 镜像。

## 访问沙箱里的服务

`forward` 通过 vsock 将宿主机回环 TCP 端口转发到沙箱内部的 `127.0.0.1`。
0 网卡沙箱也可以使用。以安装了 Python 的 guest 为例：

```bash
sudo kumabox exec --detach my-vm -- python3 -m http.server 8000 --bind 127.0.0.1
sudo kumabox forward my-vm 8080:8000
# 在宿主机另一个终端执行：
curl http://127.0.0.1:8080/
```

支持 HTTP、WebSocket 和其他 TCP 协议，保留双向半关闭语义。单次 TCP 写入阻塞超过
30 秒会断开连接；没有数据传输的空闲连接不受此超时限制。Ctrl-C 会关闭本地监听及
活动连接，guest 服务继续运行。沙箱停止会断开活动连接，本地监听保留，可在同一沙箱重启后
接受新连接；同名沙箱删除重建后，不会接管原来的转发。

`0:8000 --json` 会分配空闲的本地端口并输出监听地址；`--address ::1` 可监听 IPv6
回环地址。仅允许回环 IP，每条命令最多同时转发 64 个连接。首次输出表示本地监听已建立，
不代表 guest 服务已就绪；单个连接失败会写入 stderr。需要包含新版 agent 的镜像，
只替换宿主机二进制不会升级已有 guest。

包含 Python 3 和对应 agent 的镜像可在 Linux/KVM 测试机运行完整转发测试：

```bash
sudo python3 tests/e2e/port_forward.py --image my-updated-image --commit AGENT_BUILD_ID
```

## 远程 API 与 SDK

`kumabox serve` 将现有服务开放为带令牌认证的 HTTP API，默认只监听
`127.0.0.1:8765`。远程访问请通过 SSH 隧道或 TLS 反向代理：

```bash
sudo sh -c 'umask 077; openssl rand -hex 32 > /etc/kumabox-api.token'
sudo kumabox serve --token-file /etc/kumabox-api.token
# 客户端机器：ssh -L 8765:127.0.0.1:8765 user@kumabox-host
```

`sdk/python` 和 `sdk/typescript` 提供 KumaBox SDK，支持创建、连接、执行命令、
生命周期操作和快照。使用前需先在宿主机导入或拉取镜像。
guest 命令非零退出时会抛出包含输出的 `CommandExitError`；Python 可传
`check=False`、TypeScript 可传 `{ check: false }` 直接读取退出码。
Python 包可用 `python -m pip install ./sdk/python` 安装；TypeScript 包先执行
`npm ci --prefix sdk/typescript && npm run build --prefix sdk/typescript`，
再从 `./sdk/typescript` 安装到应用中。

```python
from kumabox import Client

client = Client(token="YOUR_API_TOKEN")
sandbox = client.create("my-image")
print(sandbox.commands.run("uname -a").stdout)
sandbox.stop()
sandbox.kill()
```

另外提供**实验性的 E2B 协议适配**，支持创建、连接、查询、列表、删除、快照、
前台命令、普通文件读写、metadata、环境变量默认值、TTL 及暂停恢复。
将 `E2B_API_URL` 与 `E2B_SANDBOX_URL` 指向同一个 API 地址，`E2B_API_KEY`
设为服务端令牌。`templateID` 对应本地镜像或已有快照；默认 `Sandbox.create()`
需要先把镜像导入为 `base`。这仍然**不是完整 E2B SDK 兼容**，本地 CLI 的进程管理
和端口转发尚未接入该适配层。已验证的协议版本、具体支持范围和限制见
[E2B 适配说明](api/e2b/README.md)。

## 常用命令

| 领域 | 命令 |
| --- | --- |
| VM 生命周期 | `run`、`create`、`start`、`stop`、`rm`、`ps`、`inspect` |
| Guest 访问 | `exec`、`console`、`logs`、`forward`、`reseed`、`process ls/kill/logs/attach` |
| Guest 文件 | `file ls/stat/read/write/mkdir/rm/mv` |
| 镜像 | `image pull`、`image import`、`image inspect`、`image ls`、`image verify`、`image remove` |
| 快照 | `snapshot save`、`snapshot ls`、`snapshot inspect`、`snapshot export`、`snapshot import`、`snapshot rm`、`restore`、`clone`、`hibernate` |
| 网络与设备 | `net`、`disk attach/detach`、`fs attach/detach/list`、`device attach/detach` |
| 运维 | `status`、`gc`、`daemon`、`doctor`、`version` |

完整参数以 `kumabox <command> --help` 为准。

## 与同类项目的对比

<p align="center"><img src="assets/readme/comparison.svg" alt="KumaBox、CubeSandbox 与 E2B 的设计选择" width="100%"></p>

[E2B](https://github.com/e2b-dev/infra) 和
[CubeSandbox](https://github.com/TencentCloud/CubeSandbox) 都是非常优秀的项目。
它们和 KumaBox 目标一致：为每个 Agent 任务提供独立的内核。KumaBox 在以下几个方面走了不同的路线：

- **VM 原生，而不是"套了 VM 的容器"。** 沙箱是真正的虚拟机，完整继承 Cloud Hypervisor 的设备模型：热插拔磁盘、virtio-fs 共享目录、在线调整网卡，以及面向 GPU 等加速卡的 VFIO PCI 直通。
- **快照可以带走。** 运行态快照是一个可校验、可移植的包：导出、拷贝到另一台主机、导入，再从它克隆。
- **分层镜像，磁盘共享。** OCI 层被转换成只读 EROFS 镜像，由同一主机上的所有 VM 共享。每台 VM 只为自己的写时复制数据付出存储成本。
- **安装极简。** 只需一个 Go 二进制，加上 Cloud Hypervisor 和 CNI 插件。元数据存放在内嵌的 SQLite 中，不需要额外运维数据库。
- **正确性可审计。** 明确的状态迁移、操作锁和分阶段发布，覆盖生命周期、网络、快照与克隆等路径。
- **MIT 许可**，同时支持 amd64 和 arm64。

相关项目：[Kata Containers](https://katacontainers.io/)、
[gVisor](https://gvisor.dev/)、
[Firecracker](https://firecracker-microvm.github.io/)、
[Cloud Hypervisor](https://www.cloudhypervisor.org/)、
[Cocoon](https://github.com/cocoonstack/cocoon)。

## 愿景

每一次 Agent 行动都应该拥有一台用完即弃的计算机：像 git 分支一样便宜地分叉，像独立机器一样安全。
KumaBox 自底向上构建这一目标。先在每台主机上做好一个正确、崩溃一致的运行时，
再基于同一套状态机和元数据，往上构建常驻服务和多节点控制面。
这样，同一个沙箱在单台服务器上和整个集群中的行为是一致的。

## 路线图

> 规划方向，欢迎在 Issue 中参与讨论。

- [x] OCI 转 EROFS 镜像、CNI 网络、基于 vsock 的 guest 命令执行
- [x] 运行态快照、克隆、恢复、休眠、导出与导入
- [x] 热插拔磁盘、virtio-fs 和 VFIO PCI 设备
- [x] 带认证的 HTTP API 与运行状态回收
- [ ] 多节点控制面与调度
- [x] 仓库内 Python、TypeScript SDK
- [ ] 独立版本的 Python、TypeScript SDK 包发布
- [x] 实验性 E2B 协议适配，明确已支持的兼容范围
- [ ] 补齐其余 E2B 协议兼容能力
- [ ] MCP server，让 Agent 以工具的形式创建和操作沙箱
- [ ] 预热池，以及公开的克隆延迟基准测试
- [ ] 按沙箱粒度的出网策略

## 构建与测试

```bash
git clone https://github.com/kgpp34/KumaBox.git && cd KumaBox
make build
make test
go vet ./...
./bin/kumabox version --json
```

VM 启动、guest 网络以及快照克隆的端到端验证需要 Linux/KVM 主机。

README 中的配图由代码生成。修改 `assets/readme/src/` 下的脚本后，
运行 `python3 assets/readme/src/build.py` 即可重新生成。

## 安全模型

- KumaBox 在工作负载外增加了一层虚拟机边界，但 VMM、KVM、guest 内核、固件、镜像和 agent 仍然属于可信计算基。
- 主机初始化会修改特权网络和系统配置。运行 `--fix` 或 `--upgrade` 之前，请先审阅 `scripts/kumabox-check.sh`。
- VFIO 会把物理设备直接交给 guest，需要正确的 IOMMU 分组；使用不当可能影响主机的稳定性和隔离性。
- 快照的兼容性取决于主机架构、Cloud Hypervisor 版本、VM 配置和快照类型。

可复现的 Bug 和安全问题请通过 Issue 反馈。请不要在公开报告中附带密钥、私有镜像或生产环境快照。

## 许可证

KumaBox 基于 [MIT 许可证](LICENSE) 开源。
