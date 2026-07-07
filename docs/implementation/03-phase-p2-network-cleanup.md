# P2: 基础网络与主机资源清理

## 1. 阶段目标

第三阶段让 VM 稳定获得网络，并保证 stop/delete/gc 能清理宿主网络资源。P2 的目标不是复杂网络功能，而是“可达 + 可回收 + 可诊断”：

```text
kumabox run ubuntu --name p2-net --network default
kumabox inspect p2-net --json
kumabox network inspect p2-net --json
kumabox stop p2-net
kumabox delete p2-net
kumabox gc --dry-run --json
```

P2 推荐先实现 provider 化的 host-tap + managed bridge/NAT，再接 CNI provider。这里和 Cocoon 保持相同的大方向：VM record 只保存 backend 启动所需的网络快照，network provider index 保存 lease、cleanup pending 和 provider-specific 状态。区别是 KumaBox 第一版默认自己管理 `kumabox0` 和 NAT，Cocoon 的 bridge provider 则要求外部 bridge 已存在。

## 当前进度

| 小节 | 状态 | 说明 |
| --- | --- | --- |
| P2-01 Network Config 与 Capability | done | 配置、provider 接口、root 权限检查、iptables/nft 能力 |
| P2-02 Tap/MAC/IP Allocator | done | tap name、MAC、IP lease 分配、恢复和冲突检测 |
| P2-03 Host-Tap Provider 与 Bridge/NAT | done | managed bridge、gateway、NAT、DNS 基础配置 |
| P2-04 Cloud Hypervisor Net Renderer | done | 从 VM networkConfigs 注入 virtio-net |
| P2-05 Network Provider Index 与 Inspect | done | 持久化 tap/MAC/IP/DNS/cleanup/provider 状态 |
| P2-05b Network E2E Smoke | done | 启动真实 VM 并验证 host-to-guest 网络连通 |
| P2-06 Stop/Delete Cleanup | done | provider Delete、tap、lease、NAT ref，失败写 pending |
| P2-07 GC Pending Cleanup | done | dry-run 报告 pending cleanup、stale tap、orphan lease；retry 后置 |
| P2-08 CNI Provider | todo | libcni/命令调用 ADD/DEL，和 host-tap 共享 provider 边界 |

## 2. 测试环境假设

P2 验收仍在 Linux/KVM VM 中执行，但需要额外宿主权限：

- 当前用户可通过 root 或 sudo 创建 tap/bridge。
- `ip` 命令可用。
- `iptables` 或 `nft` 至少一种可用。
- Linux kernel 支持 tuntap。
- 如果启用 CNI，`cni_config_dir` 和 `cni_bin_dir` 可读，必要 plugin 存在。
- 网络验收可以访问 gateway；外网 ping 可能受环境限制，因此脚本应区分 gateway 可达和 external 可达。

默认测试目录仍使用：

```text
/tmp/kumabox-p0/
  data/
  fixtures/
  logs/
  run/
```

## 3. 交付物

| 类型 | 路径或命令 | 说明 |
| --- | --- | --- |
| CLI | `kumabox run IMAGE --network default` | 创建网络并启动 VM |
| CLI | `kumabox run IMAGE --network none` | 显式无网络 |
| CLI | `kumabox network ls` | 列出 VM 网络资源 |
| CLI | `kumabox network inspect VM --json` | 查看 tap/MAC/IP/cleanup/drift |
| Data | `data/network/index.json` | network provider 状态、cleanup pending |
| Data | `data/network/leases.json` | host-tap IP lease |
| Data | `data/network/host-tap.json` | managed bridge/NAT owner 与 ref 状态 |
| VM record | `networkConfigs` | VM 启动 backend 所需的 tap/MAC/queue/IP 快照 |
| GC | `kumabox gc --dry-run --json` | 报告 pending network cleanup |
| 验收脚本 | `scripts/linux/verify-network.sh` | P2 可执行验收 |

脚本要求：

- 支持 `--sudo` 或检测当前是否 root。
- 失败时输出 `ip link`、`ip addr`、`ip route`、iptables/nft 摘要。
- 清理时只删除 KumaBox 拥有的 tap/bridge/lease，不碰用户网络。

## 4. 配置与目录布局

配置：

```toml
[network]
mode = "host-tap"
default = "default"
bridge = "kumabox0"
cidr = "10.88.0.0/16"
gateway = "10.88.0.1"
dns = ["1.1.1.1", "8.8.8.8"]
tap_prefix = "kbtap"
nat_backend = "auto"
cni_config_dir = "/etc/cni/net.d"
cni_bin_dir = "/opt/cni/bin"
```

新增目录：

```text
data/
  network/
    index.json
    index.lock
    leases.json
    leases.lock
    host-tap.json
    host-tap.lock
```

`data/network` 是 network module 的事实目录。CNI 只是 provider 之一，不应该把所有网络状态命名为 `data/cni`。

## 5. 数据模型

### 5.1 Provider 边界

P2 的网络实现必须先有 provider 边界，避免 host-tap、CNI、bridge、后续 netns 模式互相污染。接口语义对齐 Cocoon：

```go
type Provider interface {
    Type() string
    Prepare(ctx context.Context, vmID string, vm VMRecord) (netnsPath string, err error)
    Add(ctx context.Context, vmID string, vm VMRecord, specs ...AddSpec) ([]NetworkConfig, error)
    Remove(ctx context.Context, vmID string, indices ...int) error
    Delete(ctx context.Context, vmIDs []string) ([]string, error)
    Inspect(ctx context.Context, vmID string) (NetworkInspect, error)
    List(ctx context.Context) ([]NetworkInspect, error)
    RegisterGC(gc *Orchestrator)
}
```

`AddSpec` 需要支持 `Existing *NetworkConfig`。这是恢复路径：如果 VM record 已经保存 tap/MAC/IP，`start` 或 repair 应尽量复用这些身份重建 host 侧 plumbing，而不是重新分配导致 guest 配置和宿主状态不一致。

### 5.2 Network Provider Index

`data/network/index.json`：

```json
{
  "schemaVersion": "kumabox.network.index.v1",
  "networks": {
    "net_xxx": {
      "id": "net_xxx",
      "vmId": "kb_xxx",
      "network": "default",
      "provider": "host-tap",
      "ifName": "eth0",
      "tap": "kbtapabc123",
      "mac": "02:00:00:12:34:56",
      "numQueues": 1,
      "queueSize": 256,
      "bridgeDev": "kumabox0",
      "netnsPath": "",
      "ips": ["10.88.0.12/16"],
      "gateway": "10.88.0.1",
      "dns": ["1.1.1.1"],
      "cleanup": {
        "pending": false,
        "reason": "",
        "lastAttemptAt": ""
      },
      "createdAt": "2026-06-29T00:00:00Z",
      "updatedAt": "2026-06-29T00:00:00Z"
    }
  }
}
```

### 5.3 VM Record Network Configs

VM record 中同步保存 backend 启动所需的 network configs。这个结构要尽量贴近 Cocoon 的 `NetworkConfig`：backend renderer 只依赖 VM record，不反查 provider index。

```json
{
  "networkConfigs": [
    {
      "id": "net_xxx",
      "tap": "kbtapabc123",
      "mac": "02:00:00:12:34:56",
      "numQueues": 1,
      "queueSize": 256,
      "backend": "host-tap",
      "bridgeDev": "kumabox0",
      "netnsPath": "",
      "network": {
        "ip": "10.88.0.12",
        "gateway": "10.88.0.1",
        "prefix": 16,
        "dns": ["1.1.1.1"]
      }
    }
  ]
}
```

VM record 里的 network config 是启动快照，不是 cleanup 的唯一事实源。provider index 和 lease store 才负责 delete、pending cleanup 和 GC。

### 5.4 Lease 与 Host-Tap Owner

`data/network/leases.json`：

```json
{
  "schemaVersion": "kumabox.network.leases.v1",
  "cidr": "10.88.0.0/16",
  "leases": {
    "10.88.0.12": {
      "vmId": "kb_xxx",
      "mac": "02:00:00:12:34:56",
      "tap": "kbtapabc123",
      "createdAt": "2026-06-29T00:00:00Z"
    }
  }
}
```

`data/network/host-tap.json`：

```json
{
  "schemaVersion": "kumabox.network.hostTap.v1",
  "bridge": "kumabox0",
  "cidr": "10.88.0.0/16",
  "gateway": "10.88.0.1",
  "natBackend": "iptables",
  "owner": {
    "kind": "kumabox",
    "rootDir": "/tmp/kumabox-p0/data"
  },
  "refCount": 1,
  "createdAt": "2026-06-29T00:00:00Z",
  "updatedAt": "2026-06-29T00:00:00Z"
}
```

## 6. 命令范围

| 命令 | 作用 |
| --- | --- |
| `kumabox run IMAGE --network default` | 使用默认网络启动 |
| `kumabox run IMAGE --network none` | 不创建网络设备 |
| `kumabox network ls [--json]` | 从 VM store 和 network provider index 汇总 |
| `kumabox network inspect VM --json` | 查看单 VM 网络 |
| `kumabox inspect VM --json` | 增加 network 字段 |
| `kumabox stop VM` | 停 VMM，不释放网络身份 |
| `kumabox delete VM` | 调用 provider Delete，释放 tap/lease/pending cleanup |
| `kumabox gc --dry-run --json` | 报告 stale tap、pending cleanup、orphan lease |

建议策略：

- `stop` 默认保留 network provider index 记录和 lease，便于 `start` 后网络身份稳定。
- `delete` 删除网络资源，并更新 network provider index。
- `--network none` 的 VM 不创建 network provider 记录，VM record 中 networkConfigs 为空。
- `start` 遇到已有 networkConfigs 时走 recover/recreate 路径，优先复用 MAC/IP。

## 7. 任务拆解

### P2-01: Network Config 与 Capability

目标：先把宿主能力检查、配置加载和 provider 边界做稳定。

交付物：

- network config loader。
- network provider interface 和 provider resolver。
- doctor/env-check 增加 tuntap、ip、iptables/nft、sudo 能力检查。
- `kumabox network ls --json` 空结果。
- VM record 预留 `networkConfigs` 字段。
- `scripts/linux/verify-network-config.sh` 验收脚本。

当前状态：已完成。

已实现范围：

- `internal/config` 增加 `[network]` 默认配置和校验。
- `internal/network` 增加 provider 常量、provider resolver、network config 数据结构、capability helper 和 `data/network/index.json` 读取骨架。
- `internal/vmstore.VMRecord` 增加 `networkConfigs`，为后续 P2-02/P2-04 写入 tap/MAC/IP 快照预留位置。
- `kumabox network ls [--json]` 和 `kumabox network inspect VM [--json]` 已可读取 network provider index；index 不存在时返回空结果。
- `kumabox doctor --json` 增加 `networkProvider`、`networkTun`、`networkIPCommand`、`networkNAT`、`networkPermission` 检查。
- `scripts/linux/env-check.sh --network` 增加 Linux 网络能力检查，但默认不启用，避免影响 P0/P1 旧验收脚本。

验收：

```bash
kumabox doctor --json
kumabox network ls --json
scripts/linux/verify-network-config.sh \
  --kumabox ./bin/kumabox \
  --cloud-hypervisor cloud-hypervisor \
  --qemu-img qemu-img \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs \
  --sudo
```

通过标准：

- 无 root 权限时给出 `NETWORK_PERMISSION_DENIED` 或明确 suggestion。
- 不存在 `ip` 命令时返回 `IPROUTE2_MISSING`。
- 网络功能不可用不影响 `--network none` VM。
- provider 未配置时 `--network default` 返回明确 `NETWORK_PROVIDER_NOT_CONFIGURED`。
- `network ls --json` 在没有 `data/network/index.json` 时输出 `[]`。
- `network inspect kb_missing --json` 输出 `vmId` 和空 `interfaces`。
- P2-01 不创建 tap、bridge、NAT rule，也不启动 VM。

### P2-02: Tap/MAC/IP Allocator

目标：生成不冲突的 tap、MAC 和 IP lease，并支持恢复已有身份。

交付物：

- tap name allocator。
- locally administered MAC generator。
- IP lease store。
- lease lock。
- `AddSpec.Existing` 恢复路径。
- `scripts/linux/verify-network-allocator.sh` 验收脚本。

当前状态：已完成。

已实现范围：

- `internal/network.Allocator` 支持分配 tap、MAC、IP lease，并返回 provider record 与 VM `networkConfigs` 快照。
- tap name 使用 VM ID 和 NIC index 派生，满足 Linux IFNAMSIZ 长度限制。
- MAC 使用 locally administered unicast 地址。
- IP lease 写入 `data/network/leases.json`，并用 `data/network/leases.lock` 做排他锁。
- allocator 会跳过 gateway 和已被其他 VM 占用的 IP。
- recover 路径支持 `Existing *network.Config`，优先复用已有 tap/MAC/IP。
- recover 遇到 IP 已被其他 VM lease 占用时返回 `ErrLeaseConflict`。
- `ReleaseIP` 支持释放 lease，为 P2-06 delete cleanup 预留能力。

验收：

```bash
scripts/linux/verify-network-allocator.sh \
  --kumabox ./bin/kumabox \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs
```

通过标准：

- tap name 长度满足 Linux IFNAMSIZ 限制。
- MAC 具备 locally administered bit，不复用已有 VM MAC。
- IP 不复用 active lease。
- lease store 写入 `data/network/leases.json`。
- recover 时复用 VM record 中已有 MAC/IP；复用失败必须中止，不得静默分配新身份。
- P2-02 不创建宿主 tap、bridge、NAT rule，也不启动 VM。

### P2-03: Host-Tap Provider 与 Bridge/NAT

目标：实现默认 host-tap provider，创建和维护 KumaBox owned bridge/gateway/NAT。

交付物：

- `kumabox0` bridge ensure。
- gateway IP ensure。
- NAT rule ensure。
- idempotent setup。
- owner/conflict 检测。
- provider rollback。
- `kumabox network setup --json`。
- `kumabox network teardown --json`。
- `scripts/linux/verify-hosttap-network.sh` 验收脚本。

当前状态：已完成。

已实现范围：

- Linux 上 `EnsureHostTap` 创建或确认 KumaBox managed bridge。
- bridge 默认是 `kumabox0`，gateway 默认是 `10.88.0.1/16`。
- setup 会执行 `ip link add ... type bridge`、`ip addr add`、`ip link set up`、`sysctl net.ipv4.ip_forward=1`。
- NAT backend 支持 `auto`、`iptables`、`nft`、`none`；`auto` 优先选择 `iptables`，否则选择 `nft`。
- setup 写入 `data/network/host-tap.json`，记录 bridge、CIDR、gateway、NAT backend、owner root-dir。
- 如果 bridge 已存在但没有当前 root-dir 的 KumaBox owner state，setup 返回 `ErrNetworkConflict`，不接管用户网络。
- setup 可重复执行，不重复创建 bridge 或 NAT rule。
- teardown 只删除当前 root-dir owner state 对应的 bridge/NAT；无 owner state 时 no-op；owner 不匹配时失败。
- 非 Linux 平台提供 stub，明确返回 host-tap 需要 Linux。

限制：

- P2-03 只管理 bridge/gateway/NAT owner，不创建 VM tap。
- 真正的 VM tap 创建和挂 bridge 会在后续 provider Add / renderer 阶段接入。

验收：

```bash
scripts/linux/verify-hosttap-network.sh \
  --kumabox ./bin/kumabox \
  --cloud-hypervisor cloud-hypervisor \
  --qemu-img qemu-img \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs \
  --sudo
```

通过标准：

- `network setup --json` 创建或确认 `kumabox0`。
- `ip link show dev kumabox0` 成功。
- `ip -4 addr show dev kumabox0` 包含 `10.88.0.1/16`。
- `data/network/host-tap.json` 存在并记录 owner。
- 重复 setup 不重复创建冲突 rule。
- bridge 已存在但非 KumaBox owner 时返回 `NETWORK_CONFLICT`。
- NAT backend 自动选择 iptables 或 nft，并在 inspect/doctor 中展示。
- `network teardown --json` 删除 owner state 对应的 bridge/NAT，并移除 `host-tap.json`。

### P2-04: Cloud Hypervisor Net Renderer

目标：从 VM record 的 networkConfigs 把 tap 注入 Cloud Hypervisor config。

交付物：

- CH net config renderer。
- guest `eth0` 对应 tap。
- cidata network-config 使用 MAC 匹配生成 guest 网络配置。
- `create/run --network default` 最小接入链路。
- `scripts/linux/verify-network-render.sh` 验收脚本。

当前状态：已完成。

已实现范围：

- `create/run` 增加 `--network none|default`，默认 `none`，避免 P0/P1 旧脚本突然需要 root。
- `--network default` 时执行 host-tap 接入：ensure `kumabox0`、分配 tap/MAC/IP、创建 tap、挂到 `kumabox0`、写 provider index、写 VM `networkConfigs`。
- Cloud Hypervisor config 增加 `nets` 字段，并渲染 `--net tap=...,mac=...,num_queues=...,queue_size=...`。
- NoCloud cidata `network-config` 在存在 network configs 时按 MAC 写静态 IP/gateway/DNS。
- create 渲染失败时回滚 provider record、tap 和 IP lease。
- 非 Linux 平台 `--network default` 明确失败，不影响 `--network none`。

限制：

- P2-04 只验证 VM 创建阶段的 tap attach 和 config render，不启动 VM。
- delete/stop 自动释放 tap/lease 会在 P2-06 完整实现；当前 P2-04 验收脚本会手动清理本次创建的 tap/bridge/state。

验收：

```bash
scripts/linux/verify-network-render.sh \
  --kumabox ./bin/kumabox \
  --cloud-hypervisor cloud-hypervisor \
  --qemu-img qemu-img \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs \
  --root-disk /tmp/kumabox-p0/fixtures/jammy-server-cloudimg-amd64.img \
  --firmware /tmp/kumabox-p0/fixtures/CLOUDHV.fd \
  --name p2-render \
  --sudo
```

通过标准：

- `cloud-hypervisor.json` 包含 net 设备。
- CH renderer 只读 VM record networkConfigs，不反查 provider index。
- network provider index 在 ADD 成功后立即落盘，VM record 同步写入 networkConfigs。
- tap 存在且 master 是 `kumabox0`。
- cidata `network-config` 包含 MAC 匹配、静态 IP、gateway。
- render 失败释放 tap/lease。

### P2-05: Network Provider Index 与 Inspect

目标：网络事实可观测。

交付物：

- `data/network/index.json`。
- VM record `networkConfigs`。
- `kumabox network inspect`。
- `inspect --json` network 字段。
- events log。

当前状态：已完成。

已实现范围：

- `kumabox network inspect VM --json` 支持 VM name、VM id 和 id prefix。
- inspect 输出同时包含 provider index 的 `interfaces` 和 VM record 的 `vmConfigs`。
- `kumabox inspect VM --json` 增加只读 `networkStatus` 字段。
- provider index 与 VM record 不一致时输出 `drift`，不自动修复。
- `scripts/linux/verify-network-inspect.sh` 验证正常 inspect 与人为 drift 检测。

验收：

```bash
scripts/linux/verify-network-inspect.sh \
  --kumabox ./bin/kumabox \
  --cloud-hypervisor cloud-hypervisor \
  --qemu-img qemu-img \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs \
  --root-disk /tmp/kumabox-p0/fixtures/jammy-server-cloudimg-amd64.img \
  --firmware /tmp/kumabox-p0/fixtures/CLOUDHV.fd \
  --name p2-inspect \
  --sudo
```

通过标准：

- 输出 tap、MAC、IP、gateway、DNS、cleanup 状态。
- network provider 记录缺失时返回明确 `NETWORK_NOT_CONFIGURED`，但不得从 run dir 猜测唯一状态。
- VM stale 时 network inspect 不误删资源。
- provider index 和 VM record 不一致时 inspect 标记 `drift`，不自动修复。
- 脚本人为修改 provider index 后，`drift` 报告 `mac mismatch`。

### P2-05b: Network E2E Smoke

目标：在没有 guest agent/exec 的前提下，验证 VM 启动后 guest 静态 IP 对宿主机可达。

交付物：

- `scripts/linux/verify-network-e2e.sh`。

验证边界：

- 该脚本启动真实 microVM。
- 它验证 guest cloud-init 应用静态 IP、virtio-net 设备连通、tap 挂到 `kumabox0`、host-to-guest ICMP 可达。
- 它不验证 guest 主动访问外网；这需要 guest agent/exec，或后续专门的 user-data 注入能力。
- 脚本退出时会调用 delete/network teardown 做收尾清理；delete 负责释放 VM 级 tap/lease/provider record，network teardown 负责移除全局 `kumabox0`。

验收：

```bash
scripts/linux/verify-network-e2e.sh \
  --kumabox ./bin/kumabox \
  --cloud-hypervisor cloud-hypervisor \
  --qemu-img qemu-img \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs \
  --root-disk /tmp/kumabox-p0/fixtures/jammy-server-cloudimg-amd64.img \
  --firmware /tmp/kumabox-p0/fixtures/CLOUDHV.fd \
  --name p2-e2e \
  --sudo
```

通过标准：

- VM 进入 `running`。
- `network inspect` 的 `drift` 为 0。
- tap 存在且 master 是 `kumabox0`。
- host 能 ping 通 VM record 中分配的 guest IP。
- 失败时脚本打印 console log tail 和 network inspect，便于定位 boot、cloud-init 或 host 网络问题。

### P2-06: Stop/Delete Cleanup

目标：delete VM 时清理宿主网络资源。

交付物：

- tap delete。
- lease release。
- provider Delete，包括 CNI DEL 或 host-tap cleanup。
- cleanup failure -> pending。

验收：

```bash
scripts/linux/verify-network-cleanup.sh \
  --kumabox ./bin/kumabox \
  --cloud-hypervisor cloud-hypervisor \
  --qemu-img qemu-img \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs \
  --root-disk /tmp/kumabox-p0/fixtures/jammy-server-cloudimg-amd64.img \
  --firmware /tmp/kumabox-p0/fixtures/CLOUDHV.fd \
  --name p2-cleanup \
  --sudo
```

通过标准：

- delete 后 tap 不存在。
- lease 从 leases.json 删除。
- cleanup 失败不删除 network provider 记录，而是标记 pending。
- delete 可重复执行。
- stop 不释放 lease，不删除 provider record。

### P2-07: GC Pending Cleanup

目标：GC 能发现网络残留。

交付物：

- stale tap scanner。
- orphan lease scanner。
- pending cleanup report。
- 可选 `gc --retry-cleanup` 后置，不进入最小验收。

验收：

```bash
scripts/linux/verify-network-gc.sh \
  --kumabox ./bin/kumabox \
  --cloud-hypervisor cloud-hypervisor \
  --qemu-img qemu-img \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs \
  --root-disk /tmp/kumabox-p0/fixtures/jammy-server-cloudimg-amd64.img \
  --firmware /tmp/kumabox-p0/fixtures/CLOUDHV.fd \
  --name p2-gc \
  --sudo
```

通过标准：

- active running VM 的 tap 不出现在候选。
- pending cleanup 输出 component=`network`、path/name、reason。
- VM store 或 leases 读取失败时 GC fail-closed。
- 读到 provider index 和 VM record drift 时只报告，不猜测删除。

### P2-08: CNI Provider

目标：在 host-tap provider 稳定后接入 CNI provider。CNI provider 的边界和 host-tap provider 一致，但 provider-specific 状态可以存放在 `data/network/index.json` 的记录中。

交付物：

- CNI ADD/DEL wrapper。
- CNI result parser。
- CNI lease/metadata 写入 network provider index。
- 可选 per-VM netns 路径字段。
- CNI error event。

验收：

```bash
kumabox run ubuntu --name p2-cni --network cni:default
kumabox network inspect p2-cni --json
kumabox delete p2-cni --force
```

通过标准：

- ADD 成功后立即写 network provider index 和 VM record networkConfigs。
- DEL 失败写 pending cleanup。
- clone/restore 后不复用 MAC/IP 的策略在 VM record 和 network provider index 中可表达。
- 如果 CNI 未配置，Delete/Inspect/List 尽量仍可根据 provider index 工作；Add 必须明确失败。

## 8. 非目标

- 不做 multiqueue 性能调优，但 VM record 预留 `numQueues`/`queueSize` 字段。
- 不做 vhost-user-net。
- 不做复杂 port mapping。
- 不做多网络挂载。
- 不做 network policy。
- 不做 guest agent readiness。

## 9. Cocoon 注意事项

Cocoon 的网络经验说明：网络最难的问题不是第一次连通，而是失败清理和身份复用。

- CNI DEL 失败不能吞错。
- ADD 成功后 network provider index 必须立即落盘，否则 CLI 崩溃后无法清理。
- VM record 中的 networkConfigs 必须足够渲染 Cloud Hypervisor，不依赖 run dir 里的临时文件。
- recover/recreate 网络时必须优先复用 VM record 的 MAC/IP，避免 guest cidata 与 host lease 分裂。
- clone/restore 默认不能复用 MAC/IP。
- tap、bridge queue size、GRO、txqueuelen 属于后期性能调优，不进入 P2。
- GC 不能在读不全 VM/network 状态时删除网络资源。

## 10. Linux 验收脚本

新增：

```text
scripts/linux/verify-network.sh
```

流程：

```text
1. env-check --strict with network checks
2. ensure image ubuntu exists, or import fixture
3. run ubuntu --network default
4. inspect network fields from VM record and provider index
5. assert tap exists on host
6. optionally assert guest console/cloud-init sees eth0
7. stop VM
8. delete VM
9. assert tap gone and lease released
10. gc --dry-run has no pending network cleanup
```

示例：

```bash
scripts/linux/verify-network.sh \
  --kumabox ./bin/kumabox \
  --cloud-hypervisor cloud-hypervisor \
  --root-dir /tmp/kumabox-p0/data \
  --run-dir /tmp/kumabox-p0/run \
  --log-dir /tmp/kumabox-p0/logs \
  --image ubuntu \
  --name p2-net \
  --sudo
```

## 11. 通过标准

P2 整体通过条件：

- `--network default` VM 能成功创建 tap 并启动。
- `inspect` 和 `network inspect` 输出 tap/MAC/IP。
- delete VM 后宿主 tap 和 lease 被清理。
- 手动制造 cleanup 失败时，network provider index 进入 pending，GC dry-run 能报告。
- `--network none` 仍可启动 VM。
- start/recover 不重新分配已有 VM 的 MAC/IP。
- P1 image 引用和 P0 lifecycle 验收不回归。

## 12. P2 完成后的不变量

- VM 网络资源必须有 owner。
- ADD 成功后必须有 network provider 记录和 VM record networkConfigs。
- DEL 失败必须可恢复，不允许静默丢失。
- delete 是释放网络资源的主路径。
- GC 只在读全 VM/network/image 状态后报告候选。
- clone/restore 后的网络身份策略必须默认为 new，不默认为 pinned。
- run dir 不保存唯一网络状态；provider index 和 VM record 是事实来源。
