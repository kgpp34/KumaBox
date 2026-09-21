# S2 OCI Linux 手动验收

状态：2026-09-15 项目负责人已完成测试机验证并明确审批 S2 通过。按 DEC-022，本阶段不启动 VMM、不需要 KVM。

## 准备

需要 Go、mkfs.erofs >= 1.8、fsck.erofs、jq 和一份包含真实 regular kernel/initrd 的 KumaBox-compatible OCI 镜像。alpine 等普通容器镜像通常不满足启动合同。

```bash
make verify
make lint
make build
export PATH="$PWD/bin:$PATH"
mkfs.erofs --version
kumabox doctor
export KUMABOX_S2_REFERENCE='填写真实兼容镜像引用'
KUMABOX_S2_WORK=$(mktemp -d /var/tmp/kumabox-s2.XXXXXX)
kb() { kumabox --root-dir "$KUMABOX_S2_WORK/data" --run-dir "$KUMABOX_S2_WORK/run" --log-dir "$KUMABOX_S2_WORK/log" "$@"; }
kb image ls --json
```

空列表必须是 `[]`。本节只写隔离的临时 root；系统 doctor 的 fix/upgrade 由负责人在验收机上显式运行。

## 真转换、registry 与幂等

```bash
kb image pull "$KUMABOX_S2_REFERENCE" --platform linux/amd64
kb image inspect "$KUMABOX_S2_REFERENCE" > "$KUMABOX_S2_WORK/first.json"
kb image verify "$KUMABOX_S2_REFERENCE"
find "$KUMABOX_S2_WORK/data/images/layers" -name '*.erofs' -exec fsck.erofs '{}' \;
find "$KUMABOX_S2_WORK/data/images" -type f -exec sha256sum '{}' \; | sort > "$KUMABOX_S2_WORK/before.sha256"
kb image pull "$KUMABOX_S2_REFERENCE" --platform linux/amd64
kb image inspect "$KUMABOX_S2_REFERENCE" > "$KUMABOX_S2_WORK/second.json"
find "$KUMABOX_S2_WORK/data/images" -type f -exec sha256sum '{}' \; | sort > "$KUMABOX_S2_WORK/after.sha256"
diff "$KUMABOX_S2_WORK/before.sha256" "$KUMABOX_S2_WORK/after.sha256"
diff "$KUMABOX_S2_WORK/first.json" "$KUMABOX_S2_WORK/second.json"
test ! -e "$KUMABOX_S2_WORK/data/images/blobs"
test -z "$(find "$KUMABOX_S2_WORK/data/staging/imports" -mindepth 1 -print -quit)"
```

每个 source layer 只有一份 EROFS；重复 pull 不出现转换进程，产物、digest 和 created_at 不变。inspect 包含 compressed source digest、EROFS digest、boot candidates 的 digest/size 与最终选择。

## Layout/archive 与别名

```bash
kb image import tiny ./testdata/oci-layout --platform linux/amd64
kb image verify tiny
KUMABOX_S2_ARCHIVE="$KUMABOX_S2_WORK/tiny.bin"
tar -C testdata/oci-layout -czf "$KUMABOX_S2_ARCHIVE" .
kb image import tiny-alias "$KUMABOX_S2_ARCHIVE" --platform linux/amd64
kb image inspect tiny | jq '.names'
kb image rm tiny
kb image verify tiny-alias
kb image rm tiny-alias
```

gzip 通过 magic 检测，与扩展名无关。fixture 启动文件是占位数据，只验证转换/完整性；不能用来启动 VM。实际 Ubuntu 等镜像另外验证 versioned boot basename、whiteout 和 arm64 gzip kernel。

## 损坏与恢复

针对真实已拉取镜像：

```bash
KUMABOX_S2_LAYER=$(kb image inspect "$KUMABOX_S2_REFERENCE" | jq -r '.boot.kernel_layer | sub("^sha256:"; "")')
KUMABOX_S2_KERNEL=$(kb image inspect "$KUMABOX_S2_REFERENCE" | jq -r '.boot.kernel_file')
printf x >> "$KUMABOX_S2_WORK/data/images/boot/sha256/$KUMABOX_S2_LAYER/$KUMABOX_S2_KERNEL"
kb image verify "$KUMABOX_S2_REFERENCE"; test "$?" -eq 5
kb image pull "$KUMABOX_S2_REFERENCE" --platform linux/amd64
kb image verify "$KUMABOX_S2_REFERENCE"
```

verify 报 ARTIFACT_CORRUPT；重拉只重建损坏 layer，恢复原摘要。缺失文件报 ARTIFACT_UNAVAILABLE（6），缺镜像为 NOT_FOUND（3），用法错误为 2。

## 并发、取消与崩溃

使用另一个空 root 对同一镜像并发 pull 两次；两进程都成功，允许 staging 重复转换，最终只有一份 EROFS。两边都 verify，通过后删除检查共享引用。

对空 root 中慢下载/大镜像 import 发送 SIGINT/SIGTERM：进程终止下载和 mkfs.erofs，staging 清理完毕，没有半成品 image 记录。原样重试成功。

在转换期间和发布/提交窗口分别 kill -9：inspect 要么 NOT_FOUND，要么完整且 verify 通过；绝不显示 importing/半成品。重试时未知最终文件重建；已提交产物校验后复用。kill -9 的旧 staging 可以保留为不可见孤儿，后续 GC 阶段回收。

最后记录宿主架构、mkfs.erofs 版本、镜像完整 manifest digest、各命令退出码与产物文件列表，附到 ROADMAP 的进度日志。不要把 cache、bin 或 coverage 产物加入 Git；本目录中的规范和 runbook 应随代码提交。
