// main.go —— 宿主演练:预挂内核服务、按依赖装载、HTTP 请求、热更新、优雅停机。
//
// 运行:cd example/web_example && go run .
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"GoTenon"
)

// webLoader 演示 Loader 钩子与并行度定制。
type webLoader struct{ concurrency int }

func (l *webLoader) IntoRegister(name string) error {
	fmt.Printf("[loader] 注册 %s\n", name)
	return nil
}
func (l *webLoader) Delete(name string) error     { fmt.Printf("[loader] 删除 %s\n", name); return nil }
func (l *webLoader) Enable(name string) error     { fmt.Printf("[loader] 启用 %s\n", name); return nil }
func (l *webLoader) Disable(name string) error    { fmt.Printf("[loader] 禁用 %s\n", name); return nil }
func (l *webLoader) Hook(name string) error       { return nil }
func (l *webLoader) Timeout(string) time.Duration { return 2 * time.Second }
func (l *webLoader) Concurrency() int {
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

// get 发起一次 HTTP GET,返回状态码与响应体。
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
	fmt.Printf("  GET %-22s → %d %s", path, code, body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		fmt.Println()
	}
}

func dump(m *GoTenon.Manager) {
	fmt.Println("[宿主] 插件状态:")
	for _, name := range []string{"http", "grpc", "discovery", "ratelimit", "users", "admin", "greeter", "gateway"} {
		rt, ok := m.Get(name)
		if !ok {
			continue
		}
		fmt.Printf("  %-10s State=%-9s Loaded=%-5v Inject=%v\n",
			name, rt.State, rt.Loaded(), rt.Plugin.Inject())
	}
}

func main() {
	section("0. 宿主:在 root 上预挂内核服务")
	root := GoTenon.New("web")
	root.SetInfo("app", "gotenon-web")

	router := NewRouter()
	grpcSrv := NewGRPCServer()
	registry := NewRegistry()

	root.Isolate("svc/router")
	root.SlotOf("svc/router").Value = router
	root.Isolate("svc/grpc")
	root.SlotOf("svc/grpc").Value = grpcSrv
	root.Isolate("svc/registry")
	root.SlotOf("svc/registry").Value = registry
	fmt.Println("[宿主] svc/router / svc/grpc / svc/registry 已挂到 root,插件沿祖先链解析")

	m := GoTenon.NewManager(root)
	m.Loader = &webLoader{concurrency: 4}
	m.Logger = GoTenon.LoggerFunc(logf)

	section("1. 注册插件(用 Inject 声明依赖)")
	entries := []struct {
		p   GoTenon.PluginInfo
		cfg any
	}{
		{&httpKernel{}, "127.0.0.1:0"},
		{&grpcKernel{}, nil},
		{&discoveryPlugin{}, nil},
		{&ratelimitPlugin{}, ratelimitConfig{Limit: 1000}},
		{&usersPlugin{}, usersConfig{Version: "v1"}},
		{&adminPlugin{}, nil},
		{&greeterPlugin{}, nil},
		{&gatewayPlugin{}, nil},
	}
	for _, e := range entries {
		if _, err := m.Register(e.p, e.cfg); err != nil {
			panic(err)
		}
	}

	section("2. 启用:收敛器按依赖波次自动装载")
	for _, name := range []string{"http", "grpc", "discovery", "ratelimit", "users", "admin", "greeter", "gateway"} {
		if err := m.Enable(name); err != nil {
			panic(err)
		}
	}
	dump(m)

	rt, _ := m.Get("http")
	addr := rt.Context.SlotOf("svc/http").Value.(*httpKernel).Addr()

	section("3. 路由命中:业务插件注册的端点")
	show(addr, "/api/users")
	show(addr, "/api/health")
	show(addr, "/admin/stats")
	show(addr, "/api/greet?name=GoTenon")
	fmt.Printf("[宿主] 当前路由表: %v\n", router.Dump())

	section("4. Disable(admin):路由随插件卸载而摘除")
	if err := m.Disable("admin"); err != nil {
		panic(err)
	}
	show(addr, "/admin/stats")
	fmt.Printf("[宿主] 当前路由表: %v\n", router.Dump())

	section("5. Update(users):热更新换版本,路由原子替换")
	if err := m.Update("users", usersConfig{Version: "v2"}); err != nil {
		panic(err)
	}
	show(addr, "/api/users")

	section("6. Effect 失败回滚:整组路由一起撤销")
	err := m.Update("users", usersConfig{Version: "v3", Fail: true})
	fmt.Printf("[宿主] Update(users, fail) → %v\n", err)
	show(addr, "/api/users")
	fmt.Printf("[宿主] 当前路由表: %v\n", router.Dump())
	if err := m.Update("users", usersConfig{Version: "v1"}); err != nil {
		panic(err)
	}
	show(addr, "/api/users")

	section("7. 中间件热调阈值:Update(ratelimit, Limit=3)")
	if err := m.Update("ratelimit", ratelimitConfig{Limit: 3}); err != nil {
		panic(err)
	}
	for i := 1; i <= 5; i++ {
		code, _ := get(addr, "/api/health")
		fmt.Printf("  第 %d 次 /api/health → %d\n", i, code)
	}

	section("8. 服务发现 + gRPC 直连")
	fmt.Printf("[宿主] registry.Resolve(greeter) = %v\n", registry.Resolve("greeter"))
	fmt.Printf("[宿主] registry.Resolve(gateway) = %v\n", registry.Resolve("gateway"))
	resp, err := grpcSrv.Invoke(context.Background(), "demo.Greeter", "SayHello", []byte("direct"))
	fmt.Printf("[宿主] gRPC 直连 → %v err=%v\n", string(resp.Payload), err)

	section("9. 卸载与优雅停机")
	// 按依赖者先走的顺序禁用,引用归零的内核插件随之卸载
	for _, name := range []string{"gateway", "admin", "users", "ratelimit", "greeter", "discovery", "grpc", "http"} {
		if err := m.Disable(name); err != nil {
			panic(err)
		}
	}
	dump(m)
	code, body := get(addr, "/api/users")
	fmt.Printf("[宿主] 停机后访问 → %d %s\n", code, body)
	fmt.Println("[宿主] 全部卸载完成")
}
