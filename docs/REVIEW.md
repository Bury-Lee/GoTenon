# GoTenon 设计与实现评审

> 评审范围：内核 12 个源文件（`context.go` / `effect.go` / `plugin.go` / `runtime.go` /
> `manager.go` / `graph.go` / `converge.go` / `lifecycle.go` / `message.go` / `loader.go` /
> `logger.go` / `errors.go`）与 `example/` 下三个示例（`game_example` / `web_example` /
> `microservice_example`，含两份 `DESIGN.md`）。
>
> 结论面向 v0.1 内核成型阶段。本文与 [ROADMAP.md](ROADMAP.md) 互补：ROADMAP 是排期计划，
> 本文是问题清单与设计缺口，逐条指到代码位置。状态标记：🔴 正确性隐患 ｜ 🟡 语义/表达力 ｜
> 🔵 能力缺口。

---

## 1. 总体结论

**理念统一、抽象干净、示例质量高，是合格的 v0.1 内核成型；但作为"插件库"仍缺关键能力，
且存在若干正确性隐患。**

设计主轴（"三件套"）非常清晰，一句话可概括全部扩展方式：

> **扩展 = 往依赖图里加节点 + 在 `Apply` 期登记 `Disposer`。内核与既有插件零改动。**

这条判据在 `web_example` / `microservice_example` 中被反复兑现（加路由、加拦截器都只是加节点），
是这套设计最值得肯定的部分。

| 维度 | 评价 |
|---|---|
| 概念统一性 | 优秀：共享槽 / 可逆副作用 / 依赖声明 三件套贯穿全部示例 |
| 可逆副作用 | 扎实：幂等、逆序、panic 隔离、错误聚合（`effect.go`） |
| 依赖调度 | 良好：DAG + 拓扑波次 + 并行 + 环检测 + 收敛后返回 |
| 示例与文档 | 优秀：边界路径（超时/成环/Pending/Kept）都显式演示 |
| 并发正确性 | 不足：Manager 无锁、超时协程不可取消、运行期 panic 无隔离 |
| 依赖表达力 | 不足：依赖按**插件名**而非**服务名**，缺可选依赖/排序边 |
| 插件库完整度 | 不足：缺服务注册/DJ、事件总线、插件发现与动态加载、可观测 |

---

## 2. 设计亮点

1. **可逆副作用模型扎实**（`effect.go`）。
   - `once` 保证幂等（`effect.go:17`），`runDisposer` / `runBody` 把 panic 转为 error（`effect.go:29` `:39`）；
   - `effectScope.dispose` 逆序回收、单点失败不阻断其余、`errors.Join` 聚合（`effect.go:68`）；
   - `Effect` 嵌套形成可逆序拆解的回收树（`effect.go:111`），事务化路由组/服务注册全靠它。
2. **依赖图与收敛设计正确**。
   - `relink` 自动按 `Inject()` 建边（`graph.go:6`），`checkCycle` DFS 提前拒绝成环（`graph.go:24`）；
   - `converge` 波次并行、装载优先、只挑叶子卸载，天然保证"依赖者先走"（`converge.go:5`）；
   - `converge()` 返回即稳定态，不存在后台震荡（README 设计原则 5）。
3. **状态机语义清晰**（`runtime.go:25-38`）。`Enable` 是意图位、`State` 是生命周期，
   `Kept` 表达"未启用但被依赖者保留"，`Loaded()` 是派生判断（`runtime.go:83`）。
4. **上下文树分层干净**（`context.go`）。子继承父、永不修改父；`Intercept` + `Config`
   提供 root-first 配置层（`context.go:134`）；`Isolate` / `IsolateLabel` 私有/共享槽语义明确。
5. **示例把边界显式跑出来**：超时后 `Start`/`Run` 仍被调用、成环被拒、Pending 自动补装、
   Kept 仍可收消息——既是演示也是自曝缺陷，文档价值高。

---

## 3. 正确性隐患（P0）

### 🔴 3.1 Manager 无同步，存在数据竞争

`Manager` 全部公开方法（`manager.go:5-14` 起）假定单一调度者，无任何互斥；
而 `Get()` 直接把 `*PluginRuntime` 暴露给调用方（`manager.go:175`），
`State` / `Err` / `Config` / `Enable` / `Missing` 会被并发读写；
`PluginTable.Info` 与 `m.rt` 两个 map 也无保护。消息管道在独立协程里回调 `Send`
（`message.go:52`），插件钩子也可能回调 Manager，竞争现实存在。

**建议**：Manager 内部一把大锁，或把 `Enable/Disable/Update` 改为单一命令协程串行消费；
`Get` 返回**只读快照**而非内部指针。

### 🔴 3.2 重入 / 递归未防护

插件在 `Apply` / `DealWithMessage` 内回调 `m.Enable/Disable/Update` 会递归进入
`converge()`（`manager.go:119` `:194`），造成状态错乱甚至死锁。

**建议**：reentrancy guard（检测到收敛中的回调则报错或排队），或明确禁止并在文档中写死。

### 🔴 3.3 装载超时后协程无法取消

`loadOne` 超时后调用 `cleanContext` 回滚（`lifecycle.go:110-149`），但装载协程无法强杀：
`Apply` 返回后仍会继续执行 `Start` / `Run`，可能在上下文已清空后重新产生副作用或泄漏。
`game_example/slowPlugin`（`example/game_example/plugin.go:315-332`）已把这个 bug 当作"边界"演示出来，
`ROADMAP §3.1-2` 亦记录了由此产生的 disposer 泄漏。

**建议**：`Apply` / `Start` / `Run` 注入 `context.Context` 做协作式取消；超时后晚到的
`ctx.Register` 应返回 `ErrInactiveEffect` 或 no-op 并记录。

### 🔴 3.4 卸载无超时、运行期 panic 无隔离

- `unloadOne`（`lifecycle.go:152`）直接调用 `End` 与 `cleanContext`，**无超时**：
  `End` 卡死会永久阻塞 `Disable` / `Delete`。
- recover 只覆盖 disposer 与 effect body（`effect.go:29` `:39`）；插件 `Run` 起的常驻协程、
  `DealWithMessage`（`manager.go:218`）均无 recover，插件 panic 会拖垮宿主进程。

**建议**：引入 supervisor——卸载加超时；`Start` / `Run` / `DealWithMessage` panic 转 error
并置 `Failed`；常驻协程由框架托管以便回收与重启。

### 🔴 3.5 effect 栈假设单协程 Apply

`pushScope` / `popScope` / `Register`（`effect.go:91` `:142` `:154`）基于 per-context 的 LIFO 栈；
若插件在 `Apply` 内起多个协程注册，作用域会串味。

**建议**：文档明确"`Apply` 必须单协程注册"，或改为 per-goroutine 作用域。

### 🔴 3.6 若干状态突变顺序问题

| 问题 | 位置 | 影响 |
|---|---|---|
| `Delete` 先清 `Enable` 再检查依赖者 | `manager.go:98` | 被拒后插件卡在 Kept（示例已注释） |
| `Register` 回滚不回滚 `p.Register()` 的副作用 | `manager.go:63-86` | 插件自身注册的半成品残留 |
| `SlotOf` 同 name 多 label 返回顺序未定义 | `context.go:166-175` | map 随机，结果不确定 |
| `RegisterPipe` 的 cancel 只关 `stop`，不 drain/close `in` | `message.go:46-72` | 发送方可能阻塞 |
| `unloadBatch` 不清理 `Missing`；`Enable` 非幂等 | `lifecycle.go:55` `manager.go:119` | 遗留脏状态、重复触发钩子 |

---

## 4. 语义与表达力（P1）

### 🟡 4.1 依赖按"插件名"而非"服务名"解析

`Inject()` 返回的名字必须等于目标插件的 `Name()`（`graph.go:6`），
**插件依赖的是"谁"而不是"什么（能力）"**。后果：

- 提供同一能力的多个实现无法被统一消费；
- 插件改名即破坏全部依赖者；
- 无法表达版本/能力约束。

示例只能靠**宿主预挂 root 共享槽**绕开（`web_example/DESIGN.md §5.1`、
`microservice_example/DESIGN.md §2.1`），且必须先在共同祖先声明——兄弟插件互相看不到对方的
私有槽（`example/game_example/main.go:114-120` 已把该边界打印出来）。

**建议**：升级为一等公民 `Provides()` / `Requires(service)`，按服务名 + 类型自动装配，
取代 `host.Service` 逃生口（对应 ROADMAP v0.4）。

### 🟡 4.2 缺可选依赖与"仅排序"边

`Inject()` 只有**硬依赖**一种语义。缺：

- **可选/软依赖**：目标存在则串联、缺失不阻塞（现在只能运行时 `Available` 探测）；
- **仅排序边**：`after X`（要顺序但不必依赖）——微服务示例不得不用链式 `Inject` 强制拦截器
  顺序（`microservice_example/DESIGN.md §3.2`），代价是关一个会级联。

### 🟡 4.3 `Update` 导致依赖者持有过期引用

`Update` 无视依赖者强制卸载重载（`manager.go:182-200`），依赖者仍持有旧实例句柄。
`game_example/main.go:236-243` 把这称为"依赖者持有过期引用 ← 边界"。

**建议**：提供稳定句柄/proxy（更新只换实现不换引用），或让升级沿依赖图级联刷新依赖者，
保证"依赖者恰好重载一次"（ROADMAP v0.4 验收项）。

### 🟡 4.4 配置体系未成型

- `ErrConfigInvalid` 已定义但无触发点（`errors.go:17`）；`Desc()` / `Config()` 框架从不消费
  （`plugin.go:14` `:19`，ROADMAP §3.2-8 已记录）；
- `Apply(ctx, cfg any)` 类型不安全、无校验；
- `Intercept` 的多层配置需插件**自行**遍历合并（`context.go:134`，`game_example/plugin.go:66`）。

**建议**：类型化配置 + schema 校验（`Apply` 之前触发 `ErrConfigInvalid`）+ 框架自动把
`Config(name)` 合并结果注入 `Apply`。

### 🟡 4.5 `Send` 语义不足

`Send`（`manager.go:218`）只查 `Loaded()` 不查 `Enable` 意图，无超时、无异步、无错误传播约定；
`Message` 载荷是裸 `context.Context`（`message.go:15`），消息本身无 ID / 元数据 / 优先级 / 重试语义。

---

## 5. 缺失的插件库能力（P1/P2）

### 🔵 5.1 服务注册与依赖注入（最关键）

当前没有一等的 `provide/require`。建议新增强类型服务注册表，按服务名解析并提供者选择，
替代"宿主预挂 root 槽"与 `host.Service` 逃生口。这是打开 DI、可选依赖、多实现的基础。

### 🔵 5.2 插件发现与动态加载

目前只能编译期 `WithPlugin` / `NewManager` 注册；`Loader.IntoRegister` 仅是钩子，
没有实战发现实现。一个插件库通常需要：

- 目录/清单扫描 + manifest（name / version / deps / 权限）；
- 可选 `.so`（受限）或**子进程 RPC**（`hashicorp/go-plugin` 那类）——顺带解决 panic 与资源隔离。

> 注：ROADMAP §5 已把"进程内代码热卸载"与"内置跨进程协议"列为非目标，
> 但**插件发现 + manifest + 子进程插件源**可作为独立的扩展模块（如 `gotenon-grpc` 之于内核）。

### 🔵 5.3 事件总线 / 发布订阅

现在只有点对点 `Send` 按插件名路由。缺：topic 订阅、一对多广播、顺序保证、request-reply、
消息超时/异步结果（对应 ROADMAP v0.5）。

### 🔵 5.4 可观测与治理

- 无 `Inspect` 全局快照（插件表 / 依赖图 / 状态 / 槽位）；
- 无依赖图导出（DOT / Mermaid）；`Available` 无索引，O(n) 扫描（`manager.go:204`）；
- 无状态变更回调 / `Observer`；`Loader` 只有 per-operation 钩子，拿不到状态机迁移事件；
- 无健康检查、失败自动重试退避、引用计数对外查询（对应 ROADMAP v0.6）。

### 🔵 5.5 启动入口与全局态

`Run` 成功后把 `Manager` **值拷贝**写入包级 `PluginManager`（`enter.go:3` `:35`），
全局可变、难测试，且与注释"使用 Run 返回的实例"不符；模块路径 `GoTenon` 非规范导入路径
（ROADMAP §3.2-14 已记录）。

---

## 6. 与 ROADMAP 的对照

| 本文条目 | ROADMAP 对应 | 状态 |
|---|---|---|
| 3.1 Manager 无同步 | §3.1-3 / v0.2 "并发模型定稿" | ⏳ |
| 3.2 重入未防护 | 未记录 | 新增建议 |
| 3.3 超时协程不可取消 | §3.1-2 / v0.2 "修复装载超时竞态" | ⏳ |
| 3.4 卸载无超时、panic 无隔离 | §3.1-5 / v0.2 "运行错误恢复" | 🚧 部分 |
| 3.5 effect 栈单协程假设 | 未记录 | 新增建议 |
| 3.6 状态突变顺序 | §3.1-6 / v0.2 "卸载清理" | ⏳ |
| 4.1 依赖按插件名 | §3.2 / v0.4 / 开放问题 5 | ⏳ |
| 4.2 可选依赖/排序边 | 未记录 | 新增建议 |
| 4.3 Update 悬垂引用 | v0.4 "依赖变化重载" | ⏳ |
| 4.4 配置体系 | §3.2-7、`ErrConfigInvalid` / v0.4 | ⏳ |
| 4.5 Send 语义 | §3.2-12 / v0.5 | ⏳ |
| 5.1 服务注册与 DI | v0.4 "服务读写完整化" | ⏳ |
| 5.2 插件发现与动态加载 | §5 非目标（部分） | 建议以独立模块实现 |
| 5.3 事件总线 | v0.5 | ⏳ |
| 5.4 可观测与治理 | v0.6 | ⏳ |
| 5.5 启动入口与全局态 | §3.2-14 / v0.3 | ⏳ |

---

## 7. 建议落地顺序

1. **v0.2 先止血**：Manager 加锁 + `Get` 返回快照（3.1）；超时协作式取消（3.3）；
   卸载超时 + 运行期 panic 隔离（3.4）；配套 `go test -race ./...`。
2. **v0.2/v0.3 补齐语义**：effect 栈协程约定（3.5）；`Delete`/`Register` 突变顺序（3.6）；
   `ErrConfigInvalid` 等错误码启用（4.4）。
3. **v0.4 打开表达力**：`Provides/Requires` 服务注册 + DI（4.1、5.1）；
   可选依赖/排序边（4.2）；`Update` 稳定句柄或级联刷新（4.3）；类型化配置（4.4）。
4. **v0.5 通信层**：事件总线/订阅（5.3）；`Send` 同步/异步/超时语义（4.5）。
5. **v0.6 治理与扩展**：`Inspect` / 图导出 / Observer（5.4）；
   插件发现 + manifest，进程内动态加载若不可行则落到子进程独立模块（5.2）。
6. **持续**：启动入口统一与模块路径规范化（5.5）。

---

## 8. 非目标（与 ROADMAP 一致）

- 隐式属性拦截 / 反射依赖收集；
- 进程内代码热卸载（Go `plugin.Open` 无 close，Windows 不支持）——**但子进程插件源可做**；
- 内置跨进程通信协议（交给扩展模块）；
- 插件分发 / 市场（属宿主应用层）。
