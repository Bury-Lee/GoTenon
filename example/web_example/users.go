// users.go —— 业务插件:users(Effect 事务化路由组 + 热更新)。
//
// 一组路由视为一个事务:任一步失败整组回滚;Update 换版本时先卸载旧路由再注册新的。
package main

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"GoTenon"
)

type usersConfig struct {
	Version string
	Fail    bool // 演示 Effect:body 失败整组路由回滚
}

type usersPlugin struct {
	basePlugin
	mu      sync.Mutex
	version string
	applied bool
}

func (p *usersPlugin) Name() string     { return "users" }
func (p *usersPlugin) Inject() []string { return []string{"http"} }
func (p *usersPlugin) Desc() map[string]string {
	return map[string]string{"provides": "/api/users,/api/health"}
}
func (p *usersPlugin) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.applied {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "version": p.version})
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

	// Effect:一组路由是一个事务,任一步失败整体回滚
	_, err = ctx.Effect(func() error {
		ctx.Register(router.Handle("GET", "/api/users", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"users":   []string{"alice", "bob"},
				"version": c.Version,
			})
		})))
		ctx.Register(router.Handle("GET", "/api/health", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"ok": true, "version": c.Version})
		})))
		if c.Fail {
			return errors.New("users: 配置校验失败,整组路由回滚")
		}
		return nil
	}, "users-routes")
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.version = c.Version
	p.applied = true
	p.mu.Unlock()
	fmt.Printf("[users] 已注册 /api/users 与 /api/health(%s)\n", c.Version)
	return nil
}

func (p *usersPlugin) End() error {
	p.mu.Lock()
	p.applied = false
	p.mu.Unlock()
	return nil
}

func (p *usersPlugin) DealWithMessage(m GoTenon.Message) error {
	switch m.Type {
	case GoTenon.TypeIndex:
		logIndex("users", m)
	case GoTenon.TypeRaw:
		fmt.Printf("[users] 收到原始消息: %v\n", m.Data)
	}
	return nil
}

func (p *usersPlugin) Function() map[string]any {
	return map[string]any{"users.list": cap("列出用户")}
}
func (p *usersPlugin) ExecuteFunction(any) {}
