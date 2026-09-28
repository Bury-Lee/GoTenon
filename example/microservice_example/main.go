// main.go —— 宿主演练:预挂内核服务、按依赖波次装载微服务、HTTP 网关、熔断/限流/追踪、优雅停机。
//
// 运行:cd example/microservice_example && go run .
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"GoTenon"
)

// microLoader 演示 Loader 钩子与并行度定制。
type microLoader struct{ concurrency int }

func (l *microLoader) IntoRegister(name string) error {
	fmt.Printf("[loader] 注册 %s\n", name)
	return nil
}
func (l *microLoader) Delete(name string) error { fmt.Printf("[loader] 删除 %s\n", name); return nil }
func (l *microLoader) Enable(name string) error { fmt.Printf("[loader] 启用 %s\n", name); return nil }
func (l *microLoader) Disable(name string) error {
	fmt.Printf("[loader] 禁用 %s\n", name)
	return nil
}
func (l *microLoader) Hook(name string) error       { return nil }
func (l *microLoader) Timeout(string) time.Duration { return 2 * time.Second }
func (l *microLoader) Concurrency() int {
	if l.concurrency <= 0 {
		return 4
	}
	return l.concurrency
}

func logf(level GoTenon.Level, msg string) {
	names := [...]string{"DEBUG", "INFO", "WARN", "ERROR"}
	fmt.Printf("[GoTenon %s] %s\n", names[level], msg)
}

func section(title string) { fmt.Printf("\n===== %s =====\n", title) }

func get(addr, path string) (int, string) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + path)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func show(addr, path string) {
	code, body := get(addr, path)
	fmt.Printf("  GET %-44s → %d %s", path, code, body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		fmt.Println()
	}
}

func dump(m *GoTenon.Manager, names []string) {
	fmt.Println("[宿主] 插件状态:")
	for _, name := range names {
		rt, ok := m.Get(name)
		if !ok {
			continue
		}
		fmt.Printf("  %-14s State=%-9s Loaded=%-5v Inject=%v\n",
			name, rt.State, rt.Loaded(), rt.Plugin.Inject())
	}
}

func main() {
	section("0. 宿主:在 root 上预挂微服务内核服务")
	root := GoTenon.New("microservice")
	root.SetInfo("app", "gotenon-microservice")

	registry := NewRegistry()
	grpcSrv := NewGRPCServer()
	router := NewRouter()

	root.Isolate("svc/registry")
	root.SlotOf("svc/registry").Value = registry
	root.Isolate("svc/grpc")
	root.SlotOf("svc/grpc").Value = grpcSrv
	root.Isolate("svc/router")
	root.SlotOf("svc/router").Value = router
	// 共享槽:先由宿主在 root 声明 label,再由 client 插件写入 —— 兄弟插件才能解析到
	root.IsolateLabel("svc/client", sharedLabel)
	root.IsolateLabel("svc/balancer", sharedLabel)
	root.IsolateLabel("svc/trace", sharedLabel)
	fmt.Println("[宿主] svc/registry / svc/grpc / svc/router 已挂到 root;svc/client / svc/balancer 共享槽已声明")

	m := GoTenon.NewManager(root)
	m.Loader = &microLoader{concurrency: 4}
	m.Logger = GoTenon.LoggerFunc(logf)

	section("1. 注册微服务插件(用 Inject 声明依赖)")
	entries := []struct {
		p   GoTenon.PluginInfo
		cfg any
	}{
		{&httpKernel{}, "127.0.0.1:0"},
		{&grpcKernel{}, nil},
		{&discoveryPlugin{}, nil},
		{&clientPlugin{}, nil},
		{&tracingPlugin{}, nil},
		{&ratelimitPlugin{}, ratelimitConfig{Limit: 1000}},
		{&authPlugin{}, authConfig{Token: "secret"}},
		{&circuitbreakerPlugin{}, circuitbreakerConfig{Threshold: 3, Cooldown: 250 * time.Millisecond}},
		{&usersPlugin{}, usersConfig{Version: "v1"}},
		{&ordersPlugin{}, nil},
		{&greeterPlugin{id: "a", addr: "inproc://greeter-a"}, nil},
		{&greeterPlugin{id: "b", addr: "inproc://greeter-b"}, nil},
		{&gatewayPlugin{}, nil},
	}
	names := []string{"http", "grpc", "discovery", "client", "tracing", "ratelimit", "auth",
		"circuitbreaker", "users", "orders", "greeter-a", "greeter-b", "gateway"}
	for _, e := range entries {
		if _, err := m.Register(e.p, e.cfg); err != nil {
			panic(err)
		}
	}

	section("2. 启用:收敛器按依赖波次自动装载")
	for _, name := range names {
		if err := m.Enable(name); err != nil {
			panic(err)
		}
	}
	dump(m, names)

	rt, _ := m.Get("http")
	addr := rt.Context.SlotOf("svc/http").Value.(*httpKernel).Addr()

	section("3. 微服务拓扑:发现中心与 gRPC 服务表")
	fmt.Printf("[宿主] registry.Services() = %v\n", registry.Services())
	fmt.Printf("[宿主] registry.Describe(demo.Greeter) = %v\n", registry.Describe("demo.Greeter"))
	fmt.Printf("[宿主] grpc.Methods() = %v\n", grpcSrv.Methods())
	if slot := root.SlotOf("svc/client"); slot != nil && slot.Value != nil {
		fmt.Printf("[宿主] root 共享槽 svc/client 已由 client 插件写入 = %T\n", slot.Value)
	}

	section("4. gRPC 直连(带 token / trace):拦截器链自动生效")
	direct := withMeta(context.Background(), metaToken, "secret")
	direct = withMeta(direct, metaTraceID, newTraceID())
	uaddr := registry.Resolve("demo.Users")[0]
	resp, err := grpcSrv.Invoke(direct, uaddr, "demo.Users/Get", []byte("alice"))
	if err != nil {
		fmt.Printf("[宿主] 直连失败: %v\n", err)
	} else {
		fmt.Printf("[宿主] grpc.Invoke(%s, demo.Users/Get) → %s\n", uaddr, resp.Payload)
	}

	section("5. HTTP 网关 → gRPC(多实例负载均衡 / 鉴权)")
	for i := 1; i <= 4; i++ {
		show(addr, fmt.Sprintf("/api/greet?name=GoTenon&token=secret&_=%d", i))
	}
	show(addr, "/api/users?id=alice&token=secret")
	show(addr, "/api/order?user=alice&token=secret")
	show(addr, "/api/greet?name=GoTenon") // 缺少 token → 401

	section("6. 限流插件热调阈值:Update(ratelimit, Limit=2)")
	if err := m.Update("ratelimit", ratelimitConfig{Limit: 2}); err != nil {
		panic(err)
	}
	for i := 1; i <= 3; i++ {
		code, _ := get(addr, "/api/users?id=alice&token=secret")
		fmt.Printf("  第 %d 次 /api/users → %d\n", i, code)
	}
	if err := m.Update("ratelimit", ratelimitConfig{Limit: 1000}); err != nil {
		panic(err)
	}

	section("7. 客户端熔断:连续失败开路,冷却后 half-open 恢复")
	// 上一节的 429 会被客户端拦截器计为一次失败,先重置熔断器状态
	if err := m.Update("circuitbreaker", circuitbreakerConfig{Threshold: 3, Cooldown: 250 * time.Millisecond}); err != nil {
		panic(err)
	}
	for i := 1; i <= 3; i++ {
		show(addr, "/api/order?user=boom&token=secret")
	}
	show(addr, "/api/order?user=alice&token=secret") // 开路 → 503,未到达 users
	time.Sleep(300 * time.Millisecond)
	show(addr, "/api/order?user=alice&token=secret") // 冷却后半开探测成功 → 200

	section("8. 服务发现 + 灰度:实例上下线即时生效")
	fmt.Printf("[宿主] registry.Describe(demo.Greeter) = %v\n", registry.Describe("demo.Greeter"))
	if err := m.Disable("greeter-a"); err != nil {
		panic(err)
	}
	show(addr, "/api/greet?name=gray&token=secret") // 只剩 greeter-b
	if err := m.Disable("greeter-b"); err != nil {
		panic(err)
	}
	show(addr, "/api/greet?name=none&token=secret") // 无实例 → 503
	if err := m.Enable("greeter-a"); err != nil {
		panic(err)
	}
	if err := m.Enable("greeter-b"); err != nil {
		panic(err)
	}

	section("9. 链路追踪:同一 trace id 贯穿 gateway → orders → users")
	if slot := root.SlotOf("svc/trace"); slot != nil && slot.Value != nil {
		slot.Value.(*Tracer).Clear() // 清空历史,聚焦本次调用
	}
	show(addr, "/api/order?user=alice&token=secret")
	show(addr, "/debug/traces")

	section("10. 卸载与优雅停机(依赖者先走)")
	for _, name := range []string{"gateway", "greeter-b", "greeter-a", "orders", "users",
		"circuitbreaker", "auth", "ratelimit", "tracing", "client", "discovery", "grpc", "http"} {
		if err := m.Disable(name); err != nil {
			panic(err)
		}
	}
	dump(m, names)
	code, body := get(addr, "/api/users?id=alice&token=secret")
	fmt.Printf("[宿主] 停机后访问 → %d %s\n", code, body)
	fmt.Println("[宿主] 全部卸载完成")
}
