# GoTenon

> 面向 Go 的插件化内核：**上下文作用域 × 依赖驱动生命周期 × 可逆副作用**。

GoTenon（榫卯）取「构件咬合、可拆可换」之意，是一个宿主进程内嵌的插件框架。
插件通过接口与上下文与宿主咬合：注册的一切能力都登记了逆操作，卸载即精确回滚；
插件之间的依赖构成有向无环图，由收敛调度器按依赖波次自动装载与卸载。

---

## 特性

- **上下文树与作用域**：`Extend` 派生子上下文，子继承父、永不修改父；
  `Isolate` / `IsolateLabel` 提供私有与共享槽位，`Intercept` 提供 root-first 合并的配置层。
- **依赖驱动的生命周期**：插件声明 `Inject()`，启用时自动拉取整个依赖闭包，
  按拓扑波次并行装载；依赖缺失时保持 PENDING 而不报错，引用归零时自动卸载。
- **可逆副作用**：一切注册都返回 `Disposer`，卸载时按注册顺序逆序回收；
  `Effect` 把一组注册包成可整体回滚的事务；单个失败不阻断其余，panic 被隔离为 error。
- **收敛调度**：`Register / Enable / Disable / Update` 返回即收敛到稳定态，
  同波次内互不依赖的插件并行执行，状态由调度者统一写回。
- **成环检测**：注册与启用前做 DFS 环检测，成环立即报错，不挂死。
- **消息与管道**：`Message{Name, Data}` 按插件名路由到 `DealWithMessage`；
  `RegisterPipe` 提供带停止信号的消费管道，不留死协程。
- **扩展点齐全**：`Loader` 钩子（注册/删除/启停/收尾/超时/并行度）、
  `Logger` 日志出口、稳定 `ErrorCode` 错误分类。
- **零依赖**：仅使用 Go 标准库。

## 快速开始

```bash
cd example/game_example && go run .        # 游戏主进程模拟
cd example/web_example  && go run .        # 在线服务网站 + 路由/gRPC 扩展
cd example/microservice_example && go run . # 微服务插件版(发现/负载均衡/拦截器/熔断/追踪)
```

最小示例（完整可运行版本见 [example/game_example/main.go](example/game_example/main.go)，
插件实现见 [example/game_example/plugin.go](example/game_example/plugin.go)）：

```go
package main

import (
	"context"
	"fmt"

	"GoTenon"
)

type greeter struct{}

func (p *greeter) Name() string              { return "greeter" }
func (p *greeter) Desc() map[string]string   { return nil }
func (p *greeter) Inject() []string          { return nil }
func (p *greeter) Config() map[string]string { return nil }
func (p *greeter) Register() error           { return nil }
func (p *greeter) Start() error              { return nil }
func (p *greeter) Run() error                { return nil }
func (p *greeter) End() error                { return nil }

func (p *greeter) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	name, _ := cfg.(string)
	ctx.Register(func() error { // 卸载时自动逆序回收
		fmt.Println("bye", name)
		return nil
	})
	fmt.Println("hello", name)
	return nil
}

func (p *greeter) DealWithMessage(ctx context.Context) error {
	fmt.Println("message received")
	return nil
}

func main() {
	if err := GoTenon.Run(GoTenon.WithPlugin(&greeter{}, "world")); err != nil {
		panic(err)
	}
	if err := GoTenon.PluginManager.Send(GoTenon.Message{
		Name: "greeter",
		Data: context.Background(),
	}); err != nil {
		panic(err)
	}
}
```

> 模块路径当前为 `GoTenon`，用于本地开发；发布前将迁移到规范的远程导入路径（见路线图）。

## 核心概念

### 上下文树与作用域

`GoTenonContext` 是插件的安全上下文，每个插件装载时获得一个子上下文：

```go
root := GoTenon.New("app")
child := root.Extend("child")     // 派生子上下文；子继承父，永不修改父

root.SetInfo("env", "dev")        // 本层运行时信息
env, ok := child.GetInfo("env")   // 本层没有则沿祖先链向上查找

root.Isolate("svc/db")            // 私有槽位：该子树内独立解析
root.IsolateLabel("svc/db", "tenant-a") // 共享槽位：相同 label 复用同一槽

root.SlotOf("svc/db").Value = db  // 绑定服务实例
```

配置分层用 `Intercept`，解析时沿祖先链 **root-first** 合并，后层覆盖前层：

```go
root.Intercept("http", Config{Port: 80})
child.Intercept("http", Config{Port: 8080})
layers := child.Config("http") // [80 层, 8080 层]，由消费方依次合并
```

### 插件生命周期

插件是 `PluginInfo` 接口的实现。插件身份是**接口指针**：同一指针多次挂载共享一个运行时，
每次挂载创建互不影响的上下文：

```go
type PluginInfo interface {
	Name() string                             // 展示名，同时是管理句柄
	Desc() map[string]string                  // 描述
	Inject() []string                         // 依赖的服务名（按插件名解析）
	Config() map[string]string                // 基础配置

	Register() error                          // 写入插件表之前
	Apply(ctx *GoTenonContext, cfg any) error   // 依赖就绪后；期间的注册归本插件所有
	Start() error                             // 装载完成后
	Run() error                               // 常驻运行入口
	DealWithMessage(context.Context) error    // 处理发给本插件的消息
	End() error                               // 卸载前：资源回收与持久化
}
```

装载顺序 `Apply → Start → Run`，任一步失败都会回滚已注册的半成品；
卸载时 `End` 之后由框架逆序回收全部副作用，并递归清理上下文。

### 可逆副作用

一切注册皆副作用：`Register` 把撤销函数挂到当前 effect 栈，卸载时逆序执行；
`Effect` 把一段 body 期间注册的副作用归为一个可整体回收的组：

```go
ctx.Register(func() error { return conn.Close() }) // 卸载时逆序回收

dispose, err := ctx.Effect(func() error {
	ctx.Register(...) // 组内注册，body 失败时立即整体回滚
	return nil
}, "connect")
_ = dispose // 也可提前手动回收该组
```

Disposer 幂等、可并发、不 panic（框架 recover 并转为 error）。

### 依赖图与收敛调度

- `Inject()` 返回的依赖名按插件名解析，注册表变化后自动重建依赖边；
- 注册与启用前做环检测，成环返回 `INVALID_PLUGIN`；
- `Enable` 从启用标志出发计算加载闭包，按依赖波次并行装载（缺省并行度 4）；
- 依赖未注册或未装载的插件保持 PENDING，`Missing` 字段记录缺口；
- `Disable` 后，未被启用且引用归零的插件自动卸载（依赖者先走）；
- 装载失败记入 `Err`，不自动重试，`Update` / `Enable` 可恢复。

### 消息与管道

```go
processor := GoTenon.NewMessageProcessor(m)
processor.OnError = func(msg GoTenon.Message, err error) { /* 上报 */ }

pipe, cancel := processor.RegisterPipe(16) // 有缓冲的发送管道
defer cancel()                             // 停止消费协程，不留死协程

pipe <- GoTenon.Message{Name: "render", Data: context.Background()}
```

### 扩展点

| 扩展点 | 说明 |
|---|---|
| `Loader` | 按插件名回调：`IntoRegister` / `Delete` / `Enable` / `Disable` / `Hook`；`Timeout` 负责装载超时预算 |
| `ConcurrencyLoader` | 可选能力：定制并行装载度（<=1 串行） |
| `Logger` | 框架日志出口（`[GoTenon]` 头），为 nil 时静默 |
| `ErrorCode` | 稳定错误分类：`INVALID_PLUGIN` / `DUPLICATE` / `NOT_PROVIDED` / `TIMEOUT` 等，配合 `IsCode` 判断 |

## Manager API 速查

| API | 语义 |
|---|---|
| `Register(p, cfg)` | 查重 → `Register` 钩子 → 入表 → 环检测 → `Loader.IntoRegister`；任一步失败回滚 |
| `Delete(name)` | 卸载并移除；仍有已装载依赖者时拒绝 |
| `Enable(name)` | 置启用位 → `Loader.Enable` → 收敛：依赖闭包先行装载 |
| `Disable(name)` | 清启用位 → `Loader.Disable` → 收敛：引用归零者自动卸载 |
| `Get(name)` | 按插件名读取 `*PluginRuntime` |
| `Update(name, cfg)` | 替换配置；已装载则卸载后按新配置重载（会更换上下文） |
| `Available(name)` | 任一已装载插件的上下文槽位提供了该服务（值非空） |
| `Send(msg)` | 把消息路由给目标插件的 `DealWithMessage` |

宿主内嵌时用 `NewManager` 直接持有实例；独立启动时用 `Run` 装配默认管理器，
之后通过包级 `PluginManager` 访问：

```go
m := GoTenon.NewManager(root)          // 内嵌
err := GoTenon.Run(                    // 独立启动
	GoTenon.WithRoot(root),
	GoTenon.WithLogger(logger),
	GoTenon.WithPlugins(&a{}, &b{}),
	GoTenon.WithEnabled("a"),
)
```

## 项目结构

| 文件 | 职责 |
|---|---|
| `scope.go` | 上下文树、作用域（Isolate / IsolateLabel / Intercept）、Info |
| `effect.go` | 可逆副作用：`Disposer`、effect scope、逆序回收 |
| `plugin.go` | `PluginInfo` 插件契约与可选接口（`Contextual` / `FunctionOffer`） |
| `runtime.go` | `PluginTable` 与 `PluginRuntime` 运行时状态 |
| `manager.go` | 中央管理器：注册 / 启停 / 读取更新 / 可用性 / 消息与信号调度 |
| `deps.go` | 依赖图：建边、环检测、加载闭包、依赖就绪判断 |
| `schedule.go` | 收敛调度：装载 / 卸载波次 |
| `lifecycle.go` | 单个插件的装载卸载、上下文清理、并行与超时 |
| `message.go` | 内核消息模型（`MessageType` / `Message`）与默认系统处理器 |
| `signal.go` | 信号表：组件向内核表达意图（主动关闭 / 申请启停 / 故障上报等） |
| `index.go` | 内核私有索引表：能力 / 状态索引、服务发现与订阅 |
| `innerio.go` | `InnerIO`：组件间通信接口 |
| `loader.go` | `Loader` 扩展钩子 |
| `logger.go` | 日志出口 |
| `errors.go` | 错误码与 `CordisError` |
| `run.go` | `Run` 入口与 `Option` |
| `example/game_example/` | 独立模块示例：游戏主进程模拟（`main.go` + `plugin.go`，经 `replace` 引用根模块） |
| `example/web_example/` | 独立模块示例：在线服务网站 + 可撤销路由 / gRPC 微服务扩展（按职责拆分：`base.go` / `kernel_http.go` / `kernel_grpc.go` / `kernel_discovery.go` / `users.go` / `admin.go` / `ratelimit.go` / `greeter.go` / `gateway.go` / `watcher.go` + `main.go` + `service.go`） |
| `example/microservice_example/` | 独立模块示例：微服务插件版（服务发现 / 负载均衡 / 鉴权限流熔断追踪拦截器插件 / 多实例灰度，`DESIGN.md` + `main.go` + `plugin.go` + `service.go`） |

## 设计原则

1. **一切注册皆副作用**：注册 API 都登记逆操作，卸载即回滚，不依赖插件手写清理。
2. **子继承父、永不修改父**：作用域只向下扩展，隔离发生在派生边界。
3. **失败隔离**：单个 disposer 失败不阻断其余；框架自身 panic 转为 error。
4. **依赖图必须是 DAG**：这是拓扑装载顺序与无死锁的前提。
5. **收敛后返回**：公开 API 返回时依赖图处于稳定态，不存在后台震荡。
6. **显式优于魔法**：不用 Proxy / 反射做属性拦截，上下文显式传递。

## 路线图

当前处于 **v0.1 内核成型**：上下文作用域、插件生命周期、依赖收敛、
可逆副作用、消息管道、Loader / Logger / 错误体系均已落地。

| 版本 | 主题 | 关键内容 |
|---|---|---|
| v0.2 | 内核正确性 | 测试与 `-race`、修复装载超时竞态、显式状态机、并发模型、事务化 Update、运行错误恢复 |
| v0.3 | 契约与 API 收敛 | `PluginInfo` / `Loader` 定型、错误码启用、上下文 kv、启动入口统一、模块路径规范化 |
| v0.4 | 服务与配置 | 服务读写 API、配置合并与校验、依赖变化重载 |
| v0.5 | 事件与消息 | 消息超时/异步语义、事件订阅、管道随插件回收 |
| v0.6 | 治理与可观测 | 引用计数、级联禁用、Inspect 快照、依赖图导出 |
| v1.0 | 稳定 | API 冻结、基准与回归、示例集与文档完备 |

详细的功能清单、技术债与验收标准见 [docs/ROADMAP.md](docs/ROADMAP.md)。

## 开发

```bash
go build ./...          # 构建内核
go vet ./...            # 静态检查
cd example/game_example; go run .    # 运行示例（独立模块）
```

测试用例待补，是路线图 v0.2 的首要目标。

## 文档

- [docs/PLUGIN_GUIDE.md](docs/PLUGIN_GUIDE.md) —— 插件系统设计指南:如何设计规范插件、如何使用与调用内核功能
- [docs/architecture.html](docs/architecture.html) —— 架构图
- [docs/ROADMAP.md](docs/ROADMAP.md) —— 功能规划与里程碑
- [example/web_example/DESIGN.md](example/web_example/DESIGN.md) —— 在线服务网站 + 路由 / gRPC 微服务扩展设计文档
- [example/microservice_example/DESIGN.md](example/microservice_example/DESIGN.md) —— 微服务插件版设计文档(发现 / 负载均衡 / 拦截器 / 熔断 / 追踪)