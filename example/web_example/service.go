// service.go —— 插件库对外的「内核服务」契约。
//
// 这些类型不属于 GoTenon 内核,而是插件库自身定义的能力:
//   - Router     :可撤销的 HTTP 路由表(每条路由 / 每段中间件对应一个 Disposer)
//   - GRPCServer :进程内 gRPC 服务注册表 + 一元拦截器链(形状对齐 grpc.Server)
//   - Registry   :服务发现,实例注册同样是 Disposer
//
// 宿主在 root 上下文预挂这些实例(Isolate + Slot),
// 插件通过 ctx.SlotOf 沿祖先链解析 —— 这是跨插件共享能力的推荐姿势。
package main

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"GoTenon"
)

// ---------- 可撤销 HTTP 路由表 ----------

// Router 是注册即副作用的 HTTP 路由表:
// Handle / Use 都返回 GoTenon.Disposer,插件卸载时由内核逆序摘除,天然支持热插拔。
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

// NewRouter 创建空路由表。
func NewRouter() *Router { return &Router{} }

// Handle 注册一条路由,返回幂等的注销函数。
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

// Use 追加一段中间件,返回注销函数。注册顺序即外层到内层。
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

// Handler 返回路由表的 http.Handler。每次请求读取当前快照,
// 因此注册 / 注销(热更新)对已启动的 http.Server 立即生效,无需重建 Server。
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
		// 后注册的在外层:逆序包裹
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i].mw(h)
		}
		h.ServeHTTP(w, req)
	})
}

// Dump 返回路由表快照(诊断用),按 "METHOD PATH" 排序。
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

// match 支持精确匹配与尾部通配:"/api/*" 命中 "/api/" 下的任意路径。
func match(pattern, path string) bool {
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(path, strings.TrimSuffix(pattern, "*"))
	}
	return pattern == path
}

// ---------- 进程内 gRPC ----------
//
// 真实项目里这里是 google.golang.org/grpc.Server:插件注册的是 protobuf 生成的服务。
// 示例用进程内注册表替代,保持零依赖,同时完整保留「服务注册 + 一元拦截器链」的形状。
// 接入真实 gRPC 时只替换本文件实现,插件层代码不变。

// GRPCRequest 是一元调用请求(fullMethod = Service + "/" + Method)。
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

// UnaryInterceptor 是一元拦截器:调用 next 前后可做日志/鉴权/熔断。
type UnaryInterceptor func(ctx context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error)

// GRPCServer 是进程内 gRPC 服务注册表 + 拦截器链。
type GRPCServer struct {
	mu           sync.RWMutex
	handlers     map[string]UnaryHandler
	interceptors []grpcMW
	nextID       int
}

type grpcMW struct {
	id int
	fn UnaryInterceptor
}

// NewGRPCServer 创建空服务注册表。
func NewGRPCServer() *GRPCServer {
	return &GRPCServer{handlers: make(map[string]UnaryHandler)}
}

// RegisterService 注册一元方法 handler,fullMethod 形如 "demo.Greeter/SayHello",
// 返回幂等注销函数。
func (s *GRPCServer) RegisterService(fullMethod string, h UnaryHandler) GoTenon.Disposer {
	s.mu.Lock()
	s.handlers[fullMethod] = h
	s.mu.Unlock()
	return func() error {
		s.mu.Lock()
		delete(s.handlers, fullMethod)
		s.mu.Unlock()
		return nil
	}
}

// Use 注册一元拦截器,返回注销函数。后注册的在外层。
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

// Invoke 组装拦截器链并调用方法;方法不存在返回错误。
func (s *GRPCServer) Invoke(ctx context.Context, service, method string, payload []byte) (*GRPCResponse, error) {
	full := service + "/" + method
	s.mu.RLock()
	h := s.handlers[full]
	mws := append([]grpcMW(nil), s.interceptors...)
	s.mu.RUnlock()
	if h == nil {
		return nil, fmt.Errorf("grpc: method %q not found", full)
	}

	chain := h
	for i := len(mws) - 1; i >= 0; i-- {
		next, mw := chain, mws[i].fn
		chain = func(ctx context.Context, req *GRPCRequest) (*GRPCResponse, error) {
			return mw(ctx, req, next)
		}
	}
	return chain(ctx, &GRPCRequest{Service: service, Method: method, Payload: payload})
}

// ---------- 服务发现 ----------

// Registry 是服务发现注册表:插件可登记实例地址,网关 / 客户端可解析。
type Registry struct {
	mu        sync.RWMutex
	instances map[string][]string
}

// NewRegistry 创建空注册表。
func NewRegistry() *Registry { return &Registry{instances: make(map[string][]string)} }

// Register 登记一个服务实例,返回幂等注销函数。
func (r *Registry) Register(service, addr string) GoTenon.Disposer {
	r.mu.Lock()
	r.instances[service] = append(r.instances[service], addr)
	r.mu.Unlock()
	return func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		list := r.instances[service]
		for i, a := range list {
			if a == addr {
				r.instances[service] = append(list[:i], list[i+1:]...)
				break
			}
		}
		return nil
	}
}

// Resolve 返回某服务的全部实例地址快照。
func (r *Registry) Resolve(service string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.instances[service]...)
}
