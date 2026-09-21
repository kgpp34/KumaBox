# KumaBox 架构改造计划

> 状态：**archived**（结构整理记录；R8 完成后不再决定主线功能顺序）
>
> 形成日期：2026-09-18
>
> 代码基线：KumaBox `3766f75`
>
> 对照基线：Cocoon `27ae1e0b2a65c9082c7a1b33c5245bfe43a4854d`，guest agent `v0.2.3`
>
> 相关：包边界见 `ARCHITECTURE.md`；已批准决策见 `DECISIONS.md`；功能阶段见 `ROADMAP.md`
>
> 2026-09-21：项目负责人决定 R8 完成后回到功能主线；后续以 `ROADMAP.md` 和逐功能评审为准。R6 性能实测仍等待 Linux 环境，不因归档而视为已完成。

## 1. 目的与执行规则

这轮改造只处理现有代码的结构、抽象、配置、错误、性能与测试质量，不增加新的用户功能。目标是在保留当前 CLI 行为、持久化数据和 guest 协议的前提下，使代码职责清晰、可复用、可扩展，并避免为了尚不存在的需求提前制造接口和包。

后续实施必须遵守以下规则：

1. 严格按本文第 3 节顺序逐节推进，同一时间只改一节。
2. 每节写代码前，先向项目负责人说明具体文件、类型、调用流程、兼容性和验收方法；得到明确确认后再实施。
3. 每节单独提交。提交前执行该节列出的测试以及 `make verify`；并发代码还要执行 `make race`。
4. 重构不得改变 CLI 参数、JSON 字段、metadata schema、状态机、磁盘布局或 guest 协议，除非该节明确列出并再次获得批准。
5. 不引入 `internal`、通用 `pkg`、`utils`、`common` 或按声明种类划分的包。
6. `types` 只保存跨模块共享的数据和值对象，不保存接口、CLI flag 语义、存储编码或具体实现。
7. 接口放在拥有该能力的模块，或者真实消费该能力的包中；不为了“以后也许有第二个实现”提前拆接口。
8. 文件围绕完整职责拆分。禁止为了一个结构体、一个接口或一两个方法单独建文件。
9. 关键导出类型、字段、方法和复杂流程补充英文注释；涉及顺序、锁或提交边界时在对应源码附近放简短 ASCII 图。
10. 对照实现只是事实来源，不是必须复制的规范。安全性、类型安全或可维护性更好的现有设计应保留。

## 2. 已确定的边界

以下结论已经评审，不在实施时重新讨论：

- 保留根级 `core` 作为应用服务与具体适配器的组装层；CLI 不直接编排跨模块业务流程。
- 保留 `core.SandboxService` 作为 sandbox 应用服务，不创建 `lifecycle`、`console` 或 `exec` 等小包。
- 保留当前 `vmm.Backend` 基础接口。它描述每个 VMM 后端都必须提供的进程级能力，不立即拆成多个小接口。
- snapshot、pause、restore 等未来能力只有在真实实现出现时才增加可选接口。
- `disk`、`vmm`、`images`、`metadata` 等包拥有各自能力；`types` 不收集这些接口。
- `cmd/kumabox` 只保留进程入口；命令树、参数解析和展示继续位于 `cli` 及其子包。
- 不降低镜像内容校验强度来换取速度。性能优化必须保留 digest、diffID 和最终产物验证。
- Agent 当前的一连接一 goroutine 模型可以保留；最大 session 数不是本轮的前置改造。

## 3. 实施顺序

```text
R1 错误链正确性
  ↓
R2 集中配置与显式 VMM Registry
  ↓
R3 SandboxService 内部重组
  ↓
R4 types 与 CLI 边界清理
  ↓
R5 公共 CLI 进度渲染
  ↓
R6 镜像导入性能测量与优化
  ↓
R7 VMM、Agent、CLI 测试补强
  ↓
R8 文档与 CI 治理
```

R1–R5 是行为保持型重构。R6 必须先建立基准再决定具体优化。R7、R8 在前面结构稳定后收口。

## 4. R1：错误链正确性

### 4.1 现状

`errdefs.Error` 提供稳定错误码、资源、提交状态和原因，能力比对照实现依赖哨兵错误与 `fmt.Errorf` 的方式更完整，应当保留。

当前 `errdefs.Context` 在收到已经分类的 `*errdefs.Error` 时，把整个旧错误再次包装进 `Cause`。外层格式化时会重复输出错误码，例如：

```text
ARTIFACT_UNAVAILABLE: ... ARTIFACT_UNAVAILABLE: ...
```

`Retry` 字段目前没有稳定的写入者或消费方，公共语义不成立。

### 4.2 改造

- 调整 `errdefs.Context`：保留已有 `Class`、`Code`、`Entity`、`Committed` 等分类，只在原因链中增加一次上下文，不把完整已格式化的 `Error` 再嵌进去。
- `Error` 分开保存“用于展示的直接原因”和“供 `errors.Is/As` 遍历的原始错误树”。已有分类被重新加上下文时，展示原因从旧分类的 `Cause` 开始；unwrap 仍指向传入的完整错误，从而保留原分类、哨兵和并列 cleanup/unlock/report 错误。
- 对 `errors.Join` 做显式处理：展示时把命中的旧分类节点替换为它的直接原因，并保留其他非空分支；遍历时保留原始 join 树。不能通过截取 `Error()` 字符串或匹配错误文本实现。
- 非空的新 `operation/entity/phase/action` 覆盖旧值，空值保留旧值；`Committed` 继续只能从 false 前进到 true。
- 明确 `Error()`、`Unwrap()`、`errors.Is` 和 `errors.As` 的合同。
- 删除 `Retry` 字段。全仓核对确认它只有声明，没有生产者、消费者或持久化用途；重试建议继续由 `Action` 和状态机语义表达。
- 保留各模块使用 `%w` 添加局部上下文的方式，禁止通过字符串匹配错误。

### 4.3 不做

- 不改现有错误码名称和 CLI 退出码。
- 不退回只有哨兵错误的模型。
- 不引入第三方错误框架。

### 4.4 验收

- 新增 `errdefs/error_test.go`，覆盖 nil、未分类错误、单层分类、多层 `Context`、空字段继承和 `Committed` 单向变化。
- 多层 `Context` 只打印一次分类码，原 `Error` 不被修改。
- joined classified + cleanup/unlock 场景中，分类码、主原因和并列错误各打印一次。
- `errors.Is` 仍能匹配原始原因、原分类错误和 join 中的并列错误；`errors.As` 返回最外层最新上下文。
- `CodeOf` 对普通、嵌套和 joined 错误保持稳定。
- `cli/root_test.go` 验证各错误码的退出码映射没有变化。
- 已提交与未提交错误的 CLI 映射保持不变。
- `go test ./errdefs ./core ./cli/...` 与 `make verify` 通过。

### 4.5 实施记录

- 2026-09-18，提交 `19f8456`：分离诊断 cause 与完整 unwrap 树，删除无消费者的 `Retry`，joined 并列错误保持可见和可匹配。
- 证据：定向测试、完整 `make verify`（race、双平台 vet、build）和双平台 `make lint` 全部通过，lint 为 0 issue。
- 遗留：无；R1 完成。下一节为 R2，开始前仍需单独确认配置来源、结构和 Registry 方案。

## 5. R2：集中配置与显式 VMM Registry

### 5.1 现状与对照

对照实现使用顶层 `config.Config` 汇总目录、VMM 二进制、超时、并发、网络和 cgroup 参数，并采用“默认值 → 配置文件 → 环境变量 → flag → Validate”的加载顺序。这一职责划分合理。

KumaBox 的运行策略目前散落在 CLI、`core`、`vmm/cloudhypervisor`、Agent 和镜像代码中。`core` 的 VMM factory 使用包级可变注册表，不利于测试隔离，也会让未来多后端装配依赖隐式初始化顺序。

### 5.2 目标结构

增加根级 `config` 包。配置按现有模块分组，不建立一层只有转发作用的 profile 或 settings 类型：

```go
type Config struct {
    Paths    storage.Roots
    Images   Images
    Metadata Metadata
    Sandbox  Sandbox
    VMM      VMM
}
```

职责固定为：

- `Paths`：复用 `storage.Roots`，保存 data、run、log 根目录，不复制第二套路径值类型。
- `Images`：`mkfs.erofs` 路径、导入并发度，以及 layer/unpacked/boot/archive 大小上限。
- `Metadata`：SQLite busy timeout 和整个写事务重试预算。
- `Sandbox`：`mkfs.ext4` 路径和失败补偿 cleanup timeout。
- `VMM`：默认后端、cgroup parent，以及 Cloud Hypervisor binary、startup timeout、stop grace、abort grace。

这些值保留在各模块的 `Options` 中执行；`config.Config` 只汇总和校验，`core` 显式转换。例如 `config.Images` 转成 `images.Options` 与 `erofs.Options`，`config.Metadata` 转成 `sqlite.Options`。模块不能反向读取全局配置。

配置流：

```text
defaults
   ↓
config file（若本节批准支持）
   ↓
environment
   ↓
CLI flags
   ↓
Config.Validate
   ↓
core.New(...)
   ↓
各模块 Options
```

配置加载建议采用 Cobra + Viper，但不使用 Viper 的包级全局实例：

- 每次 `cli.Execute` 创建独立 `viper.New()`，测试和多次进程内调用互不污染。
- `--config FILE` 是唯一配置文件入口；不从当前目录或用户目录隐式搜索，避免 root CLI 意外读取陌生配置。
- 文件按扩展名支持 YAML、JSON 和 TOML；显式文件缺失、不可读或字段非法都立即失败。
- 环境变量使用 `KUMABOX_` 前缀，层级中的点转换成下划线，例如 `vmm.cloud_hypervisor.binary` 对应 `KUMABOX_VMM_CLOUD_HYPERVISOR_BINARY`。
- 现有 `--root-dir`、`--run-dir`、`--log-dir` 保持；本节新增 `--config`。其他运行参数先通过配置文件或环境变量提供，避免根命令堆积低频 flags。
- 不支持热重载。每条 daemonless CLI 命令加载、校验一次，随后使用不可变的 `Config` 快照。

优先级固定为：显式 flag > 环境变量 > 显式配置文件 > 默认值。未指定 `--config` 时不读文件，不报缺失错误。

VMM 注册改为不可变的显式实例：

```go
backend, err := cloudhypervisor.New(...)
registry, err := vmm.NewRegistry(backend)
service := core.NewSandboxService(..., registry)
```

`Registry` 与 `Backend` 放在现有 `vmm/backend.go`，不为一个注册表单独制造小文件。构造函数一次性拒绝 nil backend、非法类型、类型重复或类型不匹配，之后只提供按 `types.VMMType` 查询和数量检查；不暴露运行期 `Register`，也没有包级全局 map。Registry 不负责编排 start/stop。

`core/vmm.go` 保留具体适配器装配职责：从 `Config` 创建 cgroup manager 和 Cloud Hypervisor driver，再构造 Registry。未来增加 Firecracker时只在这里追加具体构造，不修改 Registry 和 SandboxService。

### 5.3 边界

- 稳定协议值继续作为对应包常量，例如 boot profile、kernel 参数名、metadata schema version。
- 可部署策略进入配置，例如二进制路径、用户可感知的超时、并发数和资源上限。
- guest agent vsock 端口、NDJSON frame 上限、hybrid-vsock reply 上限、EROFS block size、ext4 magic、Cloud Hypervisor API response 上限、探测轮询间隔和 cgroup CFS period 保持模块常量；它们是协议、安全边界或内部算法，不是部署配置。
- `config` 不 import `core`、CLI 或具体 adapter；`core` 将配置转换为各模块 Options。
- 本节不引入反射式 DI 框架，继续使用显式构造函数。
- 不为旧的 `core.OpenImages(ctx, roots)`、`core.OpenSandbox(ctx, roots, ...)` 保留兼容 wrapper；全仓调用和测试直接迁移为显式 `Config`，避免两套装配入口。
- 本节需要新增 Viper 依赖；必须使用实例 API，禁止 global Viper、`init()` 和隐式注册。

### 5.4 验收

- 所有运行策略硬编码都有“保留为协议常量”或“迁移到配置”的明确归属。
- 两个独立 Registry 测试实例互不影响。
- 缺少、重复或未知 VMM 后端均返回稳定错误。
- 配置测试覆盖 defaults、文件、环境变量、flag 四层优先级，显式缺失文件、非法 duration、负数/零上限和路径重叠。
- 两次 `cli.Execute` 使用不同环境和 flags 时没有跨调用配置泄漏。
- 配置文件与环境变量能实际传到 images、SQLite、disk、cgroup 和 Cloud Hypervisor 构造器，不只停留在 DTO。
- 默认 CLI 行为与当前版本一致。
- 配置表驱动测试、Registry 测试、现有 CLI 集成测试、`make verify` 和 `make lint` 通过。

### 5.5 实施记录

- 2026-09-19，提交 `bc8a559`：增加调用级 `config.Config` 与显式 `--config`，固定优先级为 flag、环境变量、显式文件、默认值；CLI 每次执行使用独立 Viper 实例。
- `core.OpenImages` 与 `core.OpenSandbox` 只接受验证后的配置快照，并把 image limits、并发度、SQLite 超时、ext4 formatter、cleanup timeout、cgroup parent 和 Cloud Hypervisor lifecycle 参数显式传给各模块 Options。
- 删除包级 VMM factory map，增加构造后不可变的 `vmm.Registry`；构造时拒绝 nil、typed nil、非法和重复 backend，查询时区分损坏的持久化类型与本机缺少的 backend。
- 证据：配置四层优先级、未知字段、非法值、重叠路径、CLI 调用隔离、配置到 image adapter 的集成测试，以及 Registry 和各 adapter Options 测试通过；完整 `make verify` 和 Linux/Darwin `make lint` 均通过，lint 为 0 issue。
- R2 完成。下一节为 R3；开始前需要单独确认 `SandboxService` 的具名依赖与文件重组方案。

## 6. R3：SandboxService 内部重组

### 6.1 现状与对照

对照实现把大量 create/start/stop/remove 编排放在 `cmd/vm` 和宽 `Hypervisor` 接口中。KumaBox 当前的 `CLI → core.SandboxService → 模块` 依赖方向更清楚，应当保留。

问题在于 `core/sandbox.go` 已同时容纳服务定义、依赖、查询、存储生命周期、VMM 生命周期、console 和 exec，阅读与修改成本过高；构造函数位置参数也过多。

### 6.2 文件组织

只在 `core` 包内按完整职责重组，不增加新包：

```text
core/
  sandbox.go           SandboxService、Dependencies、构造与公共查找
  sandbox_storage.go   create、remove、磁盘与 metadata 补偿
  sandbox_runtime.go   start、stop、恢复、console、exec
```

若实际代码规模表明两个文件即可表达完整职责，应减少文件，而不是机械采用三个文件。

### 6.3 依赖构造

使用具名依赖结构替换过长的位置参数：

```go
type SandboxDependencies struct {
    Catalog  SandboxCatalog
    Images   ImageCatalog
    Disks    disk.Store
    VMMs     *vmm.Registry
    Cgroups  cgroup.Manager
    // 仅列 SandboxService 真正消费的能力。
}
```

接口仍遵循消费方所有原则。若某接口只被 `core.SandboxService` 消费，可以继续定义在 `core`；若它就是某模块稳定公开的能力，则使用模块自己的接口，避免同一能力出现两份近似合同。

### 6.4 流程约束

重组不得改变以下顺序：

```text
实体锁
  ↓
重读并校验 generation/state
  ↓
持久化操作意图
  ↓
执行宿主副作用
  ↓
校验真实结果
  ↓
generation-fenced 最终提交
  ↓
释放实体锁
```

`console` 和长时间 `exec` 不得在数据转发期间持有实体操作锁。

### 6.5 验收

- 公开的 `SandboxService` 行为和方法保持兼容。
- 不出现 `lifecycle`、`console`、`exec` 新包，也不出现单声明文件。
- 构造依赖可从一个文件完整看出。
- create/remove/start/stop/console/exec 原有测试全部通过，`make race` 通过。

### 6.6 实施记录

- 2026-09-19，提交 `a31d591`：`SandboxService` 改用包内具名依赖对象，同一 sandbox catalog 只注入一次，构造时统一校验 adapter、默认 VMM 与 cleanup policy。
- `core/sandbox.go` 只保留服务定义、组装和查询；存储生命周期进入 `sandbox_storage.go`，运行生命周期、console 和 exec 进入 `sandbox_runtime.go`，没有增加新包。
- 测试按相同职责拆分，并增加依赖缺失、默认 reporter、ID 生成器和时钟的构造测试；锁、generation、提交与补偿顺序保持原测试覆盖。
- `make race`、完整 `make verify` 及 Linux/Darwin `make lint` 全部通过，lint 为 0 issue。R3 完成，下一节 R4 实施前需单独确认类型与 CLI 边界方案。

## 7. R4：types 与 CLI 边界清理

### 7.1 原则

`types` 表达可跨模块传递、持久化或稳定共享的领域事实。CLI 负责命令语法、flag、终端和展示。应用层请求只有在多个模块确实共享时才进入 `types`。

### 7.2 改造

- 移除共享类型校验错误中的 `--cpus`、`--memory`、`--storage` 等 flag 名称。
- 共享类型返回领域错误，例如 `CPU count must be positive`；CLI 将它映射为具体 flag 错误。
- CLI 将 `KEY=VALUE` 解析成 `map[string]string`，业务层与 Agent 不解析 CLI 字符串。
- CLI 根据是否传递 stdin 表达交互输入，不向领域层传递 `Interactive` 布尔语义。
- exec 使用中性命令模型：

```go
type Command struct {
    Args []string
    Env  map[string]string
}
```

- 只有在 `core`、Agent client 或其他模块共同消费 `Command` 时才把它放进 `types`；否则留在最窄的消费边界。
- CLI 的 JSON/table DTO 继续留在 `cli/sandbox`，不进入 `types`。

### 7.3 不做

- 不把接口迁进 `types`。
- 不建立 `apis` 包；当前没有稳定的 HTTP、gRPC 或 CRD wire contract。
- 不让持久化模型直接承担 CLI 输出格式。

### 7.4 验收

- `types` 不含 flag 名称、Cobra 类型、terminal 状态或具体 JSON 展示 DTO。
- 环境变量重复、空键、非法格式在 CLI 边界有表驱动测试。
- exec 的 stdin、stdout、stderr 和退出码行为保持不变。
- `go test ./types ./cli/sandbox ./core ./agent/...` 与 `make verify` 通过。

### 7.5 实施记录

- 2026-09-19，提交 `bcb8f54`：用共享的 `types.Command` 替换带 CLI 语义的 `ExecConfig`；该类型并入现有 sandbox 模型文件，未增加单声明文件。
- CLI 负责把重复的 `KEY=VALUE` 参数转换为 map，后出现的同名变量覆盖先出现的值；core 与 Agent client 只消费中立命令值。`--interactive` 只决定 CLI 是否向 core 传递 stdin。
- `SandboxConfig` 的领域错误不再包含 flag 名称；create 命令在边界上为 CPU、内存和磁盘限制补充对应 flag 上下文。
- guest NDJSON 协议、CLI 参数、stdout/stderr 流和退出码保持不变。定向测试、完整 `make verify` 及 Linux/Darwin `make lint` 全部通过，lint 为 0 issue。
- R4 完成。下一节为 R5 公共 CLI 进度渲染。

## 8. R5：公共 CLI 进度渲染

### 8.1 现状与对照

镜像与 sandbox 命令各自包含相似的 spinner、TTY 检测、刷新和结束输出。对照实现有共享 `progress.Tracker`，但通过 `any` 传递事件，类型不匹配时可能静默丢失。

### 8.2 改造

新增 `cli/progress`，只复用终端展示机制：

- TTY 检测。
- 动画帧和刷新节拍。
- 当前行覆盖与清理。
- 成功、失败和取消收尾。
- 非 TTY 时输出有限的普通状态行。
- context 取消和 goroutine 回收。

领域事件继续属于各自模块或 CLI adapter：

```text
images.Progress / sandbox.Progress
                ↓
对应 CLI adapter
                ↓
cli/progress.Renderer
                ↓
stderr
```

Renderer 不接收 `any`，也不理解 layer、sandbox 或 VMM 状态。

### 8.3 验收

- image import/pull/verify/remove 与 sandbox create/remove/start/stop 共用同一渲染机制。
- stdout 仍只输出命令结果，进度只写 stderr。
- pipe/redirect 下没有控制字符和高频刷屏。
- 成功、失败、取消后都不遗留 goroutine 或半行终端内容。
- renderer 使用 fake clock/writer 的确定性测试，随后执行 `make race`。

### 8.4 实施记录

- 2026-09-20，提交 `21e0f13`：新增 `cli/progress.Renderer`，集中负责 TTY 判断、spinner ticker、写入串行化、stdout 前后的终端行清理、成功/失败/取消/已提交错误收尾和 goroutine 回收。
- `cli/image` 只保留 layer/image 计数与提交语义，`cli/sandbox` 只保留 workflow stage、提交语义和恢复提示；两者都不再维护终端状态、动画帧或 ticker。
- 非 TTY 输出只写换行结束的普通状态，并去除连续重复状态；command result 继续写 stdout，所有进度继续写 stderr。公共 Renderer 不接收 `any`，也不理解 image 或 sandbox 事件。
- Renderer 测试使用手动 ticker 和可观察 writer，确定性覆盖动画推进、输出分流、取消、写失败、初始化失败和 ticker 回收；image adapter 继续覆盖并发 layer 计数、提交后报告失败和 remove 计数。
- `make race`、完整 `make verify` 及 Linux/Darwin `make lint` 全部通过，lint 为 0 issue。R5 完成，下一节为 R6 镜像导入性能测量与优化。

## 9. R6：镜像导入性能测量与优化

### 9.1 现状与对照

对照实现并行处理 layer，并按 digest 加锁，但缓存命中主要只验证普通文件且大小大于零。KumaBox 对 compressed digest、diffID、EROFS 和 boot artifact 的验证更强，不能退化为文件存在性检查。

当前风险是同一导入流程对最终产物进行多次全文件摘要计算，并可能对 manifest 中重复的源 digest 重复转换。

### 9.2 先测量

本节开始时先建立基准和 profile，至少覆盖：

- 全新导入。
- 全缓存命中。
- 一个 layer 损坏后的修复。
- 多个镜像共享 layer。
- manifest 重复引用同一 layer。
- 1、2、4、8 并发转换。

分别记录读取字节数、hash 时间、解压时间、EROFS 转换时间、锁等待时间、总耗时和内存峰值。没有测量证据不得修改校验流程。

### 9.3 候选优化

只有 profile 证明有效时才采用：

- 单次导入内按 source digest 去重转换任务。
- 为一次操作缓存已成功的文件身份与摘要结果。
- 将最终完整验证收敛到 digest 锁内的一次，同时保留锁外转换和锁内复查。
- 避免先完整读取再转换，继续保持流式处理。
- 设定有界 worker 数，来源于 R2 配置而不是硬编码。

发布顺序保持：

```text
staging 转换
   ↓
按 digest 排序加锁
   ↓
锁内复查现有产物
   ↓
原子发布或复用
   ↓
最终完整性校验
   ↓
metadata 短事务提交
```

### 9.4 验收

- 损坏缓存仍能被发现并修复。
- digest、diffID、whiteout、boot candidate 语义不变。
- 相同输入的最终 digest 与当前版本一致。
- benchmark 报告包含改造前后数据；没有可重复收益则不提交优化代码。
- `make verify`、`make race` 和真实 `mkfs.erofs` Linux 验收通过。

### 9.5 实施记录

- 2026-09-21：项目负责人因暂时没有 Linux 测试环境，决定跳过本节；未修改镜像导入性能路径，也未把候选优化标记为完成。
- 遗留：恢复测试环境后执行 9.2 的 benchmark/profile 和 9.4 的真实 `mkfs.erofs` 验收，再根据数据决定是否提交优化。

## 10. R7：测试补强

### 10.1 VMM 与 cgroup

- Cloud Hypervisor 参数和设备顺序的 golden/结构化测试。
- PID、starttime、boot ID、binary 和 socket 身份校验。
- 启动后早退、API 未就绪、stop TERM→KILL、清理失败和重试。
- cgroup 创建、限制写入、进程放置、空组删除和残留恢复。
- Registry 重复注册、未知类型和多个实例隔离。

### 10.2 Agent

对齐成熟实现已有的生命周期覆盖：

- idle connection 下关闭服务。
- accept 永久错误。
- guest 子进程提前退出。
- stdin reader 被取消后退出。
- 所有连接在 shutdown 时关闭并被 WaitGroup 等待。
- 使用 `make race` 验证并发路径。

当前不要求增加 session semaphore；若测试或实际负载证明需要，再单独设计配置和拒绝语义。

### 10.3 CLI

- 构建真实 `kumabox` 二进制执行关键命令，而不只调用 Cobra handler。
- 验证 stdout/stderr 分离、JSON 缩进、退出码、信号取消和非 TTY 输出。
- 覆盖 `image`、`create`、`ps`、`inspect`、`start`、`stop`、`console` 参数校验；需要 KVM 的实体行为仍留给 Linux runbook。

### 10.4 验收

- 新测试验证合同和失败边界，不复制实现细节。
- 测试文件与被测包同目录，不建立生产 `*test` helper 包。
- `make verify`、`make race`、`make lint` 全部通过。

### 10.5 实施记录

- 2026-09-21，提交 `ff2d616`：补齐 VMM 参数与进程身份、启动/停止/清理、cgroup 收敛与 Registry 隔离、Agent 连接生命周期以及真实 CLI 二进制合同测试。
- 证据：完整 `make verify`、`make race`、Linux/Darwin `make lint` 均通过；Linux 专属 cgroup 与进程生命周期测试已交叉编译通过。
- 遗留：Linux 专属测试尚未在真实 Linux 主机执行，随 R6 的测试环境验收一起完成；R7 代码与当前主机验收完成。

## 11. R8：文档与 CI 治理

### 11.1 现状

当前 `.gitignore` 忽略整个 `docs/`，CI 还会拒绝任何被跟踪的文档。README 同时把这些本地文件声明为唯一规范。这使新 clone、代码审查和历史提交无法获得决定代码行为的规范。

成熟开源项目通常把架构、行为、运行手册和决策记录与代码一起评审。对照项目也跟踪其 `docs/`。

### 11.2 改造

- 从 `.gitignore` 删除 `docs/`。
- 删除 CI 中“Reject tracked local documentation”步骤。
- 清理 `docs/` 历史文件，只提交仍然有效的规范、提案、决策和 runbook。
- 修复文档中已失效的 `internal`、daemon、gRPC 和旧阶段描述；历史材料需要保留时明确标注 non-normative。
- README 的结构表、命令示例和阶段状态必须与代码一致。
- 在贡献流程中要求行为或架构变化同时更新对应文档。

### 11.3 审批边界

这一节会改变仓库治理方式，实施前必须由项目负责人再次确认：

- 哪些现有文档进入 Git。
- 历史资料删除还是迁入明确的 archive。
- 中文设计文档是否作为长期规范。

### 11.4 验收

- 全新 clone 能获得构建、架构、行为和 Linux 验收说明。
- README 中不存在指向未提交文件的链接。
- CI 不再拒绝文档，同时仍检查文档引用或格式。
- 所有 normative 文档与当前代码完成一次一致性审查。

### 11.5 实施记录

- 2026-09-21，R8 提交：规范、活动提案和 Linux runbook 进入 Git；README、架构、行为、配置、对照和路线图按当前代码重写。
- 删除被当前实现取代的旧 design/implementation 与已完成提案；CI 改为检查 Markdown 相对链接，`make verify` 同步执行。
- 中文继续作为设计规范语言；根 README、CLI help 和代码注释保持英文。R8 完成，本文件归档。

## 12. 明确撤回或降低优先级的建议

以下内容不作为本轮任务：

| 原建议 | 当前决定 | 原因 |
|---|---|---|
| 立即拆分 `vmm.Backend` | 撤回 | 当前方法都是基础进程能力；对照实现的胖接口更宽，机械拆分会增加类型断言和装配复杂度 |
| 为每个 VMM 能力建立一个接口 | 撤回 | 等 snapshot、pause、restore 的真实第二能力出现再定义可选接口 |
| Agent 立即增加最大 session 数 | 降低优先级 | 当前模型与对照实现一致；先补取消、关闭和 goroutine 泄漏测试 |
| 使用缓存文件大小判断替代完整 hash | 否决 | 会降低内容寻址存储的完整性保证 |
| 把跨模块编排移回 CLI | 否决 | 当前 `core` 应用服务边界更清楚 |
| 把接口集中放进 `types` | 否决 | `types` 只保存共享数据和值对象 |
| 为 console、exec 建独立包 | 否决 | 它们是 SandboxService 的运行态能力，独立包只会制造碎片 |

## 13. 完成定义

本轮架构改造只有在以下条件全部满足后才能关闭：

- R1–R8 每节均有单独批准、提交和验收记录。
- 默认 CLI、JSON、metadata、磁盘目录和 guest 协议保持兼容，获批变更除外。
- 运行策略不再散落为无法覆盖的硬编码。
- `core.SandboxService`、VMM Registry、CLI progress 和 exec 数据边界能够从包结构直接理解。
- 镜像性能优化有基准证据且未削弱完整性校验。
- VMM、cgroup、Agent 生命周期和真实 CLI 边界具有足够失败路径测试。
- 有效设计文档能随仓库获取并与代码一起评审。

每完成一节，在本文对应标题下追加一条不超过五行的实施记录：提交号、关键变化、测试证据、遗留项。不得提前把尚未实现的内容标记为完成。
