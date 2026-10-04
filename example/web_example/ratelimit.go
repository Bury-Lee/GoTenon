// ratelimit.go —— 业务插件:ratelimit(中间件即插件,可热调阈值)。
//
// 往路由表挂一个限流中间件;Update 会先卸载旧中间件再按新阈值重挂。
package main

import (
	"fmt"
	"net/http"
	"sync"

	"GoTenon"
)

type ratelimitConfig struct{ Limit int }

type ratelimitPlugin struct {
	basePlugin
	mu      sync.Mutex
	count   int
	limit   int
	applied bool
}

func (p *ratelimitPlugin) Name() string     { return "ratelimit" }
func (p *ratelimitPlugin) Inject() []string { return []string{"http"} }
func (p *ratelimitPlugin) Desc() map[string]string {
	return map[string]string{"note": "全局限流中间件"}
}
func (p *ratelimitPlugin) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.applied {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "limit": p.limit, "count": p.count})
}

func (p *ratelimitPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(ratelimitConfig)
	if c.Limit <= 0 {
		c.Limit = 1000
	}

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

	p.mu.Lock()
	p.limit = c.Limit
	p.count = 0
	p.applied = true
	p.mu.Unlock()
	fmt.Printf("[ratelimit] 中间件已挂载,阈值 %d\n", c.Limit)
	return nil
}

func (p *ratelimitPlugin) End() error {
	p.mu.Lock()
	p.applied = false
	p.mu.Unlock()
	return nil
}

func (p *ratelimitPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("ratelimit", m)
	}
	return nil
}

func (p *ratelimitPlugin) Function() map[string]any {
	return map[string]any{"ratelimit.set": cap("调整限流阈值", "limit")}
}
func (p *ratelimitPlugin) ExecuteFunction(any) {}
