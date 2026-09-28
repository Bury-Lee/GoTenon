package GoTenon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// testPlugin 是可配置的假插件,覆盖各成功/失败路径。
type testPlugin struct {
	name       string
	inject     []string
	applyPanic bool
	startErr   error
	runErr     error
	dealErr    error
	dealPanic  bool
	failCfg    string
	created    *int32
	disposed   *int32
}

func (p *testPlugin) Name() string              { return p.name }
func (p *testPlugin) Desc() map[string]string   { return nil }
func (p *testPlugin) Inject() []string          { return p.inject }
func (p *testPlugin) Config() map[string]string { return nil }
func (p *testPlugin) Register() error           { return nil }

func (p *testPlugin) Apply(ctx *GoTenonContext, cfg any) error {
	if p.applyPanic {
		panic("apply boom")
	}
	if s, ok := cfg.(string); ok && p.failCfg != "" && s == p.failCfg {
		return errors.New("bad cfg")
	}
	ctx.Register(func() error {
		if p.disposed != nil {
			atomic.AddInt32(p.disposed, 1)
		}
		return nil
	})
	if p.created != nil {
		atomic.AddInt32(p.created, 1)
	}
	return nil
}

func (p *testPlugin) Start() error { return p.startErr }
func (p *testPlugin) Run() error   { return p.runErr }

func (p *testPlugin) DealWithMessage(context.Context) error {
	if p.dealPanic {
		panic("deal boom")
	}
	return p.dealErr
}

func (p *testPlugin) End() error { return nil }

// slowPlugin 在 Apply 中先注册、阻塞、再注册(用于超时后活性测试)。
type slowPlugin struct {
	name    string
	block   time.Duration
	lateErr error
	done    chan struct{}
}

func (p *slowPlugin) Name() string              { return p.name }
func (p *slowPlugin) Desc() map[string]string   { return nil }
func (p *slowPlugin) Inject() []string          { return nil }
func (p *slowPlugin) Config() map[string]string { return nil }
func (p *slowPlugin) Register() error           { return nil }

func (p *slowPlugin) Apply(ctx *GoTenonContext, cfg any) error {
	ctx.Register(func() error { return nil })
	time.Sleep(p.block)
	d := ctx.Register(func() error { return nil }) // dispose 后应被拒
	p.lateErr = d()
	if p.done != nil {
		close(p.done)
	}
	return nil
}

func (p *slowPlugin) Start() error                          { return nil }
func (p *slowPlugin) Run() error                            { return nil }
func (p *slowPlugin) DealWithMessage(context.Context) error { return nil }
func (p *slowPlugin) End() error                            { return nil }

// timeoutLoader 提供装载超时预算。
type timeoutLoader struct{ d time.Duration }

func (l *timeoutLoader) IntoRegister(string) error    { return nil }
func (l *timeoutLoader) Delete(string) error          { return nil }
func (l *timeoutLoader) Enable(string) error          { return nil }
func (l *timeoutLoader) Disable(string) error         { return nil }
func (l *timeoutLoader) Hook(string) error            { return nil }
func (l *timeoutLoader) Timeout(string) time.Duration { return l.d }

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
