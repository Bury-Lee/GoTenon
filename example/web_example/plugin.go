// plugin.go —— 内核插件与业务插件。
//
// 所有能力都通过「共享槽位解析 + 可逆副作用注册」接入:
//   - 内核插件(http/grpc/discovery):把底层 Server 的生命周期翻译成 Disposer;
//   - 业务插件(users/admin/ratelimit/greeter/gateway):Inject 内核插件,Apply 期注册路由/服务。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"GoTenon"
)

// basePlugin 提供 PluginInfo 的默认实现,插件只覆写关心的钩子。
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

// writeJSON 统一 JSON 输出。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ---------- 内核插件:HTTP ----------

// httpKernel 拥有 *http.Server:监听、路由表挂载、优雅停机。
type httpKernel struct {
	basePlugin
	addr string
	srv  *http.Server
	ln   net.Listener
}

func (p *httpKernel) Name() string { return "http" }
func (p *httpKernel) Desc() map[string]string {
	return map[string]string{"provides": "svc/http", "note": "HTTP 路由内核"}
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

	// 关闭是可逆副作用:插件卸载即优雅停机,不留端口占用
	ctx.Register(func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return p.srv.Shutdown(shutdownCtx)
	})
	go func() { _ = p.srv.Serve(ln) }()

	// 发布自身服务:业务插件通过 Inject("http") 声明依赖
	ctx.Isolate("svc/http")
	ctx.SlotOf("svc/http").Value = p
	fmt.Printf("[http] 监听 %s,路由表已挂载\n", p.addr)
	return nil
}

// Addr 返回实际监听地址(示例用 :0 让系统分配端口)。
func (p *httpKernel) Addr() string { return p.addr }

// ---------- 内核插件:gRPC ----------

// grpcKernel 激活进程内 gRPC 并叠加默认拦截器(真实项目里是 grpc.Server)。
type grpcKernel struct {
	basePlugin
}

func (p *grpcKernel) Name() string { return "grpc" }
func (p *grpcKernel) Desc() map[string]string {
	return map[string]string{"provides": "svc/grpc", "note": "gRPC 内核(进程内实现)"}
}

func (p *grpcKernel) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	srv := v.(*GRPCServer)

	// 默认拦截器:日志。tracing / recovery / metrics 同理,都可由插件叠加。
	ctx.Register(srv.Use(func(c context.Context, req *GRPCRequest, next UnaryHandler) (*GRPCResponse, error) {
		start := time.Now()
		resp, err := next(c, req)
		fmt.Printf("[grpc] %s/%s 耗时 %s err=%v\n",
			req.Service, req.Method, time.Since(start).Round(time.Microsecond), err)
		return resp, err
	}))

	ctx.Isolate("svc/grpc")
	ctx.SlotOf("svc/grpc").Value = srv
	fmt.Println("[grpc] 内核就绪(形状对齐 grpc.Server)")
	return nil
}

// ---------- 内核插件:服务发现 ----------

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

// ---------- 业务插件:users(Effect 事务化路由组 + 热更新) ----------

type usersConfig struct {
	Version string
	Fail    bool // 演示 Effect:body 失败整组路由回滚
}

type usersPlugin struct {
	basePlugin
	version string
}

func (p *usersPlugin) Name() string     { return "users" }
func (p *usersPlugin) Inject() []string { return []string{"http"} }
func (p *usersPlugin) Desc() map[string]string {
	return map[string]string{"provides": "/api/users,/api/health"}
}

func (p *usersPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(usersConfig)
	if c.Version == "" {
		c.Version = "v1"
	}
	v, err := service(ctx, "svc/router")
	if err != nil {
		return err
	}
	router := v.(*Router)
	p.version = c.Version

	// Effect:一组路由是一个事务,任一步失败整体回滚
	_, err = ctx.Effect(func() error {
		ctx.Register(router.Handle("GET", "/api/users", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"users":   []string{"alice", "bob"},
				"version": p.version,
			})
		})))
		ctx.Register(router.Handle("GET", "/api/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"ok": true, "version": p.version})
		})))
		if c.Fail {
			return errors.New("users: 配置校验失败,整组路由回滚")
		}
		return nil
	}, "users-routes")
	if err != nil {
		return err
	}
	fmt.Printf("[users] 已注册 /api/users 与 /api/health(%s)\n", p.version)
	return nil
}

// ---------- 业务插件:admin(可单独下线的路由) ----------

type adminPlugin struct {
	basePlugin
}

func (p *adminPlugin) Name() string     { return "admin" }
func (p *adminPlugin) Inject() []string { return []string{"http"} }
func (p *adminPlugin) Desc() map[string]string {
	return map[string]string{"provides": "/admin/stats"}
}

func (p *adminPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/router")
	if err != nil {
		return err
	}
	router := v.(*Router)
	ctx.Register(router.Handle("GET", "/admin/stats", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"plugins": "ok", "uptime": "42s"})
	})))
	fmt.Println("[admin] /admin/stats 上线")
	return nil
}

func (p *adminPlugin) End() error {
	fmt.Println("[admin] End:统计快照落盘")
	return nil
}

// ---------- 业务插件:ratelimit(中间件即插件,可热调阈值) ----------

type ratelimitConfig struct{ Limit int }

type ratelimitPlugin struct {
	basePlugin
	mu    sync.Mutex
	count int
	limit int
}

func (p *ratelimitPlugin) Name() string     { return "ratelimit" }
func (p *ratelimitPlugin) Inject() []string { return []string{"http"} }
func (p *ratelimitPlugin) Desc() map[string]string {
	return map[string]string{"note": "全局限流中间件"}
}

func (p *ratelimitPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(ratelimitConfig)
	if c.Limit <= 0 {
		c.Limit = 1000
	}
	p.limit = c.Limit
	p.count = 0

	v, err := service(ctx, "svc/router")
	if err != nil {
		return err
	}
	router := v.(*Router)
	ctx.Register(router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p.mu.Lock()
			p.count++
			n := p.count
			p.mu.Unlock()
			if n > p.limit {
				http.Error(w, "429 too many requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}))
	fmt.Printf("[ratelimit] 中间件已挂载,阈值 %d\n", p.limit)
	return nil
}

// ---------- 业务插件:greeter(提供 gRPC 服务 + 服务发现实例) ----------

type greeterPlugin struct {
	basePlugin
}

func (p *greeterPlugin) Name() string     { return "greeter" }
func (p *greeterPlugin) Inject() []string { return []string{"grpc", "discovery"} }
func (p *greeterPlugin) Desc() map[string]string {
	return map[string]string{"provides": "demo.Greeter"}
}

func (p *greeterPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	srv := v.(*GRPCServer)
	ctx.Register(srv.RegisterService("demo.Greeter/SayHello", func(c context.Context, req *GRPCRequest) (*GRPCResponse, error) {
		name := string(req.Payload)
		if name == "" {
			name = "world"
		}
		return &GRPCResponse{Payload: []byte("hello, " + name)}, nil
	}))

	// 登记实例:插件卸载时自动摘除,天然支持灰度
	if rv, err := service(ctx, "svc/registry"); err == nil {
		ctx.Register(rv.(*Registry).Register("greeter", "inproc://greeter"))
	}
	fmt.Println("[greeter] 注册 demo.Greeter/SayHello 并在发现中心登记实例")
	return nil
}

// ---------- 业务插件:gateway(HTTP → gRPC 网关) ----------

type gatewayPlugin struct {
	basePlugin
	grpc *GRPCServer
	reg  *Registry
}

func (p *gatewayPlugin) Name() string { return "gateway" }
func (p *gatewayPlugin) Inject() []string {
	return []string{"http", "grpc", "greeter", "discovery"}
}
func (p *gatewayPlugin) Desc() map[string]string {
	return map[string]string{"provides": "/api/greet", "note": "HTTP→gRPC 桥接"}
}

func (p *gatewayPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	rv, err := service(ctx, "svc/router")
	if err != nil {
		return err
	}
	gv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	p.grpc = gv.(*GRPCServer)
	if regv, err := service(ctx, "svc/registry"); err == nil {
		p.reg = regv.(*Registry)
	}
	router := rv.(*Router)

	// Effect:网关路由 + 发现登记是一体,任一步失败整体回滚
	_, err = ctx.Effect(func() error {
		ctx.Register(router.Handle("GET", "/api/greet", http.HandlerFunc(p.handleGreet)))
		if p.reg != nil {
			ctx.Register(p.reg.Register("gateway", "inproc://gateway"))
		}
		return nil
	}, "gateway-bridge")
	if err != nil {
		return err
	}
	fmt.Println("[gateway] /api/greet → demo.Greeter/SayHello")
	return nil
}

func (p *gatewayPlugin) handleGreet(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	resp, err := p.grpc.Invoke(r.Context(), "demo.Greeter", "SayHello", []byte(name))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if resp.Err != nil {
		http.Error(w, resp.Err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"reply": string(resp.Payload), "via": "grpc"})
}
