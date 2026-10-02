// kernel_http.go —— 内核插件:HTTP。
//
// httpKernel 拥有 *http.Server:监听、路由表挂载、优雅停机;
// 通过共享槽位 svc/http 发布自身,业务插件以 Inject("http") 声明依赖。
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

// httpKernel 拥有 *http.Server:监听、路由表挂载、优雅停机。
type httpKernel struct {
	basePlugin
	mu   sync.Mutex
	addr string
	srv  *http.Server
	ln   net.Listener
}

func (p *httpKernel) Name() string { return "http" }
func (p *httpKernel) Desc() map[string]string {
	return map[string]string{"provides": "svc/http", "note": "HTTP 路由内核"}
}
func (p *httpKernel) Status() *map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.addr == "" {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "addr": p.addr})
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
	srv := &http.Server{Handler: router.Handler()}
	p.mu.Lock()
	p.ln = ln
	p.addr = ln.Addr().String()
	p.srv = srv
	bound := p.addr
	p.mu.Unlock()

	// 关闭是可逆副作用:插件卸载即优雅停机,不留端口占用
	ctx.Register(func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
	go func() { _ = srv.Serve(ln) }()

	// 发布自身服务:业务插件通过 Inject("http") 声明依赖
	ctx.Isolate("svc/http")
	ctx.SlotOf("svc/http").Value = p
	fmt.Printf("[http] 监听 %s,路由表已挂载\n", bound)
	return nil
}

// Addr 返回实际监听地址(示例用 :0 让系统分配端口)。
func (p *httpKernel) Addr() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.addr
}

func (p *httpKernel) End() error {
	p.mu.Lock()
	p.addr = ""
	p.srv = nil
	p.ln = nil
	p.mu.Unlock()
	return nil
}

func (p *httpKernel) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("http", m)
	}
	return nil
}

func (p *httpKernel) Function() map[string]any {
	return map[string]any{"http.endpoint": cap("查询 HTTP 监听地址与运行状态")}
}
func (p *httpKernel) ExecuteFunction(any) {}
