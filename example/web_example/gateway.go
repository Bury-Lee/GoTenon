// gateway.go —— 业务插件:gateway(HTTP → gRPC 网关)。
//
// 注册 /api/greet,把请求桥接到 demo.Greeter/SayHello;
// 网关路由与发现登记是一个 Effect 事务,任一步失败整体回滚。
package main

import (
	"fmt"
	"net/http"
	"sync"

	"GoTenon"
)

type gatewayPlugin struct {
	basePlugin
	mu      sync.Mutex
	grpc    *GRPCServer
	reg     *Registry
	applied bool
}

func (p *gatewayPlugin) Name() string { return "gateway" }
func (p *gatewayPlugin) Inject() []string {
	return []string{"http", "grpc", "greeter", "discovery"}
}
func (p *gatewayPlugin) Desc() map[string]string {
	return map[string]string{"provides": "/api/greet", "note": "HTTP→gRPC 桥接"}
}
func (p *gatewayPlugin) Status() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.applied {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "route": "/api/greet"})
}

func (p *gatewayPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	rv, err := service(ctx, "svc/router")
	if err != nil {
		return err
	}
	gv, err := service(ctx, "svc/grpc")
	if err != nil {
		return err
	}
	grpcSrv := gv.(*GRPCServer)
	var reg *Registry
	if regv, err := service(ctx, "svc/registry"); err == nil {
		reg = regv.(*Registry)
	}
	router := rv.(*Router)

	// Effect:网关路由 + 发现登记是一体,任一步失败整体回滚
	_, err = ctx.Effect(func() error {
		ctx.Register(router.Handle("GET", "/api/greet", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			name := r.URL.Query().Get("name")
			resp, err := grpcSrv.Invoke(r.Context(), "demo.Greeter", "SayHello", []byte(name))
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadGateway)
				return
			}
			if resp.Err != nil {
				http.Error(w, resp.Err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"reply": string(resp.Payload), "via": "grpc"})
		})))
		if reg != nil {
			ctx.Register(reg.Register("gateway", "inproc://gateway"))
		}
		return nil
	}, "gateway-bridge")
	if err != nil {
		return err
	}

	p.mu.Lock()
	p.grpc = grpcSrv
	p.reg = reg
	p.applied = true
	p.mu.Unlock()
	fmt.Println("[gateway] /api/greet → demo.Greeter/SayHello")
	return nil
}

func (p *gatewayPlugin) End() error {
	p.mu.Lock()
	p.applied = false
	p.mu.Unlock()
	return nil
}

func (p *gatewayPlugin) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("gateway", m)
	}
	return nil
}

func (p *gatewayPlugin) Function() map[string]any {
	return map[string]any{"gateway.greet": cap("经 HTTP→gRPC 桥接打招呼", "name")}
}
func (p *gatewayPlugin) ExecuteFunction(any) {}
