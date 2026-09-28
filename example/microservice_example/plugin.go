// plugin.go —— 内核插件 + 微服务关注点插件 + 业务服务插件。
//
// 分层与依赖方向(单向,Feature → Concern → Kernel):
//
//	grpc / discovery / http        内核:发布 svc/grpc、svc/registry、svc/http
//	client                         内核客户端:Registry 解析 + 轮询 + 客户端拦截器链
//	tracing → ratelimit → auth     gRPC 关注点(服务端拦截器,按依赖链保证叠加顺序)
//	circuitbreaker                 gRPC 关注点(客户端拦截器)
//	users / orders / greeter-a|b   业务微服务(注册方法与实例)
//	gateway                        HTTP → gRPC 网关(经 Client 调用)
//
// 每个关注点都在 Apply 里登记一个 Disposer:卸载即从拦截器链 / 服务表 / 发现中心摘除。
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
func (basePlugin) Config() map[string]string                { return nil }
func (basePlugin) Register() error                          { return nil }
func (basePlugin) Apply(*GoTenon.GoTenonContext, any) error { return nil }
func (basePlugin) Start() error                             { return nil }
func (basePlugin) Run() error                               { return nil }
func (basePlugin) End() error                               { return nil }
func (basePlugin) DealWithMessage(context.Context) error    { return nil }

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

// statusOf 把 gRPC 错误码映射为 HTTP 状态,演示「协议桥接」。非 gRPC 错误按 502。
func statusOf(err error) int {
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
	addr string
	srv  *http.Server
	ln   net.Listener
}

func (p *httpKernel) Name() string { return "http" }
func (p *httpKernel) Desc() map[string]string {
	return map[string]string{"provides": "svc/http", "note": "HTTP 入口内核"}
}

func (p *httpKernel) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/router")
	if err != nil {
		return err
	}
	router := v.(*Router)
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

// ---------- 内核:gRPC ----------

type grpcKernel struct {
	basePlugin
}

func (p *grpcKernel) Name() string { return "grpc" }
func (p *grpcKernel) Desc() map[string]string {
	return map[string]string{"provides": "svc/grpc", "note": "进程内 gRPC(多实例服务表)"}
}

func (p *grpcKernel) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	srv := v.(*GRPCServer)
	// 默认拦截器:日志。tracing / ratelimit / auth 等由插件随后叠加。
	ctx.Register(srv.Use(logServerInterceptor))
	ctx.Isolate("svc/grpc")
	ctx.SlotOf("svc/grpc").Value = srv
	fmt.Println("[grpc] 内核就绪:多实例服务表 + 一元拦截器链")
	return nil
}

// ---------- 内核:服务发现 ----------

type discoveryPlugin struct {
	basePlugin
}

func (p *discoveryPlugin) Name() string { return "discovery" }
func (p *discoveryPlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/registry", "note": "服务发现"}
}

func (p *discoveryPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/registry")
	if err != nil {
		return err
	}
	ctx.Isolate("svc/registry")
	ctx.SlotOf("svc/registry").Value = v.(*Registry)
	fmt.Println("[discovery] 服务发现就绪")
	return nil
}

// ---------- 内核客户端:Registry + Balancer + Client ----------

// sharedLabel 是宿主在 root 预声明的共享槽 label。插件用 IsolateLabel 写入同一 label
// 的槽,即写入祖先(宿主)的槽;兄弟插件沿祖先链都能解析到 —— 这是跨插件共享能力的推荐姿势。
const sharedLabel = "shared"

type clientPlugin struct {
	basePlugin
}

func (p *clientPlugin) Name() string     { return "client" }
func (p *clientPlugin) Inject() []string { return []string{"grpc", "discovery"} }
func (p *clientPlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/client", "note": "gRPC 客户端(选点+拦截器链)"}
}

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

// ---------- 关注点:链路追踪(服务端 + 客户端拦截器) ----------

type tracingPlugin struct {
	basePlugin
}

func (p *tracingPlugin) Name() string     { return "tracing" }
func (p *tracingPlugin) Inject() []string { return []string{"grpc", "client"} }
func (p *tracingPlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/trace", "note": "链路追踪(双端拦截器)"}
}

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

func (p *ratelimitPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(ratelimitConfig)
	if c.Limit <= 0 {
		c.Limit = 1000
	}
	p.limit = c.Limit
	p.counts = make(map[string]int)

	sv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	ctx.Register(sv.(*GRPCServer).Use(func(cc context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
		key := req.Service + "/" + req.Method
		p.mu.Lock()
		p.counts[key]++
		n := p.counts[key]
		p.mu.Unlock()
		if n > p.limit {
			return nil, rpcErr(errExhausted, "rate limit %d exceeded for %s", p.limit, key)
		}
		return next(cc, req)
	}))
	fmt.Printf("[ratelimit] 服务端限流已挂载,阈值 %d\n", p.limit)
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

func (p *circuitbreakerPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(circuitbreakerConfig)
	if c.Threshold <= 0 {
		c.Threshold = 3
	}
	if c.Cooldown <= 0 {
		c.Cooldown = 200 * time.Millisecond
	}
	p.threshold = c.Threshold
	p.cooldown = c.Cooldown
	p.breakers = make(map[string]*breaker)

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

// ---------- 业务服务:users(提供 demo.Users/Get) ----------

type usersConfig struct{ Version string }

type usersPlugin struct {
	basePlugin
	addr    string
	version string
}

func (p *usersPlugin) Name() string     { return "users" }
func (p *usersPlugin) Inject() []string { return []string{"grpc", "discovery"} }
func (p *usersPlugin) Desc() map[string]string {
	return map[string]string{"provides": "demo.Users/Get", "addr": "inproc://users-1"}
}

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
		return nil
	}, "users-endpoint")
	if err != nil {
		return err
	}
	fmt.Printf("[users] demo.Users 实例 %s 上线(%s)\n", p.addr, p.version)
	return nil
}

// ---------- 业务服务:orders(经 Client 调用 users,演示服务间调用) ----------

type ordersPlugin struct {
	basePlugin
	addr string
}

func (p *ordersPlugin) Name() string     { return "orders" }
func (p *ordersPlugin) Inject() []string { return []string{"client", "users"} }
func (p *ordersPlugin) Desc() map[string]string {
	return map[string]string{"provides": "demo.Orders/Create", "note": "调用 demo.Users/Get"}
}

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
		return nil
	}, "orders-endpoint")
	if err != nil {
		return err
	}
	fmt.Printf("[orders] demo.Orders 实例 %s 上线,内部调用 demo.Users/Get\n", p.addr)
	return nil
}

// ---------- 业务服务:greeter(两个实例,演示负载均衡与灰度) ----------

type greeterPlugin struct {
	basePlugin
	id   string
	addr string
}

func (p *greeterPlugin) Name() string     { return "greeter-" + p.id }
func (p *greeterPlugin) Inject() []string { return []string{"grpc", "discovery"} }
func (p *greeterPlugin) Desc() map[string]string {
	return map[string]string{"provides": "demo.Greeter/SayHello", "addr": p.addr}
}

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
		return nil
	}, "greeter-endpoint")
	if err != nil {
		return err
	}
	fmt.Printf("[greeter-%s] demo.Greeter 实例 %s 上线\n", p.id, p.addr)
	return nil
}

// ---------- 网关:HTTP → gRPC(经 Client 选点调用) ----------

type gatewayPlugin struct {
	basePlugin
	cli    *Client
	tracer *Tracer
}

func (p *gatewayPlugin) Name() string     { return "gateway" }
func (p *gatewayPlugin) Inject() []string { return []string{"http", "client"} }
func (p *gatewayPlugin) Desc() map[string]string {
	return map[string]string{"provides": "/api/*,/debug/traces", "note": "HTTP→gRPC 桥接"}
}

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
	fmt.Println("[gateway] /api/greet /api/users /api/order /debug/traces 上线")
	return nil
}

// call 把 HTTP 请求翻译成一次 gRPC 调用:token 从查询串取出并写入调用元数据。
func (p *gatewayPlugin) call(w http.ResponseWriter, r *http.Request, svcService, method, payload string) {
	ctx := withMeta(r.Context(), metaToken, r.URL.Query().Get("token"))
	resp, err := p.cli.Invoke(ctx, svcService, method, []byte(payload))
	if err != nil {
		http.Error(w, err.Error(), statusOf(err))
		return
	}
	if resp.Err != nil {
		http.Error(w, resp.Err.Error(), statusOf(resp.Err))
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
