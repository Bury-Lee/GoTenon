# 微服务插件版:用 GoTenon 构建可扩展的 gRPC 微服务

> 本文是 `example/microservice_example/` 的完整设计说明,自包含:讲清「插件库怎么设计、
> gRPC 微服务怎么被插件扩展」,并逐层给出可运行实现。
> 配套源码:`service.go`(微服务内核契约)、`plugin.go`(插件)、`main.go`(宿主演练)。
> 运行:`cd example/microservice_example && go run .`

---

## 目录

1. [问题与目标](#1-问题与目标)
2. [设计总览:三件套 + 共享槽](#2-设计总览三件套--共享槽)
3. [分层架构与依赖图](#3-分层架构与依赖图)
4. [GoTenon 原语映射表](#4-gotenon-原语映射表)
5. [微服务内核契约(service.go)](#5-微服务内核契约servicego)
6. [gRPC 扩展模型:拦截器即插件](#6-grpc-扩展模型拦截器即插件)
7. [关注点插件详解](#7-关注点插件详解)
8. [逐文件实现讲解](#8-逐文件实现讲解)
9. [运行演示](#9-运行演示)
10. [如何新增一个微服务 / 关注点](#10-如何新增一个微服务--关注点)
11. [与 web_example 的关系、边界与接入真实 gRPC](#11-与-web_example-的关系边界与接入真实-grpc)

---

## 1. 问题与目标

要在 Go 进程里构造一个**微服务系统**,并让「扩展」成为常态而非改造:

1. **服务可扩展**:新增一个微服务 = 新增一个插件,不改 gRPC 内核、不改已有服务;
2. **能力可叠加**:鉴权、限流、熔断、链路追踪都是**插件**,可独立上下线、独立调参;
3. **多实例 / 负载均衡**:同一逻辑服务可有多个实例,客户端自动选点;
4. **服务间调用**:服务 A 调用服务 B,元数据(token、trace id)自动透传;
5. **热插拔 / 灰度**:实例上下线即时生效,发现中心自动摘除 / 登记;
6. **失败隔离**:单个插件装载失败不影响其余,半成品自动回滚。

GoTenon 已提供最小内核原语,本文所有设计都建立在它们之上,**不修改内核、不引入第三方依赖**。

---

## 2. 设计总览:三件套 + 共享槽

插件库**不发明新的生命周期**,而是把「gRPC 服务表 / 拦截器链 / 服务发现表」翻译成 GoTenon 原语:

| 机制 | GoTenon 原语 | 解决的问题 |
|---|---|---|
| **共享槽位** | `Isolate` + `SlotOf` + `IsolateLabel` | 插件如何拿到同一个 gRPC Server / Registry / Client |
| **可逆副作用** | `Register(Disposer)` + `Effect` | 服务注册、拦截器叠加、实例登记如何自动撤销、成组回滚 |
| **依赖声明** | `Inject()` | 插件按什么顺序装载(内核对→客户端→关注点→业务服务) |

一句话:**扩展 = 往依赖图里加节点 + 在 `Apply` 期登记 Disposer。内核与既有插件零改动。**

### 2.1 一个必须记住的边界:插件上下文互为兄弟

每个插件的上下文都是 `root.Extend(name)` 得到的 **root 的兄弟**;`SlotOf` **只沿祖先链解析**。
因此:

- 插件在自己的上下文里 `Isolate("svc/x")`,**兄弟插件看不到**;
- 想让所有插件共享一个运行时对象(如 `Client`、`Tracer`),必须由**宿主在 root 预声明**:

```go
// 宿主:声明由插件写入、全体可见的共享槽
root.IsolateLabel("svc/client", "shared")
```

```go
// client 插件:写入的其实是 root 的共享槽(祖先链上同 label 的槽)
ctx.IsolateLabel("svc/client", "shared")
ctx.SlotOf("svc/client").Value = cli
ctx.Register(func() error { ctx.SlotOf("svc/client").Value = nil; return nil })
```

本示例中 `svc/client`、`svc/balancer`、`svc/trace` 都走这条路径;而 `svc/grpc`、`svc/registry`、
`svc/router` 是宿主直接挂到 root 的**私有槽**。

---

## 3. 分层架构与依赖图

```
┌─────────────────────────────────────────────────────────────┐
│ Host(宿主)                                                  │
│   root 上预挂内核服务:                                       │
│     svc/registry → *Registry    (服务发现)                   │
│     svc/grpc     → *GRPCServer  (进程内 gRPC,多实例服务表)   │
│     svc/router   → *Router      (HTTP 路由表)                │
│   并声明共享槽:svc/client / svc/balancer / svc/trace         │
└───────────────┬─────────────────────────────────────────────┘
                │ root.Extend(name)
   ┌────────────┴─────────────┐
   │ 内核插件                  │  http / grpc / discovery
   │  发布内核服务、装默认拦截器 │
   └────────────┬─────────────┘
                │
   ┌────────────┴─────────────┐
   │ 客户端内核插件            │  client(Registry 解析 + 轮询 + 客户端拦截器链)
   └────────────┬─────────────┘
                │
   ┌────────────┴────────────────────────────┐
   │ 关注点插件(叠加 gRPC 能力)             │
   │  tracing → ratelimit → auth(服务端链)   │
   │  tracing → circuitbreaker(客户端链)     │
   └────────────┬────────────────────────────┘
                │
   ┌────────────┴────────────────────────────┐
   │ 业务微服务插件                           │
   │  users / orders / greeter-a / greeter-b │
   └────────────┬────────────────────────────┘
                │
   ┌────────────┴─────────────┐
   │ 网关插件 gateway          │  HTTP → gRPC(经 Client 选点)
   └──────────────────────────┘
```

### 3.1 依赖图(DAG)

```
grpc   discovery
  ├────────┴── client
  │              ├── tracing ─┬── ratelimit ── auth
  │              │            └── circuitbreaker
  │              ├── orders ── users
  │              └── gateway
  ├── users
  ├── orders
  ├── greeter-a
  └── greeter-b
http ── gateway
```

**扩展单位是节点**:要加「重试」就加一个 `retry` 节点 `Inject(["client"])`;要加「配置中心」就加
一个 `config` 节点。内核与既有插件都不改 —— 这是「良好扩展」的判据。

### 3.2 关心顺序:为什么关注点用链式 Inject

服务端拦截器链的叠加顺序 = 插件装载顺序。若 `tracing`、`ratelimit`、`auth` 都只 `Inject(["grpc"])`,
它们会在**同一并行波次**装载,注册顺序不确定。因此本示例显式串起来:

```
tracing → ratelimit → auth         服务端链:日志 → tracing → ratelimit → auth → handler
tracing → circuitbreaker           客户端链:tracing → circuitbreaker → 选点
```

这既让顺序确定,也顺带演示了「依赖表达组合关系」;代价是关一个会级联(符合预期)。

---

## 4. GoTenon 原语映射表

| 微服务需求 | GoTenon 原语 | 对应实现 |
|---|---|---|
| 插件共享 gRPC Server / Registry | root 私有槽 `Isolate` + `SlotOf` | 宿主预挂 `svc/grpc`、`svc/registry` |
| 插件共享运行时构建的 Client / Tracer | `IsolateLabel` 共享槽 | `client` / `tracing` 写入 root 共享槽 |
| 服务注册、拦截器叠加、实例登记 | `Register(Disposer)` | `GRPCServer.RegisterService` / `Use` / `Registry.Register` |
| 方法 + 实例登记要么全成要么全回滚 | `Effect` 事务组 | `users` / `orders` / `greeter` 的 `ctx.Effect` |
| 关注点按确定顺序叠加 | `Inject()` 链 | `tracing → ratelimit → auth` |
| 拦截器热调参、实例上下线 | `Enable / Disable / Update` 收敛 | 第 6~8 节演练 |
| 并行装载互不依赖的微服务 | `ConcurrencyLoader` | `microLoader.Concurrency()` |

---

## 5. 微服务内核契约(service.go)

`service.go` 把微服务拆成五个一等能力,全部返回 `GoTenon.Disposer`:

| 类型 | 职责 | 关键方法(返回 Disposer) |
|---|---|---|
| `Router` | 可撤销 HTTP 路由表;每请求读快照 | `Handle` / `Use` |
| `Registry` | 服务名 → 多实例 `{Addr, Version}` | `Register` |
| `Balancer` | 按服务名**轮询**选点 | `Pick` |
| `GRPCServer` | 多实例服务表 + 一元拦截器链 | `RegisterService(addr, full, h)` / `Use` |
| `Client` | Registry 解析 → Balancer 选点 → 客户端拦截器链 | `Use` / `Invoke` |
| `Tracer` | 收集 span,供 `/debug/traces` | `Add` / `Snapshot` |

### 5.1 多实例:handler 以 (实例地址, 方法) 为键

```go
func handlerKey(addr, fullMethod string) string { return addr + "\x00" + fullMethod }

func (s *GRPCServer) RegisterService(addr, fullMethod string, h UnaryHandler) GoTenon.Disposer
func (s *GRPCServer) Invoke(ctx context.Context, addr, fullMethod string, payload []byte) (*GRPCResponse, error)
```

同一方法 `demo.Greeter/SayHello` 可在 `inproc://greeter-a` 与 `inproc://greeter-b` 上并存,
这正是多实例与负载均衡的基础(web_example 的单实例实现只能有一个 handler)。

### 5.2 客户端:选点 + 客户端拦截器链

```go
func (c *Client) Invoke(ctx context.Context, service, method string, payload []byte) (*GRPCResponse, error) {
    addr, err := c.lb.Pick(service)          // 1. 解析 + 轮询选点
    if err != nil { return nil, err }
    // 2. 组装客户端拦截器链(tracing / circuitbreaker ...)
    // 3. 终端:向选中实例发起一元调用
    return chain(ctx, &GRPCRequest{Service: service, Method: method, Payload: payload})
}
```

**服务端不知道客户端是谁,客户端也不知道有哪些实例** —— 二者只通过 `Registry` + `Client` 间接咬合。

### 5.3 元数据沿 context 透传

```go
type metaKey string
const (
    metaTraceID  metaKey = "gotenon.trace_id"
    metaToken    metaKey = "gotenon.token"
    metaInstance metaKey = "gotenon.instance"
)
```

网关把 `?token=` 写进调用 context;`Client.Invoke` 原样向下传;`orders` 调用 `users` 时复用
同一个 context,于是 token 与 trace id **自动跨服务透传**,无需手工搬运。

---

## 6. gRPC 扩展模型:拦截器即插件

真实 gRPC 的扩展点是一元拦截器。本示例把 `UnaryInterceptor` 暴露为内核契约,插件通过
`GRPCServer.Use` / `Client.Use` 叠加,卸载即摘除:

| gRPC 概念 | 插件化后 | 逆操作 |
|---|---|---|
| `Server.RegisterService(desc, impl)` | `ctx.Register(srv.RegisterService(addr, full, h))` | 从服务表摘除 |
| `Server.UnaryInterceptor` | `ctx.Register(srv.Use(fn))` | 从服务端链摘除 |
| `Client.UnaryInterceptor` / 熔断 / 重试 | `ctx.Register(cli.Use(fn))` | 从客户端链摘除 |
| 实例注册(etcd / Consul) | `ctx.Register(reg.Register(service, addr, ver))` | 从发现中心摘除 |
| `Serve(ln)` | `Apply` 里 `go Serve` + `Shutdown` Disposer | 优雅停机 |

### 6.1 服务端链 vs 客户端链

```
HTTP 网关
   │  ?token=... → ctx
   ▼
Client(tracing → circuitbreaker)      ← 熔断、重试、灰度在这里
   │  Registry 选点
   ▼
GRPCServer(日志 → tracing → ratelimit → auth)  ← 鉴权、限流、服务端追踪在这里
   ▼
业务 handler
```

**关注点落在哪一侧是有讲究的**:限流/鉴权在服务端(保护自己),熔断/重试在客户端(保护调用方)。
插件库把两侧都做成可插拔的链,使用者按语义选择。

---

## 7. 关注点插件详解

### 7.1 tracing(双端拦截器 + 跨服务透传)

```go
// 服务端:读取(或生成)trace id,记录一次 server span
ctx.Register(srv.Use(func(c context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
    traceID := metaOf(c, metaTraceID)
    if traceID == "" { traceID = newTraceID(); c = withMeta(c, metaTraceID, traceID) }
    start := time.Now()
    resp, err := next(c, req)
    tracer.Add(Span{TraceID: traceID, Kind: "server", ...})
    return resp, err
}))

// 客户端:若无 trace id 则生成,保证跨服务同一条链路
ctx.Register(cli.Use(func(c context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
    if metaOf(c, metaTraceID) == "" { c = withMeta(c, metaTraceID, newTraceID()) }
    ...
}))
```

一次 `/api/order` 会产出 4 个 span,共享同一个 trace id:

```
server trace=t-000018 demo.Users/Get      inst=inproc://users-1
client trace=t-000018 demo.Users/Get
server trace=t-000018 demo.Orders/Create  inst=inproc://orders-1
client trace=t-000018 demo.Orders/Create
```

### 7.2 ratelimit(服务端,热调阈值)

```go
ctx.Register(srv.Use(func(c context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
    key := req.Service + "/" + req.Method
    if n := p.incr(key); n > p.limit {
        return nil, rpcErr(errExhausted, "rate limit %d exceeded for %s", p.limit, key)
    }
    return next(c, req)
}))
```

`m.Update("ratelimit", ratelimitConfig{Limit: 2})` 换上下文重载,旧拦截器摘除、计数归零。

### 7.3 auth(服务端,校验调用元数据)

```go
if metaOf(cc, metaToken) != p.token {
    return nil, rpcErr(errUnauthenticated, "invalid token for %s/%s", req.Service, req.Method)
}
```

网关从 `?token=` 取值写入 ctx;缺少 token → `401`;内部 `orders → users` 因 ctx 透传而自动带上。

### 7.4 circuitbreaker(客户端,按服务熔断)

```go
ctx.Register(cli.Use(func(cc context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
    if err := p.before(req.Service); err != nil { return nil, err } // 开路则短路
    resp, err := next(cc, req)
    p.after(req.Service, err) // 连续失败达阈值 → 开路;成功 → closed
    return resp, err
}))
```

`before` 在冷却期外放行一次探测(half-open);`after` 决定回闭或再开。阈值 / 冷却都是 `Update` 可调配置。

---

## 8. 逐文件实现讲解

### 8.1 `service.go` —— 微服务内核契约

| 类型 | 关键点 |
|---|---|
| `Router` | `Handler()` 每请求取快照,热插拔对已启动 Server 立即生效 |
| `Registry` | `Register` 返回 Disposer;`Resolve` 给 LB;`Describe` / `Services` 诊断 |
| `Balancer` | 轮询 `rr[service]`,无实例返回 `UNAVAILABLE` |
| `GRPCServer` | `(addr, fullMethod)` 键;服务端拦截器链 |
| `Client` | 选点 + 客户端拦截器链;`Invoke` 是网关 / 服务间调用的统一入口 |
| `Tracer` | 并发安全收集 span |
| `rpcError` | 稳定错误码,网关据此映射 HTTP 状态 |

### 8.2 `plugin.go` —— 插件

| 插件 | 层 | `Inject` | 作用 |
|---|---|---|---|
| `httpKernel` | Kernel | — | 监听、挂路由表、优雅停机;发布 `svc/http` |
| `grpcKernel` | Kernel | — | 发布 `svc/grpc`;装默认日志拦截器 |
| `discoveryPlugin` | Kernel | — | 发布 `svc/registry` |
| `clientPlugin` | Kernel | `grpc, discovery` | 构建 Client/Balancer,**写入共享槽** |
| `tracingPlugin` | Concern | `grpc, client` | 双端拦截器 + `svc/trace` |
| `ratelimitPlugin` | Concern | `tracing` | 服务端方法级限流 |
| `authPlugin` | Concern | `ratelimit` | 服务端 token 鉴权 |
| `circuitbreakerPlugin` | Concern | `tracing` | 客户端熔断 |
| `usersPlugin` | Service | `grpc, discovery` | `demo.Users/Get` + 实例登记 |
| `ordersPlugin` | Service | `client, users` | `demo.Orders/Create`,内部调 `users` |
| `greeterPlugin` ×2 | Service | `grpc, discovery` | `demo.Greeter/SayHello`,两实例演示 LB/灰度 |
| `gatewayPlugin` | Edge | `http, client` | HTTP → gRPC |

共用底座:

```go
type basePlugin struct{}                       // PluginInfo 默认实现
func service(ctx, name) (any, error)           // 沿祖先链解析共享槽
func statusOf(err error) int                    // gRPC 错误码 → HTTP 状态
```

### 8.3 `main.go` —— 宿主演练

预挂内核服务 → 声明共享槽 → 注册 / 启用 → gRPC 直连 → 网关调用 → 限流 / 熔断 / 灰度 / 追踪 → 优雅停机。
`microLoader` 实现 `Loader` + `ConcurrencyLoader`;`dump()` 打印状态机快照。

---

## 9. 运行演示

```bash
cd example/microservice_example && go run .
```

关键片段(端口为 `:0` 随机分配):

```
===== 3. 微服务拓扑:发现中心与 gRPC 服务表 =====
registry.Services() = [demo.Greeter(2) demo.Orders(1) demo.Users(1)]
grpc.Methods() = [inproc://greeter-a demo.Greeter/SayHello inproc://greeter-b demo.Greeter/SayHello
                  inproc://orders-1 demo.Orders/Create inproc://users-1 demo.Users/Get]

===== 5. HTTP 网关 → gRPC(多实例负载均衡 / 鉴权) =====
  GET /api/greet?name=GoTenon&token=secret  → 200 {"reply":"hello, GoTenon (from a)"}
  GET /api/greet?name=GoTenon&token=secret  → 200 {"reply":"hello, GoTenon (from b)"}
  GET /api/greet?name=GoTenon&token=secret  → 200 {"reply":"hello, GoTenon (from a)"}
  GET /api/users?id=alice&token=secret      → 200 {"reply":"user:alice@v1"}
  GET /api/order?user=alice&token=secret    → 200 {"reply":"order(alice) -> user:alice@v1"}
  GET /api/greet?name=GoTenon               → 401 invalid token

===== 6. 限流热调阈值 =====
  第 1 次 /api/users → 200
  第 2 次 /api/users → 200
  第 3 次 /api/users → 429

===== 7. 客户端熔断 =====
  /api/order?user=boom  ×3 → 502(连续失败,熔断器开路)
  /api/order?user=alice    → 503 circuit open for demo.Orders(短路,未到达服务)
  sleep 300ms
  /api/order?user=alice    → 200(half-open 探测成功,恢复)

===== 8. 服务发现 + 灰度 =====
  registry.Describe(demo.Greeter) = [{inproc://greeter-a v1} {inproc://greeter-b v1}]
  Disable(greeter-a) → /api/greet 只命中 greeter-b
  Disable(greeter-b) → /api/greet 503 no live instance

===== 9. 链路追踪(同一 trace id 贯穿三层) =====
  server trace=t-000018 demo.Users/Get      inst=inproc://users-1
  client trace=t-000018 demo.Users/Get
  server trace=t-000018 demo.Orders/Create  inst=inproc://orders-1
  client trace=t-000018 demo.Orders/Create

===== 10. 卸载与优雅停机 =====
  停机后访问 → 0 connection refused
```

> 第 10 节按「依赖者先走」的顺序禁用,`http` 引用归零后自动卸载,`http.Server.Shutdown` 作为
> Disposer 执行、端口释放,所以停机后再访问是连接拒绝。

---

## 10. 如何新增一个微服务 / 关注点

### 10.1 新增一个业务微服务

```go
type echoPlugin struct{ basePlugin; addr string }

func (p *echoPlugin) Name() string     { return "echo" }
func (p *echoPlugin) Inject() []string { return []string{"grpc", "discovery"} }

func (p *echoPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
    sv, _ := service(ctx, "svc/grpc")
    rv, _ := service(ctx, "svc/registry")
    _, err := ctx.Effect(func() error {
        ctx.Register(sv.(*GRPCServer).RegisterService(p.addr, "demo.Echo/Ping",
            func(c context.Context, req *GRPCRequest) (*GRPCResponse, error) {
                return &GRPCResponse{Payload: []byte("pong:" + string(req.Payload))}, nil
            }))
        ctx.Register(rv.(*Registry).Register("demo.Echo", p.addr, "v1"))
        return nil
    }, "echo-endpoint")
    return err
}
```

再 `m.Register(&echoPlugin{addr: "inproc://echo-1"}, nil)` + `m.Enable("echo")` 即可。
网关只要加一条路由,或由任意服务经 `svc/client` 调用 `demo.Echo/Ping` —— 内核与已有插件不变。

### 10.2 新增一个关注点(拦截器插件)

```go
type retryPlugin struct{ basePlugin }

func (p *retryPlugin) Name() string     { return "retry" }
func (p *retryPlugin) Inject() []string { return []string{"client"} }

func (p *retryPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
    cv, _ := service(ctx, "svc/client")
    ctx.Register(cv.(*Client).Use(func(c context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
        var last error
        for i := 0; i < 3; i++ {
            resp, err := next(c, req)
            if err == nil { return resp, nil }
            last = err
        }
        return nil, last
    }))
    return nil
}
```

**扩展 gRPC 微服务能力 = 写一个插件 + 在 `Apply` 里 `Use`。** 这就是本设计给出的答案。

---

## 11. 与 web_example 的关系、边界与接入真实 gRPC

### 11.1 与 `web_example` 的关系

| 维度 | `web_example` | `microservice_example`(本文) |
|---|---|---|
| 侧重 | HTTP 网站 + 路由扩展 | gRPC 微服务关注点 |
| gRPC 服务表 | 单实例(fullMethod 为键) | **多实例**(addr + fullMethod 为键) |
| 共享机制 | root 私有槽 | 私有槽 + **`IsolateLabel` 共享槽** |
| 客户端 | 无 | `Client`(解析 + 轮询 + 客户端拦截器链) |
| 关注点 | 限流中间件 | 追踪 / 限流 / 鉴权 / **熔断** |
| 追踪 | 无 | 双端 span + 跨服务 trace id 透传 |

两者共享同一套设计哲学(三件套),`microservice_example` 是它在分布式方向的展开。

### 11.2 边界与路线图

| 项 | 现状 | 建议 |
|---|---|---|
| 真实 gRPC | 进程内注册表 | 换成 `google.golang.org/grpc`,建议做成独立 `gotenon-grpc` 模块 |
| 负载均衡策略 | 轮询 | 可插拔:权重 / 最少连接 / 一致性哈希(新写一个节点) |
| 实例健康检查 | 注册即在线,卸载即摘除 | 增加带 TTL 的租约与主动探活 |
| 重试与幂等 | 见 §10.2 示例 | 作为客户端插件叠加,注意幂等键 |
| 路由参数 | 精确 + 尾部通配 | 扩展 `match` 支持 `:param` / 正则 |
| 装载超时 | 内核放弃等待但无法强杀协程 | 插件应自行控制 `Apply` 耗时 |

### 11.3 接入真实 gRPC 的映射

| 本示例 | 真实 gRPC |
|---|---|
| `GRPCServer.RegisterService(addr, full, h)` | `grpcServer.RegisterService(desc, impl)` + 每实例一个 `grpc.Server` |
| `GRPCServer.Use(fn)` | `grpc.UnaryInterceptor(fn)` / `grpc.ChainUnaryInterceptor` |
| `Client.Invoke(svc, method, payload)` | `conn.Invoke(ctx, "/pkg.Svc/Method", req, resp)` |
| `Registry` | etcd / Consul / Nacos 适配器 |
| `Balancer.Pick` | `grpc.WithBalancerName` / resolver + pick_first / round_robin |

替换 `service.go` 的实现即可,`plugin.go` 的插件层代码**不变** —— 关注点仍以 Disposer 的形式挂载。

---

## 附:一键运行

```bash
cd example/microservice_example && go run .
go build ./... && go vet ./...
```
