# KumaBox 性能规范

> 状态：normative

性能优化必须先测量，再修改。完整性校验、状态提交顺序、资源身份验证和失败恢复不能为了基准数字被削弱。

## 对齐方法

Cocoon commit `27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d` 是比较基线。比较时固定：host、kernel、Cloud Hypervisor、镜像内容、CPU、内存、存储、网络模式和缓存冷热状态。记录两边的实际参数，不能依赖不同默认值。

## 当前基准场景

镜像导入：

- 全新导入；
- 全缓存命中；
- 单 layer 损坏修复；
- 多 image 共享 layer；
- manifest 重复 layer；
- 1、2、4、8 worker。

Sandbox 生命周期：

- create sparse COW 与 ext4 格式化；
- start 到 Cloud Hypervisor API Running；
- start 到首次 agent exec；
- stop 到 process/cgroup/runtime 清理完成；
- 100 和 1000 条 metadata 查询。

未来网络阶段增加：CNI ADD、首次出网、stop quiesce、restart recover、CNI DEL 和多 NIC 成本。

## 指标

至少记录 wall time、CPU time、读取/写入字节、hash/解压/EROFS 时间、锁等待、峰值 RSS、goroutine 和 FD 数。microVM 指标必须区分：

```text
process launched → VMM API Running → agent exec ready → workload ready
```

## 当前状态

结构整理期间的镜像 benchmark 因缺少真实 Linux 环境暂缓，尚未提交性能优化。恢复测试环境后先建立基线和 profile；只有可重复收益才修改热路径。

允许的候选方向包括单次导入内 source digest 去重、操作内校验结果复用和减少重复全文件摘要，但最终 digest、diffID、EROFS 与 boot artifact 验证必须保留。
