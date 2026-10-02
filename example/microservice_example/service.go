// service.go —— 微服务内核服务契约(插件库对外能力,不属于 GoTenon 内核)。
//
// 把「微服务」拆成五个可组合的一等能力,全部遵循「注册即副作用」:
//
//	Router     可撤销 HTTP 路由表(网关入口)
//	Registry   服务发现:服务名 → 多实例地址(带版本)
//	Balancer   客户端负载均衡:轮询挑选实例
//	GRPCServer 进程内 gRPC:多实例服务表(addr + fullMethod)+ 一元拦截器链
//	Client     gRPC 客户端:Registry 解析 + Balancer 选点 + 客户端拦截器链
//	Tracer     链路追踪:收集 span,供 /debug/traces 展示
//
// 宿主在 root 预挂 Registry / GRPCServer / Router;Client 与 Balancer 由内核插件构建。
// 真实项目只替换本文件(GRPCServer/Client 换成 google.golang.org/grpc),插件层代码不变。
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"GoTenon"
)

// ---------- 调用元数据:沿 context 传播(trace id / token / 实例地址) ----------

type metaKey string

const (
	metaTraceID  metaKey = "gotenon.trace_id"
	metaToken    metaKey = "gotenon.token"
	metaInstance metaKey = "gotenon.instance"
)

func withMeta(ctx context.Context, k metaKey, v string) context.Context {
	return context.WithValue(ctx, k, v)
}

func metaOf(ctx context.Context, k metaKey) string {
	v, _ := ctx.Value(k).(string)
	return v
}

var traceSeq uint64

func newTraceID() string {
	return fmt.Sprintf("t-%06d", atomic.AddUint64(&traceSeq, 1))
}

// ---------- gRPC 错误码:供网关映射 HTTP 状态 ----------

const (
	errUnauthenticated = "UNAUTHENTICATED"
	errExhausted       = "RESOURCE_EXHAUSTED"
	errUnavailable     = "UNAVAILABLE"
	errInternal        = "INTERNAL"
)

type rpcError struct{ Code, Msg string }

func (e *rpcError) Error() string { return e.Code + ": " + e.Msg }

func rpcErr(code, format string, args ...any) error {
	return &rpcError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

func rpcCode(err error) string {
	var e *rpcError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ============================ 可撤销 HTTP 路由表 ============================

// Router 是注册即副作用的 HTTP 路由表:Handle / Use 都返回 GoTenon.Disposer。
type Router struct {
	mu     sync.RWMutex
	routes []*route
	mws    []mwEntry
	nextID int
}

type route struct {
	id      int
	method  string
	pattern string
	handler http.Handler
}

type mwEntry struct {
	id int
	mw func(http.Handler) http.Handler
}

func NewRouter() *Router { return &Router{} }

// Handle 注册一条路由,返回幂等注销函数。
func (r *Router) Handle(method, pattern string, h http.Handler) GoTenon.Disposer {
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	r.routes = append(r.routes, &route{id: id, method: strings.ToUpper(method), pattern: pattern, handler: h})
	r.mu.Unlock()
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, rt := range r.routes {
			if rt.id == id {
				r.routes = append(r.routes[:i], r.routes[i+1:]...)
				return nil
			}
		}
		return nil
	}
}

// Use 追加一段中间件,返回注销函数;注册顺序即外层到内层。
func (r *Router) Use(mw func(http.Handler) http.Handler) GoTenon.Disposer {
	r.mu.Lock()
	id := r.nextID
	r.nextID++
	r.mws = append(r.mws, mwEntry{id: id, mw: mw})
	r.mu.Unlock()
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		for i, m := range r.mws {
			if m.id == id {
				r.mws = append(r.mws[:i], r.mws[i+1:]...)
				return nil
			}
		}
		return nil
	}
}

// Handler 每请求读取路由表快照,使热插拔对已启动的 http.Server 立即生效。
func (r *Router) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.RLock()
		routes := append([]*route(nil), r.routes...)
		mws := append([]mwEntry(nil), r.mws...)
		r.mu.RUnlock()

		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			for _, rt := range routes {
				if rt.method == req.Method && match(rt.pattern, req.URL.Path) {
					rt.handler.ServeHTTP(w, req)
					return
				}
			}
			http.NotFound(w, req)
		})
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i].mw(h)
		}
		h.ServeHTTP(w, req)
	})
}

// Dump 返回 "METHOD PATH" 排序快照(诊断用)。
func (r *Router) Dump() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.routes))
	for _, rt := range r.routes {
		out = append(out, rt.method+" "+rt.pattern)
	}
	sort.Strings(out)
	return out
}

func match(pattern, path string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == path
}

// ============================ 服务发现 ============================

// Instance 是一个服务实例(地址 + 版本);地址是进程内 gRPC 的寻址键。
type Instance struct {
	Addr    string
	Version string
}

// Registry 是服务名 → 多实例的注册表:注册即 Disposer,卸载即摘除。
type Registry struct {
	mu        sync.RWMutex
	instances map[string][]Instance
}

func NewRegistry() *Registry { return &Registry{instances: make(map[string][]Instance)} }

// Register 登记一个服务实例,返回幂等注销函数。
func (r *Registry) Register(service, addr, version string) GoTenon.Disposer {
	r.mu.Lock()
	r.instances[service] = append(r.instances[service], Instance{Addr: addr, Version: version})
	r.mu.Unlock()
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		list := r.instances[service]
		for i, inst := range list {
			if inst.Addr == addr {
				r.instances[service] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(r.instances[service]) == 0 {
			delete(r.instances, service)
		}
		return nil
	}
}

// Resolve 返回某服务的全部实例地址快照(负载均衡消费)。
func (r *Registry) Resolve(service string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list := r.instances[service]
	out := make([]string, 0, len(list))
	for _, inst := range list {
		out = append(out, inst.Addr)
	}
	return out
}

// Describe 返回某服务的实例明细(诊断用)。
func (r *Registry) Describe(service string) []Instance {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]Instance(nil), r.instances[service]...)
}

// Services 返回全部服务名与实例数(诊断用),按服务名排序。
func (r *Registry) Services() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.instances))
	for name, list := range r.instances {
		names = append(names, fmt.Sprintf("%s(%d)", name, len(list)))
	}
	sort.Strings(names)
	return names
}

// ============================ 负载均衡 ============================

// Balancer 按服务名轮询挑选实例。
type Balancer struct {
	reg *Registry
	mu  sync.Mutex
	rr  map[string]int
}

func NewBalancer(reg *Registry) *Balancer {
	return &Balancer{reg: reg, rr: make(map[string]int)}
}

// Pick 返回该服务的下一个实例地址;无实例时返回错误。
func (b *Balancer) Pick(service string) (string, error) {
	addrs := b.reg.Resolve(service)
	if len(addrs) == 0 {
		return "", rpcErr(errUnavailable, "no live instance for service %q", service)
	}
	b.mu.Lock()
	i := b.rr[service]
	b.rr[service] = (i + 1) % len(addrs)
	b.mu.Unlock()
	return addrs[i%len(addrs)], nil
}

// ============================ 进程内 gRPC ============================

// GRPCRequest 是一元调用请求。
type GRPCRequest struct {
	Service string
	Method  string
	Payload []byte
}

// GRPCResponse 是一元调用响应。
type GRPCResponse struct {
	Payload []byte
	Err     error
}

// UnaryHandler 是一元方法处理函数。
type UnaryHandler func(ctx context.Context, req *GRPCRequest) (*GRPCResponse, error)

// UnaryInterceptor 是一元拦截器:服务端与客户端共用同一签名。
type UnaryInterceptor func(ctx context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error)

// GRPCServer 是进程内 gRPC:handler 以 (实例地址, fullMethod) 为键,
// 因此同一方法可在多个实例上并存 —— 这是多实例负载均衡的基础。
type GRPCServer struct {
	mu           sync.RWMutex
	handlers     map[string]UnaryHandler // key: addr + "\x00" + fullMethod
	interceptors []grpcMW
	nextID       int
}

type grpcMW struct {
	id int
	fn UnaryInterceptor
}

func NewGRPCServer() *GRPCServer {
	return &GRPCServer{handlers: make(map[string]UnaryHandler)}
}

func handlerKey(addr, fullMethod string) string { return addr + "\x00" + fullMethod }

// RegisterService 在指定实例地址上注册一元方法,返回幂等注销函数。
func (s *GRPCServer) RegisterService(addr, fullMethod string, h UnaryHandler) GoTenon.Disposer {
	s.mu.Lock()
	s.handlers[handlerKey(addr, fullMethod)] = h
	s.mu.Unlock()
	return func() error {
		s.mu.Lock()
		delete(s.handlers, handlerKey(addr, fullMethod))
		s.mu.Unlock()
		return nil
	}
}

// Use 注册一元服务端拦截器,返回注销函数;注册顺序即外层到内层。
func (s *GRPCServer) Use(i UnaryInterceptor) GoTenon.Disposer {
	s.mu.Lock()
	id := s.nextID
	s.nextID++
	s.interceptors = append(s.interceptors, grpcMW{id: id, fn: i})
	s.mu.Unlock()
	return func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		for idx, m := range s.interceptors {
			if m.id == id {
				s.interceptors = append(s.interceptors[:idx], s.interceptors[idx+1:]...)
				return nil
			}
		}
		return nil
	}
}

// Invoke 在指定实例上执行一次调用:组装服务端拦截器链并派发到 handler。
func (s *GRPCServer) Invoke(ctx context.Context, addr, fullMethod string, payload []byte) (*GRPCResponse, error) {
	s.mu.RLock()
	h := s.handlers[handlerKey(addr, fullMethod)]
	mws := append([]grpcMW(nil), s.interceptors...)
	s.mu.RUnlock()
	if h == nil {
		return nil, rpcErr(errUnavailable, "method %q not served on %q", fullMethod, addr)
	}

	ctx = withMeta(ctx, metaInstance, addr) // 让拦截器知道本次命中哪个实例
	chain := h
	for i := len(mws) - 1; i >= 0; i-- {
		next, mw := chain, mws[i].fn
		chain = func(ctx context.Context, req *GRPCRequest) (*GRPCResponse, error) {
			return mw(ctx, req, next)
		}
	}
	service, method, _ := strings.Cut(fullMethod, "/")
	return chain(ctx, &GRPCRequest{Service: service, Method: method, Payload: payload})
}

// Methods 返回已注册的 (实例, 方法) 快照(诊断用)。
func (s *GRPCServer) Methods() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.handlers))
	for key := range s.handlers {
		addr, full, _ := strings.Cut(key, "\x00")
		out = append(out, addr+" "+full)
	}
	sort.Strings(out)
	return out
}

// ============================ gRPC 客户端 ============================

// Client 是 gRPC 客户端:Registry 解析 → Balancer 选点 → 客户端拦截器链 → 命中实例。
// 服务端不知道客户端是谁,客户端拦截器就是熔断 / 重试 / 灰度的落点。
type Client struct {
	grpc   *GRPCServer
	lb     *Balancer
	mu     sync.RWMutex
	mws    []clientMW
	nextID int
}

type clientMW struct {
	id int
	fn UnaryInterceptor
}

func NewClient(grpc *GRPCServer, lb *Balancer) *Client {
	return &Client{grpc: grpc, lb: lb}
}

// Use 注册客户端拦截器,返回注销函数;注册顺序即外层到内层。
func (c *Client) Use(i UnaryInterceptor) GoTenon.Disposer {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	c.mws = append(c.mws, clientMW{id: id, fn: i})
	c.mu.Unlock()
	return func() error {
		c.mu.Lock()
		defer c.mu.Unlock()
		for idx, m := range c.mws {
			if m.id == id {
				c.mws = append(c.mws[:idx], c.mws[idx+1:]...)
				return nil
			}
		}
		return nil
	}
}

// Invoke 按服务名发起调用:选点后组装客户端拦截器链并向实例发起一元调用。
func (c *Client) Invoke(ctx context.Context, service, method string, payload []byte) (*GRPCResponse, error) {
	addr, err := c.lb.Pick(service)
	if err != nil {
		return nil, err
	}
	full := service + "/" + method

	c.mu.RLock()
	mws := append([]clientMW(nil), c.mws...)
	c.mu.RUnlock()

	terminal := UnaryHandler(func(ctx context.Context, req *GRPCRequest) (*GRPCResponse, error) {
		return c.grpc.Invoke(ctx, addr, full, req.Payload)
	})
	chain := terminal
	for i := len(mws) - 1; i >= 0; i-- {
		next, mw := chain, mws[i].fn
		chain = func(ctx context.Context, req *GRPCRequest) (*GRPCResponse, error) {
			return mw(ctx, req, next)
		}
	}
	return chain(ctx, &GRPCRequest{Service: service, Method: method, Payload: payload})
}

// ---------- 链路追踪 ----------

// Span 是一次调用的耗时记录(Kind: server / client)。
type Span struct {
	TraceID  string
	Kind     string
	Service  string
	Method   string
	Instance string
	Start    time.Time
	Duration time.Duration
	Err      error
}

// Tracer 收集 span;插件卸载即停止上报。
type Tracer struct {
	mu    sync.Mutex
	spans []Span
}

func (t *Tracer) Add(s Span) {
	t.mu.Lock()
	t.spans = append(t.spans, s)
	t.mu.Unlock()
}

func (t *Tracer) Snapshot() []Span {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]Span(nil), t.spans...)
}

func (t *Tracer) Clear() {
	t.mu.Lock()
	t.spans = nil
	t.mu.Unlock()
}
