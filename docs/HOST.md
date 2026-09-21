# 主机要求与环境自检

> 状态：normative

## 开发与静态测试

macOS 和 Linux 都能运行 `make verify`、`make race` 和 `make lint`。不需要 root、KVM 或外部 VMM。测试使用临时目录、SQLite、真实本地 socket 和 fake formatter。

## 真实运行

Sandbox 启动当前只支持 Linux。需要：

- `/dev/kvm` 可用且当前用户有权限；
- cgroup v2，并允许在配置的 parent 下创建 scope、写 CPU 控制和迁移进程；
- Cloud Hypervisor 可执行文件；
- `mkfs.erofs` 1.8 或更新版本；
- `mkfs.ext4`；
- 支持 pidfd 和 vsock 的内核；
- 足够的 data、run、log 目录权限。

`kumabox doctor` 检查当前主机。普通检查只读；`--fix` 和 `--upgrade` 是显式的宿主修改授权。

```bash
sudo kumabox doctor
sudo kumabox doctor --fix
sudo kumabox doctor --upgrade
```

## Guest 镜像

可启动 OCI 镜像必须：

- 含 `/boot/vmlinuz*` 与 `/boot/initrd.img*` regular files；
- 声明 `io.kumabox.boot.profile=overlay-v1`；
- initramfs 能识别 `kumabox.layers`、`kumabox.cow` 和相应 virtio disk serial；
- 启动 `kumabox-agent` 并监听 guest vsock port 1024，才能使用 `exec`。

参考构建位于 [`oci-images/ubuntu`](../oci-images/ubuntu/README.md)。

## 网络阶段的新增要求

网络尚未实现。接入 CNI 时将增加：

- CNI plugin binaries，默认 `/opt/cni/bin`；
- 至少一份 conflist，默认 `/etc/cni/net.d`；
- 创建持久 netns、TAP、veth 和 TC redirect 的权限；
- CNI ADD/DEL 所需的宿主 sysctl 与防火墙配置。

这些检查必须先进入 `doctor`，再开放网络 flags，避免命令接受参数后才静默降级。
