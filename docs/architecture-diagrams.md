# KumaBox 当前架构图

## 命令调用

```text
                 immutable Config
                       │
cmd/kumabox → cli ─────┴────► core application service
                                 │
               ┌─────────────────┼─────────────────┐
               ▼                 ▼                 ▼
             images           sandbox             vmm
        source / erofs       catalog / disk    cloudhypervisor
               │                 │                 │
               └──────────┬──────┘                 ├── cgroup
                          ▼                        └── agent/vsock
                    metadata/sqlite
```

## Sandbox 创建

```text
validate request
      ↓
sandbox entity lock
      ↓
resolve + pin image in transaction
      ↓
reserve creating record
      ↓
prepare sparse ext4 COW
      ↓
verify disk → CAS created
```

## 启动和停止

```text
start:
lock → CAS starting → runtime/cgroup → launch VMM → persist identity
     → vm.info Running → CAS running

stop:
lock → CAS stopping → vm.shutdown → identity-safe TERM → optional KILL
     → verify absent → runtime/cgroup cleanup → CAS stopped
```

## Guest exec

```text
CLI streams ─► core validates Running generation
                    ↓
            Cloud Hypervisor vsock UDS
                    ↓  CONNECT 1024
               guest agent
              ┌─────┴─────┐
       stdin frames    stdout/stderr/exit
```

## 计划中的网络顺序

```text
reserve sandbox
      ↓
persist NIC intents
      ↓
netns → CNI ADD → TAP/TC → persist MAC/IP
      ↓
launch VMM in netns
      ↓
stop: quiesce ── start: recover/unquiesce ── rm: CNI DEL + netns delete
```
