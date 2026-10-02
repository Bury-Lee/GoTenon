# GoTenon 插件系统设计指南

> 面向插件作者与宿主开发者的实践文档。
> 讲两件事:**如何设计一个规范的插件/组件**,以及**如何使用与调用内核功能**。
> 配套代码:`example/game_example`(内核四件套范式)、`example/web_example`(分文件工程化示例)、
> `test/`(可运行的验证程序)。

---

## 1. 设计理念

GoTenon 把插件框架当成一个**微型操作系统**来设计:

| 概念 | OS 类比 | 内核侧 |
|---|---|---|
| 组件(Plugin) | 进程 / 驱动 | `PluginRuntime` |
| `Status()` | `/proc` 状态寄存器 | 状态索引 |
| `Message{Name,Type,Data}` | IPC / 系统调用 | 消息总线 |
| `FunctionOffer` | 能力 / 系统调用表 | 能力索引 |
| `Signal` | 信号 / 中断 | 内核动作 |
| `GoTenonContext` | 进程地址空间 | 作用域树 |

四条不可动摇的原则:

1. **一切注册皆副作用**:注册 API 都登记逆操作,卸载即精确回滚,不靠手写清理。
2. **子继承父、永不修改父**:作用域只向下扩展,隔离发生在派生边界。
3. **收敛后返回**:公开 API 返回时依赖图处于稳定态,不存在后台震荡。
4. **显式优于魔法**:不用反射/代理,上下文显式传递,状态显式声明。

---

## 2. 内核模型速览

### 2.1 上下文树与作用域(`scope.go`)

```go
root := GoTenon.New("app")
child := root.Extend("child")          // 派生子上下文;子继承父,永不改父

root.SetInfo("env", "dev")             // 本层运行时信息
v, ok := child.GetInfo("env")          // 本层没有则沿祖先链向上找

root.Isolate("svc/db")                 // 私有槽:该子树内独立解析
root.SlotOf("svc/db").Value = db        // 绑定服务实例

root.IsolateLabel("cache", "shared")   // 共享槽:相同 label 复用同一槽

root.Intercept("http", cfg1)           // 配置层
child.Intercept("http", cfg2)
layers := child.Config("http")          // root-first 层序列,后层覆盖前层
```

规则:`SlotOf` 解析优先级为 **本层私有槽 > 祖先私有槽 > label 共享槽**。插件的
`ctx` 是其挂载点的子上下文,插件在 `Apply` 期写入的槽位随卸载自动清理。

### 2.2 可逆副作用(`effect.go`)

```go
ctx.Register(func() error { return conn.Close() })       // 卸载时逆序回收

dispose, err := ctx.Effect(func() error {                // 一组注册 = 一个事务
    ctx.Register(...)
    if bad { return err }                                 // body 失败 → 整组立即回滚
    return nil
}, "connect")
_ = dispose                                              // 也可提前手动回收
```

`Disposer` 幂等、可并发、panic 被转为 error;`Effect` 嵌套形成可逆序拆解的回收树。

### 2.3 组件契约(`plugin.go`)

```go
type PluginInfo interface {
    Name() string
    Desc() map[string]string
    Inject() []string                         // 依赖的组件名列表(硬依赖)
    Status() *map[string]any                  // 自管的动态运行状态;nil = 不可用
    Register() error                          // 写入插件表之前
    Apply(ctx *GoTenonContext, cfg any) error // 依赖就绪后;期间注册归本组件所有
    Start() error
    Run() error
    DealWithMessage(Message) error            // 内核原封投递的消息
    End() error                               // 卸载前:资源回收/持久化
}
```

可选接口(按需实现,不强制):

```go
type Contextual interface {                        // 装载可协作取消
    ApplyContext(ctx context.Context, gctx *GoTenonContext, cfg any) error
}
type FunctionOffer interface {                     // 能力声明与调用
    Function() map[string]any                      // 能力描述表(推荐 MCP 风格)
    ExecuteFunction(any)                           // 解码并执行
}
type InnerIO interface {                           // 组件间通信(向内核申请通道)
    DealWith(From string, Message Message) error
    SendTo(Des string, Message Message) error
}
```

### 2.4 生命周期状态机(`runtime.go`)

```
Disabled --Enable--> Pending --依赖就绪--> Loading --成功--> Ready
                        ↑                    └--失败--> Failed
                        └--仍启用-- Unloading <--Disable/Update--
```

- `Enable` 是**意图位**(bool),`State` 是**生命周期状态**,二者分离;
- `Kept`=未启用但被依赖者持有;`Loaded()`=Ready/Kept/Unloading;
- `Err`/`Missing` 用于诊断,不自动重试,`Update`/`Enable` 可恢复。

### 2.5 依赖图与收敛调度(`deps.go` `schedule.go`)

- `Inject()` 返回的名字按**组件名**解析,注册时自动建边;
- 注册与启用前做 DFS 环检测,成环立即拒绝;
- `Enable` 计算加载闭包,按依赖波次并行装载(缺省并行度 4);
- 依赖未就绪的组件保持 `Pending` 并在 `Missing` 记录缺口;
- 禁用后引用归零的组件自动卸载,**依赖者先走**。

### 2.6 消息与信号(`message.go` `signal.go`)

```go
type Message struct {
    Name string      // 目标;空 = 发往系统/内核
    Type MessageType // 类型码:信封标签
    Data any         // 载荷
}
```

`Type` 号段:**0–15 系统保留,16+ 组件自定义**。

| 值 | 常量 | Data 约定 |
|---|---|---|
| 0 | `TypeUnknown` | 未指定 |
| 1 | `TypeContext` | `context.Context` |
| 2 | `TypeMessage` | `*Message`(嵌套/转发) |
| 3 | `TypeRaw` | `any`(自定义/能力载荷) |
| 4 | `TypeSignal` | `*SignalRequest` |
| 5 | `TypeReply` | `*Message` |
| 6 | `TypeJSON` | `[]byte` 或 map |
| 7 | `TypeIndex` | `IndexEvent`(索引变更通知) |
| 16+ | `TypeCustomBase` | 组件自定义 |

分发规则(`DefaultMessageProcesser`):

- `Name != ""` → 内核只做查表与**原封投递**,由目标组件的 `DealWithMessage` **自行解包**;
- `Name == ""` → 走**系统分支**,由内核按 `Type` 解析(信号等)。

信号是组件向内核表达的意图:

| 信号 | 内核动作 |
|---|---|
| `SHUTDOWN` | `Disable(From)`(主动关闭自身) |
| `REQ_STOP` / `REQ_ENABLE` / `REQ_RELOAD` / `REQ_DELETE` | 对 `Target` 执行 Disable/Enable/Update/Delete |
| `FAULT` / `PANIC` | 标记 `From` 为 `Failed`(故障隔离) |
| `DECLARE` / `RETRACT` | 刷新 / 摘除索引条目 |
| `READY` | 就绪上报(仅记录) |

### 2.7 能力与索引(`index.go`)

```go
type CapabilityEntry struct {
    Plugin, Name string
    Desc         map[string]any
    Available    bool
}
type IndexEvent struct{ Kind, Plugin string } // up / down / update
type Subscription struct {
    Subscriber string
    Filter     func(IndexEvent) bool
}
```

内核私有索引表保存「能力 / 状态 / 可用性」,由生命周期事件**消息驱动**地原子更新;
对外只暴露查询(`Capabilities`/`Discover`/`Status`)与订阅(`Subscribe`)。

---

## 3. 怎么写一个规范的组件

### 3.1 最小骨架

```go
type greeter struct {
    basePlugin            // 自己的 base,提供 PluginInfo 默认实现(见下)
    mu    sync.Mutex
    ready bool
}

func (p *greeter) Name() string            { return "greeter" }
func (p *greeter) Inject() []string        { return []string{"http"} } // 依赖组件名
func (p *greeter) Desc() map[string]string { return map[string]string{"provides": "demo.Greeter"} }

// Status 是组件自管的状态:内核只读,用于索引与可观测。nil 表示不可用。
func (p *greeter) Status() *map[string]any {
    p.mu.Lock(); defer p.mu.Unlock()
    if !p.ready { return &map[string]any{"state": "pending"} }
    return &map[string]any{"state": "ready", "version": "v1"}
}

func (p *greeter) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
    // 1) 从祖先槽位解析依赖(不要回调 Manager!)
    v, err := service(ctx, "svc/grpc")
    if err != nil { return err }
    srv := v.(*GRPCServer)
    // 2) 一切注册皆可逆:用 ctx.Register 登记撤销动作
    ctx.Register(srv.RegisterService("demo.Greeter/SayHello", handler))
    // 3) 把自己的服务发布到本层槽位,供后代/同层经祖先解析
    ctx.Isolate("svc/greeter")
    ctx.SlotOf("svc/greeter").Value = p
    p.mu.Lock(); p.ready = true; p.mu.Unlock()
    return nil
}

func (p *greeter) DealWithMessage(m GoTenon.Message) error {
    switch m.Type {
    case GoTenon.TypeIndex:
        ev := m.Data.(GoTenon.IndexEvent) // 索引变更通知
        _ = ev
    case GoTenon.TypeRaw:
        // 自己的载荷语义自己解释
    }
    return nil
}

// 能力声明(可选):推荐 MCP 风格 {description,inputSchema}
func (p *greeter) Function() map[string]any {
    return map[string]any{
        "greeter.sayHello": map[string]any{
            "description": "打招呼",
            "inputSchema": map[string]any{
                "type": "object",
                "properties": map[string]any{"name": map[string]any{"type": "string"}},
            },
        },
    }
}
func (p *greeter) ExecuteFunction(any) {}

// base 提供 PluginInfo 默认实现,组件只覆写关心的钩子
type basePlugin struct{}
func (basePlugin) Desc() map[string]string                  { return nil }
func (basePlugin) Inject() []string                         { return nil }
func (basePlugin) Status() *map[string]any                  { return nil }
func (basePlugin) Register() error                          { return nil }
func (basePlugin) Apply(*GoTenon.GoTenonContext, any) error { return nil }
func (basePlugin) Start() error                             { return nil }
func (basePlugin) Run() error                               { return nil }
func (basePlugin) End() error                               { return nil }
func (basePlugin) DealWithMessage(GoTenon.Message) error    { return nil }
```

### 3.2 硬性纪律(否则会死锁)

> **组件在装载期(`Apply` / `Start` / `Run`)绝不回调 Manager。**

`Enable`/`Disable`/`Update` 会持有 Manager 锁直到依赖闭包收敛完成,而组件的装载钩子
就在其中执行。若组件在 `Apply` 里调用 `m.Get/Enable/Send/Discover/Subscribe/Signal`,
会**重入自身的锁**而死锁,最终以装载超时告终。

正确姿势:

- 装载期只用 **`ctx` 槽位**解析依赖、只做**可逆注册**;
- 与内核交互(发现能力、订阅、发信号)放在 **消息回调**(`DealWithMessage`)或在
  宿主 `main` 中、`Enable` 之后执行;
- 常驻协程在 `Run` 里 `go` 出去,真正的内核调用发生在协程稍后执行时。

### 3.3 依赖声明

- `Inject()` 返回**组件名**列表,构成有向无环图;
- 依赖未就绪时组件保持 `Pending`,不会报错;依赖补注册后自动装载;
- 不要用 `Inject` 表达"仅排序"意图;它同时意味着硬依赖与自动级联。

### 3.4 资源生命周期

- 任何外部资源(连接、监听、定时器、协程停止信号)都必须登记为 `Disposer`;
- 一组相关注册用 `Effect` 包成一个事务,失败整组回滚;
- 卸载顺序:`End()` → 逆序回收全部 `Disposer` → 递归清理上下文。

---

## 4. 怎么使用与调用内核功能

### 4.1 启动

```go
// 方式一:宿主内嵌,直接持有 Manager
root := GoTenon.New("app")
root.Isolate("svc/db"); root.SlotOf("svc/db").Value = db
m := GoTenon.NewManager(root)
m.Loader = myLoader
m.Logger = GoTenon.LoggerFunc(logf)

// 方式二:独立启动,装配默认管理器
err := GoTenon.Run(
    GoTenon.WithRoot(root),
    GoTenon.WithLogger(logger),
    GoTenon.WithPlugins(&a{}, &b{}),
    GoTenon.WithEnabled("a"),
)
```

### 4.2 生命周期

```go
m.Register(p, cfg)   // 登记:查重 → Register 钩子 → 入表 → 环检测 → Loader.IntoRegister
m.Enable("a")        // 置启用位 → 收敛:依赖闭包先行装载
m.Disable("a")       // 清启用位 → 收敛:引用归零者自动卸载
m.Update("a", cfg)   // 替换配置;失败事务化回滚旧配置
m.Delete("a")        // 卸载并移除;仍有已装载依赖者时拒绝
rt, ok := m.Get("a") // 读取运行时:State/Enable/Loaded/Missing/Dependence/...
m.Available("svc/x") // 任一已装载插件的槽位提供了该服务
```

### 4.3 消息

```go
// 目标组件:内核原封投递,组件自行解包
m.Send(GoTenon.Message{Name: "render", Type: GoTenon.TypeRaw, Data: "frame"})

// 系统消息(Name 为空):内核按 Type 解析
reply, err := m.Dispatch(&GoTenon.Message{Name: "", Type: GoTenon.TypeContext, Data: ctx})
```

宿主侧建议用 `TypeRaw` 传自定义载荷,或用 16+ 自定义号段区分业务语义;
组件侧在 `DealWithMessage` 里按 `Type` 分派。

### 4.4 信号

```go
// 组件或宿主向内核投递信号
sig, _ := m.Signals.Lookup("SHUTDOWN")
m.Signal(GoTenon.SignalRequest{From: "storage", Signal: sig})

// 申请关闭其他组件
stop, _ := m.Signals.Lookup("REQ_STOP")
m.Signal(GoTenon.SignalRequest{From: "supervisor", Target: "render", Signal: stop})

// 自定义信号
m.Signals.Register(GoTenon.Signal{Kind: GoTenon.SigCustom, Name: "RELOAD_CFG", Desc: "..."})
```

### 4.5 能力与状态

```go
caps, ok := m.Capabilities("render")             // 某组件声明的能力表
st, ok := m.Status("render")                     // 某组件上报的运行状态
for _, e := range m.Discover(func(e GoTenon.CapabilityEntry) bool {
    return e.Available                            // 服务发现:只看可用能力
}) {
    _ = e
}
```

### 4.6 订阅(索引走消息,不建函数通道)

```go
// 订阅只登记「兴趣」;内核在索引变化时把 IndexEvent 以 TypeIndex 消息
// 投递给订阅组件的 DealWithMessage。
stop, err := m.Subscribe(GoTenon.Subscription{
    Subscriber: "watcher",
    Filter:     func(e GoTenon.IndexEvent) bool { return e.Plugin == "render" },
})
defer stop()

// watcher 组件里:
func (p *watcher) DealWithMessage(msg GoTenon.Message) error {
    if msg.Type == GoTenon.TypeIndex {
        ev := msg.Data.(GoTenon.IndexEvent)   // ev.Kind: up/down/update
        _ = ev
    }
    return nil
}
```

### 4.7 扩展点

| 扩展点 | 说明 |
|---|---|
| `Loader` | 按组件名回调:`IntoRegister`/`Delete`/`Enable`/`Disable`/`Hook`;`Timeout` 提供装载超时 |
| `ConcurrencyLoader` | 可选:定制并行装载度(`<=1` 串行) |
| `Logger` / `LoggerFunc` | 框架日志出口;nil 静默 |
| `MessageProcesser` | 可替换内核默认消息处理器 |
| `ErrorCode` + `IsCode` | 稳定错误分类判断 |

---

## 5. 一个完整的最小闭环

```go
package main

import (
    "fmt"
    "GoTenon"
)

type greeter struct{ basePlugin }

func (p *greeter) Name() string { return "greeter" }
func (p *greeter) Function() map[string]any {
    return map[string]any{"greeter.hello": map[string]any{"description": "打招呼"}}
}
func (p *greeter) ExecuteFunction(any) {}
func (p *greeter) DealWithMessage(m GoTenon.Message) error {
    if m.Type == GoTenon.TypeRaw {
        fmt.Println("hello,", m.Data)
    }
    return nil
}

func main() {
    if err := GoTenon.Run(GoTenon.WithPlugin(&greeter{}, nil)); err != nil {
        panic(err)
    }
    _ = GoTenon.PluginManager.Send(GoTenon.Message{
        Name: "greeter", Type: GoTenon.TypeRaw, Data: "world",
    })
}
```

---

## 6. 规范清单(Do / Don't)

**Do**

- `Status()` 返回快照,并对装载前/卸载后的空值做防护;
- `Function()` 用 MCP 风格描述能力(格式开放,推荐而非强制);
- 外部资源一律 `ctx.Register` / `ctx.Effect`;
- 依赖用 `Inject()` 显式声明,让调度器负责装载顺序;
- 组件间尽量通过槽位(装载期)与消息/信号(运行期)解耦。

**Don't**

- ❌ 在 `Apply`/`Start`/`Run` 里调用 Manager(重入死锁);
- ❌ 在组件间直接持有对方指针跨生命周期使用(可能悬垂,用槽位/消息);
- ❌ 手写清理逻辑代替 `Disposer`(易漏、顺序难保证);
- ❌ 用 `Inject` 表达"只要顺序、不要依赖";
- ❌ 让 `Status`/`Function` 阻塞或产生副作用(它们是只读查询)。

---

## 7. 常见问题

**Q: 组件之间怎么互相拿到服务?**
装载期:提供方在 `Apply` 里把自己写进 `ctx` 槽位;消费方在共同祖先可见范围内用
`ctx.SlotOf` 解析(兄弟组件需经共同祖先,或由宿主预挂)。运行期:用消息/信号,或
`InnerIO` 申请通道。避免跨生命周期持有裸指针。

**Q: 为什么 `Subscribe` 收不到消息?**
订阅写入的是「订阅者组件名」,内核把消息投递给该组件的 `DealWithMessage`。若订阅者
未注册/未装载,投递会失败(被记录为 debug)。请先 `Enable` 订阅者组件。

**Q: 装备超时后为什么 `Apply` 仍在跑?**
Go 无法强杀协程。超时后内核会释放锁并回滚上下文,晚到的 `ctx.Register` 返回
`ErrInactiveEffect`(不再产生逃逸副作用)。真正耗时的工作应实现 `Contextual`
以便协作取消。

**Q: 索引为什么是"消息驱动"的?**
每次注册/上下线/声明能力都会原子更新内核索引并排入一条事件;内核随后在**锁外**
把 `IndexEvent` 以 `TypeIndex` 消息投递给订阅者。这样查询永远走索引,订阅永远走消息,
二者不互相耦合。

---

## 8. 快速参考

- 错误码:`ErrInvalidPlugin` / `ErrDuplicate` / `ErrNotProvided` / `ErrTimeout` /
  `ErrInactiveEffect` / `ErrDisposed` / `ErrConfigInvalid` /
  `ErrMessageTypeMismatch` / `ErrSignalUnhandled` / `ErrMessageLoop`;用 `GoTenon.IsCode(err, code)` 判断。
- 状态:`Disabled` / `Pending` / `Loading` / `Ready` / `Failed` / `Kept` / `Unloading`。
- 运行验证:`go run ./test/all`(消息 / 加载 / 依赖 / 能力,61 项断言)。
