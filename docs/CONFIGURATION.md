# KumaBox 配置

> 状态：normative

每次命令解析一个独立配置快照。优先级从高到低：

```text
显式 flag > KUMABOX_* 环境变量 > --config 指定文件 > 默认值
```

未传 `--config` 时不会搜索当前目录、用户目录或 `/etc`。配置文件按扩展名支持 YAML、JSON 和 TOML；未知字段或无效值立即失败。

## 根 flags

| Flag | 配置键 | 默认值 |
|---|---|---|
| `--root-dir` | `paths.data` | `/var/lib/kumabox` |
| `--run-dir` | `paths.run` | `/run/kumabox` |
| `--log-dir` | `paths.log` | `/var/log/kumabox` |
| `--config` | — | 不读取文件 |

## 配置键

| 键 | 环境变量 | 默认值 |
|---|---|---|
| `paths.data` | `KUMABOX_PATHS_DATA` | `/var/lib/kumabox` |
| `paths.run` | `KUMABOX_PATHS_RUN` | `/run/kumabox` |
| `paths.log` | `KUMABOX_PATHS_LOG` | `/var/log/kumabox` |
| `images.erofs_binary` | `KUMABOX_IMAGES_EROFS_BINARY` | `mkfs.erofs` |
| `images.parallelism` | `KUMABOX_IMAGES_PARALLELISM` | `min(4, host CPUs)` |
| `images.layer_size` | `KUMABOX_IMAGES_LAYER_SIZE` | 8 GiB |
| `images.unpacked_size` | `KUMABOX_IMAGES_UNPACKED_SIZE` | 16 GiB |
| `images.boot_size` | `KUMABOX_IMAGES_BOOT_SIZE` | 512 MiB |
| `images.archive_size` | `KUMABOX_IMAGES_ARCHIVE_SIZE` | 32 GiB |
| `metadata.busy_timeout` | `KUMABOX_METADATA_BUSY_TIMEOUT` | `50ms` |
| `metadata.retry_limit` | `KUMABOX_METADATA_RETRY_LIMIT` | `5s` |
| `sandbox.ext4_binary` | `KUMABOX_SANDBOX_EXT4_BINARY` | `mkfs.ext4` |
| `sandbox.cleanup_timeout` | `KUMABOX_SANDBOX_CLEANUP_TIMEOUT` | `10s` |
| `vmm.default` | `KUMABOX_VMM_DEFAULT` | `cloud-hypervisor` |
| `vmm.cgroup_parent` | `KUMABOX_VMM_CGROUP_PARENT` | `/sys/fs/cgroup/kumabox.slice` |
| `vmm.cloud_hypervisor.binary` | `KUMABOX_VMM_CLOUD_HYPERVISOR_BINARY` | `cloud-hypervisor` |
| `vmm.cloud_hypervisor.startup_timeout` | `KUMABOX_VMM_CLOUD_HYPERVISOR_STARTUP_TIMEOUT` | `10s` |
| `vmm.cloud_hypervisor.stop_grace` | `KUMABOX_VMM_CLOUD_HYPERVISOR_STOP_GRACE` | `5s` |
| `vmm.cloud_hypervisor.abort_grace` | `KUMABOX_VMM_CLOUD_HYPERVISOR_ABORT_GRACE` | `3s` |

## YAML 示例

```yaml
paths:
  data: /srv/kumabox/data
  run: /run/kumabox
  log: /srv/kumabox/log
images:
  parallelism: 4
metadata:
  busy_timeout: 100ms
  retry_limit: 5s
vmm:
  cgroup_parent: /sys/fs/cgroup/kumabox.slice
  cloud_hypervisor:
    binary: /usr/local/bin/cloud-hypervisor
    startup_timeout: 15s
```

模块不能直接读取这些环境变量。新增运行策略必须先决定它是稳定协议常量，还是进入本配置结构的部署策略。
