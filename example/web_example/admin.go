// admin.go —— 业务插件:admin(可单独下线的路由)。
//
// 注册 /admin/stats;禁用 admin 时路由随 Disposer 自动摘除。
package main

import (
	"fmt"
	"net/http"
	"sync"

	"GoTenon"
)

type adminPlugin struct {
	basePlugin
	mu      sync.Mutex
	applied bool
}

func (p *adminPlugin) Name() string     { return "admin" }
func (p *adminPlugin) Inject() []string { return []string{"http"} }
func (p *adminPlugin) Desc() map[string]string {
	return map[string]string{"provides": "/admin/stats"}
}
func (p *adminPlugin) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.applied {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "route": "/admin/stats"})
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
	p.mu.Lock()
	p.applied = true
	p.mu.Unlock()
	fmt.Println("[admin] /admin/stats 上线")
	return nil
}

func (p *adminPlugin) End() error {
	p.mu.Lock()
	p.applied = false
	p.mu.Unlock()
	fmt.Println("[admin] End:统计快照落盘")
	return nil
}

func (p *adminPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("admin", m)
	}
	return nil
}

func (p *adminPlugin) Function() map[string]any {
	return map[string]any{"admin.stats": cap("输出管理统计")}
}
func (p *adminPlugin) ExecuteFunction(any) {}
