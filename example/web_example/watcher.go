// watcher.go —— 订阅索引通知的组件。
//
// watcher 自身不提供业务能力,只在 main 调用 m.Subscribe 后接收内核投递的
// TypeIndex 消息,演示「索引是消息驱动的」。
package main

import (
	"fmt"
	"sync"

	"GoTenon"
)

type watcher struct {
	basePlugin
	mu     sync.Mutex
	seen   []string
	loaded bool
}

func (p *watcher) Name() string            { return "watcher" }
func (p *watcher) Desc() map[string]string { return map[string]string{"subscribes": "index"} }
func (p *watcher) Status() *map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loaded {
		return statusOf(map[string]any{"state": "pending", "seen": len(p.seen)})
	}
	return statusOf(map[string]any{"state": "ready", "seen": len(p.seen)})
}

func (p *watcher) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	p.mu.Lock()
	p.loaded = true
	p.mu.Unlock()
	return nil
}

func (p *watcher) End() error {
	p.mu.Lock()
	p.loaded = false
	p.mu.Unlock()
	return nil
}

func (p *watcher) DealWithMessage(m GoTenon.Message) error {
	if m.Type != GoTenon.TypeIndex {
		return nil
	}
	ev, ok := m.Data.(GoTenon.IndexEvent)
	if !ok {
		return nil
	}
	p.mu.Lock()
	p.seen = append(p.seen, ev.Kind+" "+ev.Plugin)
	p.mu.Unlock()
	fmt.Printf("[watcher] 收到索引通知: %-6s %s\n", ev.Kind, ev.Plugin)
	return nil
}
