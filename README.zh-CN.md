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

> [!WARNING]
> KumaBox 仍在快速迭代中。CLI、元数据格式和快照格式暂不承诺向后兼容。
> 在首个稳定版发布之前，请在可随时重建的 Linux/KVM 主机上使用。

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
- **无需 SSH 即可执行命令。** `exec` 走 vsock 通道，支持流式 stdout / stderr、stdin、环境变量、工作目录、TTY，并返回真实的退出码。
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
每个克隆都会获得自己的可写磁盘、网络身份和主机名。Cloud Hypervisor v53 使用按需内存恢复；
更新的兼容版本可以使用写时复制内存恢复。

## 快速开始

需要一台 amd64 或 arm64 的 Linux 主机，能访问 `/dev/kvm`，并具备 root 权限。

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
sudo kumabox exec fresh -- hostname

# 6. 清理
sudo kumabox stop fresh
sudo kumabox stop my-vm
sudo kumabox rm fresh
sudo kumabox rm my-vm
sudo kumabox snapshot rm base
sudo kumabox image remove ghcr.io/kgpp34/kumabox/ubuntu:24.04
```

主机端和 guest 端的产物需要配套使用。对可复现性有要求时，请固定带版本号的 guest 标签
（例如 `24.04-v0.1.0`）或 OCI digest。单独执行 `sudo kumabox-check` 会做一次只读的主机检查。

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

## 常用命令

| 领域 | 命令 |
| --- | --- |
| VM 生命周期 | `run`、`create`、`start`、`stop`、`rm`、`ps`、`inspect` |
| Guest 访问 | `exec`、`console`、`logs`、`reseed` |
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
- [ ] 带 HTTP API 的 daemon 模式
- [ ] 多节点控制面与调度
- [ ] Go、Python、TypeScript SDK
- [ ] E2B 兼容 API，让现有 E2B 代码可以直接切换到 KumaBox
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
