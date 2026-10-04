// plugin.go —— 内核插件 + 微服务关注点插件 + 业务服务插件 + 索引观察者。
//
// 分层与依赖方向(单向,Feature → Concern → Kernel):
//
//	grpc / discovery / http          内核:发布 svc/grpc、svc/registry、svc/http
//	client                           内核客户端:Registry 解析 + 轮询 + 客户端拦截器链
//	tracing → ratelimit → auth       gRPC 关注点(服务端拦截器,按依赖链保证叠加顺序)
//	circuitbreaker                   gRPC 关注点(客户端拦截器)
//	users / orders / greeter-a|b     业务微服务(注册方法与实例)
//	gateway                          HTTP → gRPC 网关(经 Client 调用)
//	watcher                          索引观察者(由宿主 Subscribe,收 TypeIndex 消息)
//
// 每个关注点都在 Apply 里登记一个 Disposer:卸载即从拦截器链 / 服务表 / 发现中心摘除。
//
// 约定:组件在装载期(Apply/Start/Run)只经 ctx 槽位拿依赖,不回调 Manager ——
// 装载期消费者持有 Manager 锁,回调会重入死锁;内核能力(查询/发现/订阅/信号)
// 一律由宿主在 Enable 之后调用。
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"GoTenon"
)

// basePlugin 提供 PluginInfo 默认实现,插件只覆写关心的钩子。
type basePlugin struct{}

func (basePlugin) Desc() map[string]string                  { return nil }
func (basePlugin) Inject() []string                         { return nil }
func (basePlugin) Status() map[string]any                  { return nil }
func (basePlugin) Register() error                          { return nil }
func (basePlugin) Apply(*GoTenon.GoTenonContext, any) error { return nil }
func (basePlugin) Start() error                             { return nil }
func (basePlugin) Run() error                               { return nil }
func (basePlugin) End() error                               { return nil }
func (basePlugin) DealWithMessage(GoTenon.Message) error    { return nil }

// cap 构造 MCP 风格的能力描述(推荐但非强制)。
func cap(desc string, props ...string) map[string]any {
	properties := map[string]any{}
	required := make([]string, 0, len(props))
	for _, p := range props {
		properties[p] = map[string]any{"type": "string"}
		required = append(required, p)
	}
	return map[string]any{
		"description": desc,
		"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required},
	}
}

func statusOf(kv map[string]any) map[string]any { return kv }

// logIndex 统一处理索引通知(TypeIndex);命中返回 true。
func logIndex(p string, m GoTenon.Message) bool {
	ev, ok := m.Data.(GoTenon.IndexEvent)
	if !ok {
		return false
	}
	fmt.Printf("[%s] 索引事件: %-6s %s\n", p, ev.Kind, ev.Plugin)
	return true
}

// service 从上下文沿祖先链解析共享槽,拿不到返回错误。
func service(ctx *GoTenon.GoTenonContext, name string) (any, error) {
	slot := ctx.SlotOf(name)
	if slot == nil || slot.Value == nil {
		return nil, fmt.Errorf("required service %q unavailable", name)
	}
	return slot.Value, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "%v\n", v)
}

// httpStatusOf 把 gRPC 错误码映射为 HTTP 状态,演示「协议桥接」。非 gRPC 错误按 502。
func httpStatusOf(err error) int {
	switch rpcCode(err) {
	case errUnauthenticated:
		return http.StatusUnauthorized
	case errExhausted:
		return http.StatusTooManyRequests
	case errUnavailable:
		return http.StatusServiceUnavailable
	case errInternal:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// logServerInterceptor 是 grpc 内核自带的默认服务端拦截器(可被后续插件叠加)。
func logServerInterceptor(ctx context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
	start := time.Now()
	resp, err := next(ctx, req)
	fmt.Printf("[grpc] %-24s inst=%-20s %8s err=%v\n",
		req.Service+"/"+req.Method, metaOf(ctx, metaInstance),
		time.Since(start).Round(time.Microsecond), err)
	return resp, err
}

// ---------- 内核:HTTP ----------

type httpKernel struct {
	basePlugin
	addr   string
	srv    *http.Server
	ln     net.Listener
	router *Router
}

func (p *httpKernel) Name() string { return "http" }
func (p *httpKernel) Desc() map[string]string {
	return map[string]string{"provides": "svc/http", "note": "HTTP 入口内核"}
}
func (p *httpKernel) Status() map[string]any {
	if p.srv == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	routes := 0
	if p.router != nil {
		routes = len(p.router.Dump())
	}
	return statusOf(map[string]any{"state": "ready", "addr": p.addr, "routes": routes})
}
func (p *httpKernel) Function() map[string]any {
	return map[string]any{"http.request": cap("向本地 HTTP 入口发起请求", "method", "path")}
}
func (p *httpKernel) ExecuteFunction(any) {}

func (p *httpKernel) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/router")
	if err != nil {
		return err
	}
	router := v.(*Router)
	p.router = router
	addr, _ := cfg.(string)
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("http: listen %s: %w", addr, err)
	}
	p.ln = ln
	p.addr = ln.Addr().String()
	p.srv = &http.Server{Handler: router.Handler()}
	ctx.Register(func() error { // 卸载即优雅停机,端口释放
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return p.srv.Shutdown(shutdownCtx)
	})
	go func() { _ = p.srv.Serve(ln) }()
	ctx.Isolate("svc/http")
	ctx.SlotOf("svc/http").Value = p
	fmt.Printf("[http] 网关入口监听 %s\n", p.addr)
	return nil
}

func (p *httpKernel) Addr() string { return p.addr }

func (p *httpKernel) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("http", m)
	}
	return nil
}

// ---------- 内核:gRPC ----------

type grpcKernel struct {
	basePlugin
	srv *GRPCServer
}

func (p *grpcKernel) Name() string { return "grpc" }
func (p *grpcKernel) Desc() map[string]string {
	return map[string]string{"provides": "svc/grpc", "note": "进程内 gRPC(多实例服务表)"}
}
func (p *grpcKernel) Status() map[string]any {
	if p.srv == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "handlers": len(p.srv.Methods())})
}
func (p *grpcKernel) Function() map[string]any {
	return map[string]any{"grpc.invoke": cap("在指定实例上发起一元调用", "addr", "fullMethod")}
}
func (p *grpcKernel) ExecuteFunction(any) {}

func (p *grpcKernel) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	srv := v.(*GRPCServer)
	p.srv = srv
	// 默认拦截器:日志。tracing / ratelimit / auth 等由插件随后叠加。
	ctx.Register(srv.Use(logServerInterceptor))
	ctx.Isolate("svc/grpc")
	ctx.SlotOf("svc/grpc").Value = srv
	fmt.Println("[grpc] 内核就绪:多实例服务表 + 一元拦截器链")
	return nil
}

func (p *grpcKernel) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("grpc", m)
	}
	return nil
}

// ---------- 内核:服务发现 ----------

type discoveryPlugin struct {
	basePlugin
	reg *Registry
}

func (p *discoveryPlugin) Name() string { return "discovery" }
func (p *discoveryPlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/registry", "note": "服务发现"}
}
func (p *discoveryPlugin) Status() map[string]any {
	if p.reg == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "services": len(p.reg.Services())})
}
func (p *discoveryPlugin) Function() map[string]any {
	return map[string]any{"discovery.resolve": cap("解析某服务的全部实例地址", "service")}
}
func (p *discoveryPlugin) ExecuteFunction(any) {}

func (p *discoveryPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/registry")
	if err != nil {
		return err
	}
	p.reg = v.(*Registry)
	ctx.Isolate("svc/registry")
	ctx.SlotOf("svc/registry").Value = p.reg
	fmt.Println("[discovery] 服务发现就绪")
	return nil
}

func (p *discoveryPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("discovery", m)
	}
	return nil
}

// ---------- 内核客户端:Registry + Balancer + Client ----------

// sharedLabel 是宿主在 root 预声明的共享槽 label。插件用 IsolateLabel 写入同一 label
// 的槽,即写入祖先(宿主)的槽;兄弟插件沿祖先链都能解析到 —— 这是跨插件共享能力的推荐姿势。
const sharedLabel = "shared"

type clientPlugin struct {
	basePlugin
	cli *Client
}

func (p *clientPlugin) Name() string     { return "client" }
func (p *clientPlugin) Inject() []string { return []string{"grpc", "discovery"} }
func (p *clientPlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/client", "note": "gRPC 客户端(选点+拦截器链)"}
}
func (p *clientPlugin) Status() map[string]any {
	if p.cli == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready"})
}
func (p *clientPlugin) Function() map[string]any {
	return map[string]any{"client.invoke": cap("按服务名调用远程方法", "service", "method", "payload")}
}
func (p *clientPlugin) ExecuteFunction(any) {}

func (p *clientPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	sv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	rv, err := service(ctx, "svc/registry")
	if err != nil {
		return err
	}
	lb := NewBalancer(rv.(*Registry))
	cli := NewClient(sv.(*GRPCServer), lb)
	p.cli = cli

	// 宿主已 root.IsolateLabel("svc/client", sharedLabel):此处写入的是 root 的共享槽。
	ctx.IsolateLabel("svc/balancer", sharedLabel)
	ctx.SlotOf("svc/balancer").Value = lb
	ctx.Register(func() error { ctx.SlotOf("svc/balancer").Value = nil; return nil })

	ctx.IsolateLabel("svc/client", sharedLabel)
	ctx.SlotOf("svc/client").Value = cli
	ctx.Register(func() error { ctx.SlotOf("svc/client").Value = nil; return nil })

	fmt.Println("[client] 就绪:Registry 解析 + 轮询负载均衡 + 客户端拦截器链(写入共享槽)")
	return nil
}

func (p *clientPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("client", m)
	}
	return nil
}

// ---------- 关注点:链路追踪(服务端 + 客户端拦截器) ----------

type tracingPlugin struct {
	basePlugin
	tracer *Tracer
}

func (p *tracingPlugin) Name() string     { return "tracing" }
func (p *tracingPlugin) Inject() []string { return []string{"grpc", "client"} }
func (p *tracingPlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/trace", "note": "链路追踪(双端拦截器)"}
}
func (p *tracingPlugin) Status() map[string]any {
	if p.tracer == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "spans": len(p.tracer.Snapshot())})
}
func (p *tracingPlugin) Function() map[string]any {
	return map[string]any{"trace.snapshot": cap("导出最近的调用链路")}
}
func (p *tracingPlugin) ExecuteFunction(any) {}

func (p *tracingPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	sv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	cv, err := service(ctx, "svc/client")
	if err != nil {
		return err
	}
	tracer := &Tracer{}
	p.tracer = tracer
	// 与 svc/client 同理:写入宿主预声明的共享槽,gateway 才能解析到同一个 Tracer
	ctx.IsolateLabel("svc/trace", sharedLabel)
	ctx.SlotOf("svc/trace").Value = tracer
	ctx.Register(func() error { ctx.SlotOf("svc/trace").Value = nil; return nil })

	// 服务端:读取(或生成)trace id,记录一次 span
	ctx.Register(sv.(*GRPCServer).Use(func(c context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
		traceID := metaOf(c, metaTraceID)
		if traceID == "" {
			traceID = newTraceID()
			c = withMeta(c, metaTraceID, traceID)
		}
		start := time.Now()
		resp, err := next(c, req)
		tracer.Add(Span{TraceID: traceID, Kind: "server", Service: req.Service, Method: req.Method,
			Instance: metaOf(c, metaInstance), Start: start, Duration: time.Since(start), Err: err})
		return resp, err
	}))

	// 客户端:若无 trace id 则生成,保证跨服务透传同一条链路
	ctx.Register(cv.(*Client).Use(func(c context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
		if metaOf(c, metaTraceID) == "" {
			c = withMeta(c, metaTraceID, newTraceID())
		}
		start := time.Now()
		resp, err := next(c, req)
		tracer.Add(Span{TraceID: metaOf(c, metaTraceID), Kind: "client", Service: req.Service, Method: req.Method,
			Start: start, Duration: time.Since(start), Err: err})
		return resp, err
	}))
	fmt.Println("[tracing] 服务端+客户端拦截器已挂载,链路 id 跨服务透传")
	return nil
}

func (p *tracingPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("tracing", m)
	}
	return nil
}

// ---------- 关注点:限流(服务端拦截器,可热调阈值) ----------

type ratelimitConfig struct{ Limit int }

type ratelimitPlugin struct {
	basePlugin
	mu     sync.Mutex
	counts map[string]int
	limit  int
}

func (p *ratelimitPlugin) Name() string     { return "ratelimit" }
func (p *ratelimitPlugin) Inject() []string { return []string{"tracing"} }
func (p *ratelimitPlugin) Desc() map[string]string {
	return map[string]string{"note": "服务端方法级限流"}
}
func (p *ratelimitPlugin) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.counts == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	total := 0
	for _, n := range p.counts {
		total += n
	}
	return statusOf(map[string]any{"state": "ready", "limit": p.limit, "seen": total})
}
func (p *ratelimitPlugin) Function() map[string]any {
	return map[string]any{"ratelimit.stats": cap("查看限流计数")}
}
func (p *ratelimitPlugin) ExecuteFunction(any) {}

func (p *ratelimitPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(ratelimitConfig)
	if c.Limit <= 0 {
		c.Limit = 1000
	}
	p.mu.Lock()
	p.limit = c.Limit
	p.counts = make(map[string]int)
	p.mu.Unlock()

	sv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	ctx.Register(sv.(*GRPCServer).Use(func(cc context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
		key := req.Service + "/" + req.Method
		p.mu.Lock()
		p.counts[key]++
		n := p.counts[key]
		limit := p.limit
		p.mu.Unlock()
		if n > limit {
			return nil, rpcErr(errExhausted, "rate limit %d exceeded for %s", limit, key)
		}
		return next(cc, req)
	}))
	fmt.Printf("[ratelimit] 服务端限流已挂载,阈值 %d\n", p.limit)
	return nil
}

func (p *ratelimitPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("ratelimit", m)
	}
	return nil
}

// ---------- 关注点:鉴权(服务端拦截器,校验调用元数据里的 token) ----------

type authConfig struct{ Token string }

type authPlugin struct {
	basePlugin
	token string
}

func (p *authPlugin) Name() string     { return "auth" }
func (p *authPlugin) Inject() []string { return []string{"ratelimit"} }
func (p *authPlugin) Desc() map[string]string {
	return map[string]string{"note": "服务端 token 鉴权"}
}
func (p *authPlugin) Status() map[string]any {
	if p.token == "" {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "token": "***"})
}
func (p *authPlugin) Function() map[string]any {
	return map[string]any{"auth.check": cap("校验调用方 token")}
}
func (p *authPlugin) ExecuteFunction(any) {}

func (p *authPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(authConfig)
	if c.Token == "" {
		c.Token = "secret"
	}
	p.token = c.Token

	sv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	ctx.Register(sv.(*GRPCServer).Use(func(cc context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
		if metaOf(cc, metaToken) != p.token {
			return nil, rpcErr(errUnauthenticated, "invalid token for %s/%s", req.Service, req.Method)
		}
		return next(cc, req)
	}))
	fmt.Printf("[auth] 服务端鉴权已挂载,要求 token=%q\n", p.token)
	return nil
}

func (p *authPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("auth", m)
	}
	return nil
}

// ---------- 关注点:熔断(客户端拦截器,按服务统计连续失败) ----------

type circuitbreakerConfig struct {
	Threshold int
	Cooldown  time.Duration
}

type breaker struct {
	fails    int
	open     bool
	openedAt time.Time
}

type circuitbreakerPlugin struct {
	basePlugin
	mu        sync.Mutex
	breakers  map[string]*breaker
	threshold int
	cooldown  time.Duration
}

func (p *circuitbreakerPlugin) Name() string     { return "circuitbreaker" }
func (p *circuitbreakerPlugin) Inject() []string { return []string{"tracing"} }
func (p *circuitbreakerPlugin) Desc() map[string]string {
	return map[string]string{"note": "客户端熔断(连续失败即开路)"}
}
func (p *circuitbreakerPlugin) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.breakers == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	open := 0
	for _, b := range p.breakers {
		if b.open {
			open++
		}
	}
	return statusOf(map[string]any{"state": "ready", "threshold": p.threshold, "open": open})
}
func (p *circuitbreakerPlugin) Function() map[string]any {
	return map[string]any{"circuitbreaker.state": cap("查看各服务熔断状态")}
}
func (p *circuitbreakerPlugin) ExecuteFunction(any) {}

func (p *circuitbreakerPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(circuitbreakerConfig)
	if c.Threshold <= 0 {
		c.Threshold = 3
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 200 * time.Millisecond
	}
	p.mu.Lock()
	p.threshold = c.Threshold
	p.cooldown = c.Cooldown
	p.breakers = make(map[string]*breaker)
	p.mu.Unlock()

	cv, err := service(ctx, "svc/client")
	if err != nil {
		return err
	}
	ctx.Register(cv.(*Client).Use(func(cc context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
		if err := p.before(req.Service); err != nil {
			return nil, err
		}
		resp, err := next(cc, req)
		p.after(req.Service, err)
		return resp, err
	}))
	fmt.Printf("[circuitbreaker] 客户端熔断已挂载,阈值 %d / 冷却 %s\n", p.threshold, p.cooldown)
	return nil
}

func (p *circuitbreakerPlugin) before(service string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.breakers[service]
	if b == nil || !b.open {
		return nil
	}
	if time.Since(b.openedAt) < p.cooldown {
		return rpcErr(errUnavailable, "circuit open for %s", service)
	}
	return nil // 冷却已过:放行一次探测(half-open)
}

func (p *circuitbreakerPlugin) after(service string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b := p.breakers[service]
	if b == nil {
		b = &breaker{}
		p.breakers[service] = b
	}
	if err != nil {
		b.fails++
		if b.fails >= p.threshold {
			b.open = true
			b.openedAt = time.Now()
		}
		return
	}
	b.fails = 0
	b.open = false
}

func (p *circuitbreakerPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("circuitbreaker", m)
	}
	return nil
}

// ---------- 业务服务:users(提供 demo.Users/Get) ----------

type usersConfig struct{ Version string }

type usersPlugin struct {
	basePlugin
	addr    string
	version string
	ready   bool
}

func (p *usersPlugin) Name() string     { return "users" }
func (p *usersPlugin) Inject() []string { return []string{"grpc", "discovery"} }
func (p *usersPlugin) Desc() map[string]string {
	return map[string]string{"provides": "demo.Users/Get", "addr": "inproc://users-1"}
}
func (p *usersPlugin) Status() map[string]any {
	if !p.ready {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "addr": p.addr, "version": p.version})
}
func (p *usersPlugin) Function() map[string]any {
	return map[string]any{"demo.Users/Get": cap("按 id 查询用户", "id")}
}
func (p *usersPlugin) ExecuteFunction(any) {}

func (p *usersPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(usersConfig)
	if c.Version == "" {
		c.Version = "v1"
	}
	if p.addr == "" {
		p.addr = "inproc://users-1"
	}
	p.version = c.Version

	sv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	rv, err := service(ctx, "svc/registry")
	if err != nil {
		return err
	}
	// Effect:方法注册 + 发现登记是一体,任一步失败整体回滚
	_, err = ctx.Effect(func() error {
		ctx.Register(sv.(*GRPCServer).RegisterService(p.addr, "demo.Users/Get",
			func(c context.Context, req *GRPCRequest) (*GRPCResponse, error) {
				id := string(req.Payload)
				if id == "" {
					id = "alice"
				}
				if id == "boom" { // 故意失败,演示熔断
					return nil, rpcErr(errInternal, "user %q lookup failed", id)
				}
				return &GRPCResponse{Payload: []byte("user:" + id + "@" + p.version)}, nil
			}))
		ctx.Register(rv.(*Registry).Register("demo.Users", p.addr, p.version))
		ctx.Register(func() error { p.ready = false; return nil })
		return nil
	}, "users-endpoint")
	if err != nil {
		return err
	}
	p.ready = true
	fmt.Printf("[users] demo.Users 实例 %s 上线(%s)\n", p.addr, p.version)
	return nil
}

func (p *usersPlugin) DealWithMessage(m GoTenon.Message) error {
	switch m.Type {
	case GoTenon.TypeIndex:
		logIndex("users", m)
	case GoTenon.TypeRaw:
		fmt.Printf("[users] 收到直连调用载荷: %v\n", m.Data)
	}
	return nil
}

// ---------- 业务服务:orders(经 Client 调用 users,演示服务间调用) ----------

type ordersPlugin struct {
	basePlugin
	addr  string
	ready bool
}

func (p *ordersPlugin) Name() string     { return "orders" }
func (p *ordersPlugin) Inject() []string { return []string{"client", "users"} }
func (p *ordersPlugin) Desc() map[string]string {
	return map[string]string{"provides": "demo.Orders/Create", "note": "调用 demo.Users/Get"}
}
func (p *ordersPlugin) Status() map[string]any {
	if !p.ready {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "addr": p.addr})
}
func (p *ordersPlugin) Function() map[string]any {
	return map[string]any{"demo.Orders/Create": cap("为用户创建订单(内部调用 users)", "user")}
}
func (p *ordersPlugin) ExecuteFunction(any) {}

func (p *ordersPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	if p.addr == "" {
		p.addr = "inproc://orders-1"
	}
	sv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	rv, err := service(ctx, "svc/registry")
	if err != nil {
		return err
	}
	cv, err := service(ctx, "svc/client")
	if err != nil {
		return err
	}
	cli := cv.(*Client)

	_, err = ctx.Effect(func() error {
		ctx.Register(sv.(*GRPCServer).RegisterService(p.addr, "demo.Orders/Create",
			func(c context.Context, req *GRPCRequest) (*GRPCResponse, error) {
				user := string(req.Payload)
				if user == "" {
					user = "alice"
				}
				// 服务间调用:同一个 ctx,因此 token / trace id 自动透传
				resp, err := cli.Invoke(c, "demo.Users", "Get", []byte(user))
				if err != nil {
					return nil, err
				}
				if resp.Err != nil {
					return nil, resp.Err
				}
				return &GRPCResponse{Payload: []byte("order(" + user + ") -> " + string(resp.Payload))}, nil
			}))
		ctx.Register(rv.(*Registry).Register("demo.Orders", p.addr, "v1"))
		ctx.Register(func() error { p.ready = false; return nil })
		return nil
	}, "orders-endpoint")
	if err != nil {
		return err
	}
	p.ready = true
	fmt.Printf("[orders] demo.Orders 实例 %s 上线,内部调用 demo.Users/Get\n", p.addr)
	return nil
}

func (p *ordersPlugin) DealWithMessage(m GoTenon.Message) error {
	switch m.Type {
	case GoTenon.TypeIndex:
		logIndex("orders", m)
	case GoTenon.TypeRaw:
		fmt.Printf("[orders] 收到直连调用载荷: %v\n", m.Data)
	}
	return nil
}

// ---------- 业务服务:greeter(两个实例,演示负载均衡与灰度) ----------

type greeterPlugin struct {
	basePlugin
	id    string
	addr  string
	ready bool
}

func (p *greeterPlugin) Name() string     { return "greeter-" + p.id }
func (p *greeterPlugin) Inject() []string { return []string{"grpc", "discovery"} }
func (p *greeterPlugin) Desc() map[string]string {
	return map[string]string{"provides": "demo.Greeter/SayHello", "addr": p.addr}
}
func (p *greeterPlugin) Status() map[string]any {
	if !p.ready {
		return statusOf(map[string]any{"state": "pending", "id": p.id})
	}
	return statusOf(map[string]any{"state": "ready", "id": p.id, "addr": p.addr})
}
func (p *greeterPlugin) Function() map[string]any {
	return map[string]any{"demo.Greeter/SayHello": cap("向指定名字问好", "name")}
}
func (p *greeterPlugin) ExecuteFunction(any) {}

func (p *greeterPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	sv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	rv, err := service(ctx, "svc/registry")
	if err != nil {
		return err
	}
	_, err = ctx.Effect(func() error {
		ctx.Register(sv.(*GRPCServer).RegisterService(p.addr, "demo.Greeter/SayHello",
			func(c context.Context, req *GRPCRequest) (*GRPCResponse, error) {
				name := string(req.Payload)
				if name == "" {
					name = "world"
				}
				return &GRPCResponse{Payload: []byte(fmt.Sprintf("hello, %s (from %s)", name, p.id))}, nil
			}))
		ctx.Register(rv.(*Registry).Register("demo.Greeter", p.addr, "v1"))
		ctx.Register(func() error { p.ready = false; return nil })
		return nil
	}, "greeter-endpoint")
	if err != nil {
		return err
	}
	p.ready = true
	fmt.Printf("[greeter-%s] demo.Greeter 实例 %s 上线\n", p.id, p.addr)
	return nil
}

func (p *greeterPlugin) DealWithMessage(m GoTenon.Message) error {
	switch m.Type {
	case GoTenon.TypeIndex:
		logIndex(p.Name(), m)
	case GoTenon.TypeRaw:
		fmt.Printf("[greeter-%s] 收到直连调用载荷: %v\n", p.id, m.Data)
	}
	return nil
}

// ---------- 网关:HTTP → gRPC(经 Client 选点调用) ----------

type gatewayPlugin struct {
	basePlugin
	cli    *Client
	tracer *Tracer
	routes int
}

func (p *gatewayPlugin) Name() string     { return "gateway" }
func (p *gatewayPlugin) Inject() []string { return []string{"http", "client"} }
func (p *gatewayPlugin) Desc() map[string]string {
	return map[string]string{"provides": "/api/*,/debug/traces", "note": "HTTP→gRPC 桥接"}
}
func (p *gatewayPlugin) Status() map[string]any {
	if p.cli == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "routes": p.routes, "tracing": p.tracer != nil})
}
func (p *gatewayPlugin) Function() map[string]any {
	return map[string]any{
		"gateway.greet":  cap("GET /api/greet?name="),
		"gateway.users":  cap("GET /api/users?id="),
		"gateway.order":  cap("GET /api/order?user="),
		"gateway.traces": cap("GET /debug/traces"),
	}
}
func (p *gatewayPlugin) ExecuteFunction(any) {}

func (p *gatewayPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	rv, err := service(ctx, "svc/router")
	if err != nil {
		return err
	}
	cv, err := service(ctx, "svc/client")
	if err != nil {
		return err
	}
	p.cli = cv.(*Client)
	if tv, err := service(ctx, "svc/trace"); err == nil {
		p.tracer = tv.(*Tracer)
	}
	router := rv.(*Router)

	_, err = ctx.Effect(func() error {
		ctx.Register(router.Handle("GET", "/api/greet", http.HandlerFunc(p.handleGreet)))
		ctx.Register(router.Handle("GET", "/api/users", http.HandlerFunc(p.handleUsers)))
		ctx.Register(router.Handle("GET", "/api/order", http.HandlerFunc(p.handleOrder)))
		if p.tracer != nil {
			ctx.Register(router.Handle("GET", "/debug/traces", http.HandlerFunc(p.handleTraces)))
		}
		return nil
	}, "gateway-routes")
	if err != nil {
		return err
	}
	p.routes = 4
	if p.tracer == nil {
		p.routes = 3
	}
	fmt.Println("[gateway] /api/greet /api/users /api/order /debug/traces 上线")
	return nil
}

// call 把 HTTP 请求翻译成一次 gRPC 调用:token 从查询串取出并写入调用元数据。
func (p *gatewayPlugin) call(w http.ResponseWriter, r *http.Request, svcService, method, payload string) {
	ctx := withMeta(r.Context(), metaToken, r.URL.Query().Get("token"))
	resp, err := p.cli.Invoke(ctx, svcService, method, []byte(payload))
	if err != nil {
		http.Error(w, err.Error(), httpStatusOf(err))
		return
	}
	if resp.Err != nil {
		http.Error(w, resp.Err.Error(), httpStatusOf(resp.Err))
		return
	}
	writeJSON(w, map[string]any{"reply": string(resp.Payload)})
}

func (p *gatewayPlugin) handleGreet(w http.ResponseWriter, r *http.Request) {
	p.call(w, r, "demo.Greeter", "SayHello", r.URL.Query().Get("name"))
}

func (p *gatewayPlugin) handleUsers(w http.ResponseWriter, r *http.Request) {
	p.call(w, r, "demo.Users", "Get", r.URL.Query().Get("id"))
}

func (p *gatewayPlugin) handleOrder(w http.ResponseWriter, r *http.Request) {
	p.call(w, r, "demo.Orders", "Create", r.URL.Query().Get("user"))
}

func (p *gatewayPlugin) handleTraces(w http.ResponseWriter, r *http.Request) {
	spans := p.tracer.Snapshot()
	if len(spans) > 16 {
		spans = spans[len(spans)-16:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, s := range spans {
		errText := ""
		if s.Err != nil {
			errText = " err=" + s.Err.Error()
		}
		fmt.Fprintf(w, "%-6s trace=%-8s %-24s inst=%-20s %8s%s\n",
			s.Kind, s.TraceID, s.Service+"/"+s.Method, s.Instance,
			s.Duration.Round(time.Microsecond), errText)
	}
}

func (p *gatewayPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("gateway", m)
	}
	return nil
}

// ---------- 观察者:watcher(收内核以 TypeIndex 投递的索引变更) ----------

type watcherPlugin struct {
	basePlugin
	mu   sync.Mutex
	seen []string
}

func (p *watcherPlugin) Name() string            { return "watcher" }
func (p *watcherPlugin) Desc() map[string]string { return map[string]string{"subscribes": "index"} }
func (p *watcherPlugin) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return statusOf(map[string]any{"state": "ready", "seen": len(p.seen)})
}
func (p *watcherPlugin) Function() map[string]any {
	return map[string]any{"index.watch": cap("查看已收到的索引事件")}
}
func (p *watcherPlugin) ExecuteFunction(any) {}

// DealWithMessage 只消费内核投递的索引通知(TypeIndex);其它类型原样忽略。
func (p *watcherPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type != GoTenon.TypeIndex {
		return nil
	}
	ev, ok := m.Data.(GoTenon.IndexEvent)
	if !ok {
		return nil
	}
	p.mu.Lock()
	p.seen = append(p.seen, ev.Kind+" "+ev.Plugin)
	p.mu.Unlock()
	fmt.Printf("[watcher] 收到索引通知: %-6s %s\n", ev.Kind, ev.Plugin)
	return nil
}
