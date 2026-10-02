// greeter.go —— 业务插件:greeter(提供 gRPC 服务 + 服务发现实例)。
//
// 在 gRPC 内核上注册 demo.Greeter/SayHello,并在发现中心登记实例;
// 两者都以 Disposer 形式登记,卸载即摘除,天然支持灰度上下线。
package main

import (
	"context"
	"fmt"
	"sync"

	"GoTenon"
)

type greeterPlugin struct {
	basePlugin
	mu      sync.Mutex
	applied bool
}

func (p *greeterPlugin) Name() string     { return "greeter" }
func (p *greeterPlugin) Inject() []string { return []string{"grpc", "discovery"} }
func (p *greeterPlugin) Desc() map[string]string {
	return map[string]string{"provides": "demo.Greeter"}
}
func (p *greeterPlugin) Status() *map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.applied {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "service": "demo.Greeter"})
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

	p.mu.Lock()
	p.applied = true
	p.mu.Unlock()
	fmt.Println("[greeter] 注册 demo.Greeter/SayHello 并在发现中心登记实例")
	return nil
}

func (p *greeterPlugin) End() error {
	p.mu.Lock()
	p.applied = false
	p.mu.Unlock()
	return nil
}

func (p *greeterPlugin) DealWithMessage(m GoTenon.Message) error {
	switch m.Type {
	case GoTenon.TypeIndex:
		logIndex("greeter", m)
	case GoTenon.TypeRaw:
		fmt.Printf("[greeter] 收到原始消息: %v\n", m.Data)
	}
	return nil
}

func (p *greeterPlugin) Function() map[string]any {
	return map[string]any{"greeter.sayHello": cap("打招呼", "name")}
}
func (p *greeterPlugin) ExecuteFunction(any) {}
