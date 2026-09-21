# S3 Sandbox Linux 验收

本 runbook 验证当前无网络生命周期：create、start、exec、console、stop、restart 和 rm。需要 Linux、KVM、cgroup v2、Cloud Hypervisor、`mkfs.erofs`、`mkfs.ext4`、jq，以及一份带 `overlay-v1` profile 和 `kumabox-agent` 的真实镜像。

## 准备

```bash
make verify
make lint
make build
make agent
export PATH="$PWD/bin:$PATH"
export KUMABOX_S3_IMAGE='填写真实兼容镜像引用'
KUMABOX_S3_WORK=$(mktemp -d /var/tmp/kumabox-s3.XXXXXX)
kb() { sudo kumabox --root-dir "$KUMABOX_S3_WORK/data" --run-dir "$KUMABOX_S3_WORK/run" --log-dir "$KUMABOX_S3_WORK/log" "$@"; }

sudo kumabox doctor
kb image pull "$KUMABOX_S3_IMAGE" --platform linux/amd64
kb image verify "$KUMABOX_S3_IMAGE"
test "$(kb image inspect "$KUMABOX_S3_IMAGE" | jq -r '.boot.profile')" = overlay-v1
```

## Create 和查询

```bash
kb create "$KUMABOX_S3_IMAGE" --name lifecycle --cpus 2 --memory 1GiB --storage 10GiB --json \
  | tee "$KUMABOX_S3_WORK/create.json"
KUMABOX_S3_ID=$(jq -r '.id' "$KUMABOX_S3_WORK/create.json")
test "$(jq -r '.state' "$KUMABOX_S3_WORK/create.json")" = created
kb ps -a
kb inspect lifecycle | jq -e --arg id "$KUMABOX_S3_ID" '.id == $id and .state == "created"'
test "$(stat -c %s "$KUMABOX_S3_WORK/data/sandboxes/$KUMABOX_S3_ID/cow.raw")" = 10737418240
blkid "$KUMABOX_S3_WORK/data/sandboxes/$KUMABOX_S3_ID/cow.raw" | grep 'TYPE="ext4"'
```

COW apparent size 为 10 GiB，实际占用应明显更小。`ps` 默认不显示 created，`ps -a` 显示完整 UUID 和列标题。

## Start、exec 和 console

```bash
kb start lifecycle --json | tee "$KUMABOX_S3_WORK/start.json"
test "$(jq -r '.state' "$KUMABOX_S3_WORK/start.json")" = running
kb exec lifecycle -- uname -a
kb exec lifecycle -- hostname
echo hello | kb exec -i lifecycle -- cat
kb exec lifecycle -- sh -c 'exit 17'; test "$?" -eq 17
kb console lifecycle
```

console 中确认 guest 完成启动；使用 `Ctrl-]` 后 `.` 断开。检查：

```bash
cat "$KUMABOX_S3_WORK/run/sandboxes/$KUMABOX_S3_ID/process.json" | jq .
cat "$KUMABOX_S3_WORK/run/sandboxes/$KUMABOX_S3_ID/cmdline"
test -S "$KUMABOX_S3_WORK/run/sandboxes/$KUMABOX_S3_ID/api.sock"
test -S "$KUMABOX_S3_WORK/run/sandboxes/$KUMABOX_S3_ID/vsock.uds"
cat "$KUMABOX_S3_WORK/log/sandboxes/$KUMABOX_S3_ID/vmm.log"
```

## Stop、持久数据和 restart

```bash
kb exec lifecycle -- sh -c 'echo persisted >/persist-check'
kb stop lifecycle --json | tee "$KUMABOX_S3_WORK/stop.json"
test "$(jq -r '.state' "$KUMABOX_S3_WORK/stop.json")" = stopped
test ! -e "$KUMABOX_S3_WORK/run/sandboxes/$KUMABOX_S3_ID"
kb stop lifecycle
kb start lifecycle
kb exec lifecycle -- cat /persist-check | grep -Fx persisted
kb stop lifecycle
```

stop 后 VMM 进程不存在、runtime dir 被清理、对应 cgroup 为空并删除。第二次 stop 幂等成功。restart 保持相同 sandbox ID、image digest 和 COW 数据。

## 删除与引用

```bash
kb image rm "$KUMABOX_S3_IMAGE"; test "$?" -eq 4
kb rm lifecycle --json | tee "$KUMABOX_S3_WORK/remove.json"
test "$(jq -r '.id' "$KUMABOX_S3_WORK/remove.json")" = "$KUMABOX_S3_ID"
test ! -e "$KUMABOX_S3_WORK/data/sandboxes/$KUMABOX_S3_ID"
kb image rm "$KUMABOX_S3_IMAGE"
test "$(kb ps -a --json)" = "[]"
```

## 恢复与安全边界

至少验证：

1. start 过程中发送 SIGINT，重试 start 能收敛为唯一 VMM；
2. running 时 kill VMM，stop 不向复用 PID 的无关进程发信号；
3. stop 在 TERM 等待阶段中断，重试继续 `stopping`；
4. rm 清理中断，重试继续 `deleting`；
5. 破坏 process.json 的 boot ID、binary 或 socket 后，stop 返回冲突且不发送信号；
6. agent 未启动时 exec 有界失败，VMM 保持可 inspect/stop；
7. 非 TTY 重定向无 ANSI 控制字符，JSON 保持缩进。

记录 commit、内核、架构、Cloud Hypervisor、cgroup mode、formatter 版本、完整 image digest、每个命令退出码和失败后的持久状态。
