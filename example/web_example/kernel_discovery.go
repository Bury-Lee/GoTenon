// kernel_discovery.go —— 内核插件:服务发现。
//
// discoveryPlugin 包装 Registry,通过共享槽位 svc/registry 发布自身;
// 业务插件(如 greeter/gateway)装载时登记实例,卸载时自动摘除。
package main

import (
	"fmt"
	"sync"

	"GoTenon"
)

type discoveryPlugin struct {
	basePlugin
	mu  sync.Mutex
	reg *Registry
}

func (p *discoveryPlugin) Name() string { return "discovery" }
func (p *discoveryPlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/registry", "note": "服务发现"}
}
func (p *discoveryPlugin) Status() *map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reg == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "backend": "inproc"})
}

func (p *discoveryPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	v, err := service(ctx, "svc/registry")
	if err != nil {
		return err
	}
	reg := v.(*Registry)
	p.mu.Lock()
	p.reg = reg
	p.mu.Unlock()

	ctx.Isolate("svc/registry")
	ctx.SlotOf("svc/registry").Value = reg
	fmt.Println("[discovery] 服务发现就绪")
	return nil
}

func (p *discoveryPlugin) End() error {
	p.mu.Lock()
	p.reg = nil
	p.mu.Unlock()
	return nil
}

func (p *discoveryPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("discovery", m)
	}
	return nil
}

func (p *discoveryPlugin) Function() map[string]any {
	return map[string]any{"discovery.resolve": cap("解析服务实例地址", "service")}
}
func (p *discoveryPlugin) ExecuteFunction(any) {}
