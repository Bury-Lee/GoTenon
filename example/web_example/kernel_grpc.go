// kernel_grpc.go —— 内核插件:gRPC。
//
// grpcKernel 激活进程内 gRPC 并叠加默认拦截器(真实项目里是 grpc.Server);
// 通过共享槽位 svc/grpc 发布自身。
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"GoTenon"
)

// grpcKernel 激活进程内 gRPC 并叠加默认拦截器(真实项目里是 grpc.Server)。
type grpcKernel struct {
	basePlugin
	mu  sync.Mutex
	srv *GRPCServer
}

func (p *grpcKernel) Name() string { return "grpc" }
func (p *grpcKernel) Desc() map[string]string {
	return map[string]string{"provides": "svc/grpc", "note": "gRPC 内核(进程内实现)"}
}
func (p *grpcKernel) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.srv == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "transport": "inproc"})
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

	p.mu.Lock()
	p.srv = srv
	p.mu.Unlock()

	ctx.Isolate("svc/grpc")
	ctx.SlotOf("svc/grpc").Value = srv
	fmt.Println("[grpc] 内核就绪(形状对齐 grpc.Server)")
	return nil
}

func (p *grpcKernel) End() error {
	p.mu.Lock()
	p.srv = nil
	p.mu.Unlock()
	return nil
}

func (p *grpcKernel) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("grpc", m)
	}
	return nil
}

func (p *grpcKernel) Function() map[string]any {
	return map[string]any{"grpc.invoke": cap("进程内一元调用", "service", "method", "payload")}
}
func (p *grpcKernel) ExecuteFunction(any) {}
