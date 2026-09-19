# GoTenon 功能规划

> 本文只依据当前代码与可验证的问题制定规划，历史草稿不作为规划依据。
> 状态标记：✅ 已完成 ｜ 🚧 部分完成 ｜ ⏳ 未开始。

---

## 1. 项目定位

GoTenon 是宿主进程内嵌的 Go 插件框架，当前内核由四块构成：

| 块 | 文件 | 职责 |
|---|---|---|
| 上下文 | `context.go` | 作用域树、槽位隔离、配置层、运行时信息 |
| 副作用 | `effect.go` | 可逆注册，卸载时逆序回滚 |
| 治理 | `manager.go` `graph.go` `converge.go` `lifecycle.go` | 插件表、依赖图、波次收敛、装载卸载 |
| 通信与扩展 | `message.go` `loader.go` `logger.go` `errors.go` | 消息路由、扩展钩子、日志、错误码 |

规划原则：

1. **证据驱动**：每条规划都必须能指到具体代码位置或可复现问题；
2. **正确性优先**：不挂死、不泄漏、可回滚，先于新功能；
3. **小步交付**：每个版本可独立验收，验收未过不进入下一版本；
4. **可观测**：新增能力必须带诊断出口（状态、日志、快照）。

---

## 2. 现状盘点

### 2.1 已落地

| 能力 | 位置 | 说明 |
|---|---|---|
| 上下文树 | `context.go:31` `:43` | `New` / `Extend`，子继承父、永不修改父 |
| 运行时信息 | `context.go:62` `:69` | `SetInfo` / `GetInfo` 沿祖先链查找 |
| 私有槽位 | `context.go:84` | `Isolate(name)` 子树内独立解析 |
| 共享槽位 | `context.go:99` | `IsolateLabel(name, label)` 同 label 复用 |
| 配置层 | `context.go:130` `:139` | `Intercept` 追加，`Config` 返回 root-first 层序列 |
| 槽位诊断 | `context.go:162` | `SlotOf` 返回实际解析到的槽位 |
| 可逆副作用 | `effect.go:100` `:119` `:134` | `Register` / `Effect`；幂等、逆序、panic 隔离、错误聚合 |
| 插件契约 | `plugin.go:9` | `PluginInfo` 十个方法 |
| 运行时状态 | `runtime.go:25` | `PluginTable` 唯一注册处；`PluginRuntime` 持依赖边与状态 |
| 登记与删除 | `manager.go:50` `:89` | 查重、钩子、失败回滚、环检测 |
| 启停 | `manager.go:117` `:142` | 启用位 + Loader 钩子 + 收敛 |
| 读取与更新 | `manager.go:172` `:179` | 按插件名取运行时；`Update` 重载 |
| 可用性 | `manager.go:200` | 扫描已装载插件的槽位 |
| 消息路由 | `manager.go:217` `message.go:46` | `Send` → `DealWithMessage`；`RegisterPipe` 带停止信号 |
| 依赖图 | `graph.go:12` `:30` `:57` `:78` | 建边、DFS 环检测、加载闭包、依赖就绪 |
| 收敛调度 | `converge.go:11` | 波次并行装载/卸载，依赖先行、叶子先卸 |
| 装载与超时 | `lifecycle.go:97` | `Apply → Start → Run`；超时回滚上下文 |
| 上下文清理 | `lifecycle.go:143` | 递归清子树：逆序回收副作用、清空槽位与配置层 |
| 扩展钩子 | `loader.go:12` `:25` | `Loader` 五钩子 + 超时；可选并行度 |
| 日志与错误 | `logger.go` `errors.go` | `Logger` 出口；稳定 `ErrorCode` + `IsCode` |
| 启动入口 | `enter.go:10` | `Run` + 六种 `Option` |
| 可运行示例 | `example/main.go` `example/plugin.go` | 独立模块：游戏主进程模拟，覆盖正常与边界路径 |

### 2.2 能力边界（当前语义）

- 依赖边只按**插件名**解析：`Inject()` 返回的名字必须等于目标插件的 `Name()`；
- 并行度优先级：`Loader` 实现 `ConcurrencyLoader` > `Manager.Concurrency` > 缺省 4；
- 装载超时由 `Loader.Timeout` 提供，`<=0` 不限时；
- 状态字段：`Enable`（启用意图，bool）与 `State`（生命周期状态机：
  `Disabled` / `Pending` / `Loading` / `Ready` / `Failed` / `Kept` / `Unloading`）；
  `Loaded()` 为派生判断（Ready / Kept / Unloading）；`Err`、`Missing` 用于诊断；
- 消息载荷类型是 `context.Context`，同步调用目标插件的 `DealWithMessage`。

---

## 3. 问题清单

### 3.1 正确性（P0）

1. **零测试**：仓库没有任何 `*_test.go`，核心语义（收敛、逆序回收、超时回滚、环检测）无回归保护。
2. **装载超时竞态**：`loadOne`（`lifecycle.go:97`）超时后调用 `cleanContext`，
   但装载协程无法强杀：若 `Apply` 之后继续执行并调用 `ctx.Register`（`effect.go:100`），
   此时 effect 栈已被 `dispose`（`effect.go:134`）置空，`Register` 会新建基座 scope，
   该 disposer 永远不会被回收——资源泄漏，且现有 `ErrInactiveEffect` 并未启用。
3. **Manager 无同步**：`Manager` 公开方法（`manager.go:50`–`:226`）假定单一调度者，
   并发调用 `Register` / `Enable` / `Disable` / `Update` 存在数据竞争。
4. **`Update` 非事务**（`manager.go:179`）：先卸载再按新配置装载，新配置失败时旧实例已丢失，无法回滚。
5. **运行期错误无恢复**：`Start` / `Run` / `DealWithMessage` 的 panic 无 recover；
   装载成功后的运行错误没有状态与恢复入口。
6. **卸载残留**：`unloadBatch`（`lifecycle.go:50`）不清理 `Missing`；`Enable` 重复调用
   （`manager.go:117`）无幂等语义，会重复触发 Loader 钩子与 Hook。

### 3.2 语义与 API（P1）

7. **三枚错误码定义未用**：`ErrInactiveEffect` / `ErrConfigInvalid` / `ErrDisposed`（`errors.go:12`）无触发点。
8. **契约字段未被消费**：`PluginInfo.Desc` 与 `PluginInfo.Config`（`plugin.go:12` `:18`）框架从不调用。
9. **Loader 扩展面未定型**：钩子调用时机、失败语义与回滚责任无文档。
10. **槽位解析歧义**：同层同 name 注册多个 label 时，`SlotOf`（`context.go:162`）的返回顺序未定义。
11. **`Available` 无索引**（`manager.go:200`）：遍历全部已装载插件的槽位，插件多时为 O(n)。
12. **`Send` 语义不足**（`manager.go`）：只查 `Loaded()` 不查 `Enable` 意图；无超时、无异步、错误传播未定义。
13. **上下文 kv 缺失**：`Set` / `Delete` 高级读写未实现（原 TODO 注释已清理，计划见 v0.3）。
14. **启动入口不一致**：`Run` 返回 `error` 而宿主经包级 `PluginManager`（值拷贝）访问，
   与 `enter.go:3` 注释"使用 Run 返回的实例"不符；模块路径 `GoTenon` 不是规范导入路径。
15. **管道不随插件回收**：`RegisterPipe`（`message.go:46`）需调用方手动 cancel，未接入 effect 作用域。

### 3.3 工程配套（P2）

16. 缺 CI、`CHANGELOG.md`、`CONTRIBUTING.md`、`LICENSE`。
17. 日志级别 `LevelInfo` / `LevelError` 已声明但无使用（`logger.go:10`）。
18. 示例只有一个单文件，覆盖不到启停级联、消息管道、失败恢复等场景。
19. `temp/` 遗留历史草稿，与当前代码语义已不一致，易误导。

---

## 4. 路线图

### v0.2 内核正确性（首要）

**目标**：把 v0.1 的语义用测试钉死，消除已知竞态与中间态，达到"可被信任地嵌入"。

**交付**：

- 测试体系：`context` / `effect` / `graph` / `converge` / `lifecycle` / `message` 单元测试；
  `go test -race ./...` 通过；失败注入用例：disposer panic、`Apply` 失败、装载超时、
  依赖成环、重复启停、卸载时注册。
- 修复装载超时竞态：为 effect 栈增加活性标志，dispose 后注册返回 `ErrInactiveEffect`（或 no-op 并记录），
  超时路径与装载协程的收尾协议明确化。
- 生命周期状态机：把 `State` / `Loaded` / `Err` / `Missing` 归一为显式状态
  （`Disabled` / `Pending` / `Loading` / `Active` / `Failed` / `Unloading`），对外只读。
  （🚧 已落地：`Enable bool` 意图位 + 七态状态机，含 `Kept`（未启用但被依赖保留）；
  迁移日志与对外只读快照待补）
- 并发模型定稿：`Manager` 内部锁或单驱动队列，明确"状态由调度者统一写回"的边界；
  并发启停 `-race` 用例。
- `Update` 事务化：新配置失败回滚旧实现；保留 `UpdateLoose` 的简单语义。
- 运行错误恢复：`Start` / `Run` / `DealWithMessage` panic 转 error；
  错误状态可观测，并提供显式恢复入口。
- 卸载清理：`Missing` / `Err` 复位；`Enable` / `Disable` 幂等。
- 工程配套：CI（build / vet / test -race）、`CHANGELOG.md`、`CONTRIBUTING.md`、`LICENSE`。

**验收**：`go test -race ./...` 全绿；上述失败注入全部有断言；README 示例可编译运行。

### v0.3 契约与 API 收敛

**目标**：公开面定型，不留"未消费字段"和"占位注释"。

**交付**：

- `PluginInfo` 定型：`Desc` / `Config` 要么被框架消费（诊断、配置展示），要么从接口裁剪。
- `Loader` 定型：删除占位注释，明确每个钩子的调用时机、失败语义与回滚责任；
  提供测试用 Fake Loader。
- 错误码启用：`ErrDisposed` / `ErrConfigInvalid` / `ErrInactiveEffect` 均有真实触发点。
- 启动入口统一：`Run` 与 `PluginManager` 语义二选一；模块路径迁移为规范导入路径。
- 上下文 kv：实现 `Set` / `Delete`（`context.go:93`）。
- `SlotOf` 多 label 解析顺序定义；`Available` 建立索引或缓存。
- 公开 API 文档注释补全，并新增对应示例。

**验收**：公开 API 无未使用项、无占位注释；每条公开语义至少一个测试。

### v0.4 服务与配置

**目标**：把"槽位 + 配置层"升级为可用的服务读写与配置校验。

**交付**：

- 服务读写完整化：类型化或槽位化的提供/读取/更新 API，服务就绪查询。
- 配置消费：`Intercept` / `Config` 的合并辅助；`Apply` 之前的配置校验钩子，
  失败返回 `ErrConfigInvalid`。
- 依赖变化重载：提供方 `Update` / 卸载后，依赖者恰好重载一次，不产生震荡。

**验收**：提供方更新后依赖者只重载一次；配置非法时 `Apply` 未执行；多租户槽位隔离用例通过。

### v0.5 事件与消息

**目标**：从点对点消息升级为可订阅、可观测的通信层。

**交付**：

- 消息语义：`Send` 校验 `Enable` 意图；同步 / 异步 / 超时策略；错误传播路径明确。
- 事件订阅：一对多分发、顺序保证、错误聚合。
- 管道归属：`RegisterPipe` 返回 `Disposer` 并接入 effect 作用域，随插件或管理器卸载回收。
- 消息日志与统计出口。

**验收**：并发分发 `-race` 通过；管道无泄漏；订阅者随插件卸载自动摘除。

### v0.6 治理与可观测

**目标**：运行中的插件集可被完整检查与操作。

**交付**：

- 引用计数对外可观测：`Refs` / `Dependents` 查询 API。
- 禁用策略：`Disable` 返回阻塞者列表；提供强制级联禁用。
- Inspect 快照：插件表 / 依赖图 / 状态 / 槽位，JSON 稳定序列化。
- 依赖图导出：DOT / Mermaid。
- 日志级别实际使用（`Info` / `Error`），关键路径结构化。

**验收**：快照可稳定 diff；导出的图可直接渲染；一次完整启停流程可由日志独立还原。

### v1.0 稳定

**目标**：冻结 API，给出兼容承诺。

**交付**：

- API 冻结与语义版本；不兼容变更需附迁移说明。
- 基准测试：大量插件装载/卸载、并发收敛；给出回归阈值。
- 示例集：服务依赖、消息管道、禁用级联、失败恢复。
- 文档完备：README、ROADMAP、CHANGELOG 与代码同步。

**验收**：兼容性承诺发布；基准回归通过。

---

## 5. 非目标

| 项 | 原因 |
|---|---|
| 隐式属性拦截 / 自动依赖收集 | 显式 `ctx` 与 `Inject()` 是本项目的前提，不做反射魔法 |
| 进程内代码热卸载 | Go `plugin.Open` 无 close，Windows 不支持 |
| 内置跨进程通信协议 | 外部插件源属于扩展，交给 `Loader` 实现方 |
| 插件分发 / 市场 | 属宿主应用层，不属于内核 |

---

## 6. 开放问题

1. 超时后无法强杀的装载协程如何协作式取消：是否给 `Apply` 注入 `context.Context`？
2. 状态机的迁移日志与对外只读快照（Inspect）格式待定。
3. 并发模型选型：内部锁 vs 单驱动队列，哪个更贴合"状态由调度者统一写回"。
4. 消息载荷为 `context.Context` 的语义边界（取消、超时、跨插件传递）。
5. 依赖边仅按插件名解析，是否需要别名或服务名到插件的映射。

---

## 7. 版本完成定义

1. 交付项全部实现且有测试覆盖；
2. `go build ./...`、`go vet ./...`、`go test -race ./...` 通过；
3. README 与本文同步更新，`CHANGELOG.md` 记录变更与不兼容点；
4. 不兼容变更必须给出迁移说明。
