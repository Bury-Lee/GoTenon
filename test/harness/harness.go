// Package harness 提供可复用的探针插件与断言工具，供 test/ 下的 main 程序
// 通过「插件自身能力」验证内核:消息、组件加载、依赖、能力声明等。
package harness

import (
	"fmt"
	"sync"
	"time"

	"GoTenon"
)

// ---------- 断言 ----------

// T 是极简断言器:统计通过/失败，最后由 Done 打印摘要并返回失败数。
type T struct {
	name   string
	passed int
	failed int
}

func New(name string) *T { return &T{name: name} }

// Check 断言 cond;失败时打印原因。
func (t *T) Check(cond bool, format string, args ...any) {
	if cond {
		t.passed++
		return
	}
	t.failed++
	fmt.Printf("  [FAIL] %s\n", fmt.Sprintf(format, args...))
}

// Eq 断言 got == want(字符串化比较)。
func (t *T) Eq(got, want any, what string) {
	t.Check(fmt.Sprint(got) == fmt.Sprint(want), "%s: got %v, want %v", what, got, want)
}

// Done 打印摘要;返回失败数(可作为进程退出码)。
func (t *T) Done() int {
	status := "PASS"
	if t.failed > 0 {
		status = "FAIL"
	}
	fmt.Printf("== %s: %s (passed=%d failed=%d) ==\n", t.name, status, t.passed, t.failed)
	return t.failed
}

func Section(s string) { fmt.Printf("\n-- %s --\n", s) }

// ---------- 通用探针插件 ----------

// Base 提供 PluginInfo / FunctionOffer 的默认实现。
type Base struct{}

func (Base) Desc() map[string]string                  { return nil }
func (Base) Inject() []string                         { return nil }
func (Base) Status() map[string]any                  { return nil }
func (Base) Register() error                          { return nil }
func (Base) Apply(*GoTenon.GoTenonContext, any) error { return nil }
func (Base) Start() error                             { return nil }
func (Base) Run() error                               { return nil }
func (Base) DealWithMessage(GoTenon.Message) error    { return nil }
func (Base) End() error                               { return nil }
func (Base) Function() map[string]any                 { return nil }
func (Base) ExecuteFunction(any)                      {}

// Plugin 是可按字段定制的探针:记录生命周期次数与收到的消息，
// 并可注入失败/panic/自定义钩子，从而以最小成本覆盖各类内核路径。
type Plugin struct {
	Base

	PluginName string
	Deps       []string
	Caps       map[string]any
	StatusMap  map[string]any
	OnMessage  func(GoTenon.Message) error
	ApplyFn    func(*GoTenon.GoTenonContext, any) error
	StartFn    func() error
	RunFn      func() error
	EndFn      func() error

	mu       sync.Mutex
	applied  int
	started  int
	ran      int
	ended    int
	disposed int
	got      []GoTenon.Message
}

func (p *Plugin) Name() string             { return p.PluginName }
func (p *Plugin) Inject() []string         { return p.Deps }
func (p *Plugin) Function() map[string]any { return p.Caps }

func (p *Plugin) Status() map[string]any {
	if p.StatusMap == nil {
		return nil
	}
	return p.StatusMap
}

func (p *Plugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	p.mu.Lock()
	p.applied++
	p.mu.Unlock()
	if p.ApplyFn != nil {
		return p.ApplyFn(ctx, cfg)
	}
	return nil
}

func (p *Plugin) Start() error {
	p.mu.Lock()
	p.started++
	p.mu.Unlock()
	if p.StartFn != nil {
		return p.StartFn()
	}
	return nil
}

func (p *Plugin) Run() error {
	p.mu.Lock()
	p.ran++
	p.mu.Unlock()
	if p.RunFn != nil {
		return p.RunFn()
	}
	return nil
}

func (p *Plugin) End() error {
	p.mu.Lock()
	p.ended++
	p.mu.Unlock()
	if p.EndFn != nil {
		return p.EndFn()
	}
	return nil
}

func (p *Plugin) DealWithMessage(m GoTenon.Message) error {
	p.mu.Lock()
	p.got = append(p.got, m)
	p.mu.Unlock()
	if p.OnMessage != nil {
		return p.OnMessage(m)
	}
	return nil
}

// Messages 返回收到的消息快照。
func (p *Plugin) Messages() []GoTenon.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]GoTenon.Message(nil), p.got...)
}

// Counts 返回 Apply/Start/Run/End 调用次数。
func (p *Plugin) Counts() (applied, started, ran, ended int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.applied, p.started, p.ran, p.ended
}

// Disposed 返回已执行的 Disposer 数。仅在 ApplyFn 里调 UseDisposer 后有效。
func (p *Plugin) Disposed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disposed
}

// UseDisposer 在 ctx 上登记一个计数 Disposer，用于验证卸载/回滚的逆序回收。
func (p *Plugin) UseDisposer(ctx *GoTenon.GoTenonContext) {
	ctx.Register(func() error {
		p.mu.Lock()
		p.disposed++
		p.mu.Unlock()
		return nil
	})
}

// ---------- 慢插件(装载超时 / 超时后注册被拒) ----------

type SlowPlugin struct {
	Base
	PluginName string
	Block      time.Duration
	LateErr    error
	Done       chan struct{}
}

func (p *SlowPlugin) Name() string { return p.PluginName }
func (p *SlowPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	ctx.Register(func() error { return nil })
	time.Sleep(p.Block)
	d := ctx.Register(func() error { return nil }) // dispose 后应被拒
	p.LateErr = d()
	if p.Done != nil {
		close(p.Done)
	}
	return nil
}

// ---------- Loader(可定制装载超时) ----------

type Loader struct{ D time.Duration }

func (Loader) IntoRegister(string) error    { return nil }
func (Loader) Delete(string) error          { return nil }
func (Loader) Enable(string) error          { return nil }
func (Loader) Disable(string) error         { return nil }
func (Loader) Hook(string) error            { return nil }
func (l Loader) Timeout(string) time.Duration { return l.D }

// ---------- 便捷函数 ----------

// Manager 返回一个干净的测试管理器。
func Manager() *GoTenon.Manager { return GoTenon.NewManager(GoTenon.New("test")) }

// Register 登记并断言成功。
func Register(t *T, m *GoTenon.Manager, p GoTenon.PluginInfo, cfg any) {
	if _, err := m.Register(p, cfg); err != nil {
		t.Check(false, "register %s: %v", p.Name(), err)
	}
}

// Enable 启用并断言成功。
func Enable(t *T, m *GoTenon.Manager, name string) {
	if err := m.Enable(name); err != nil {
		t.Check(false, "enable %s: %v", name, err)
	}
}

// Signal 按信号名向内核投递信号(args 可选)。
func Signal(m *GoTenon.Manager, from, target, name string, args ...any) error {
	s, ok := m.Signals.Lookup(name)
	if !ok {
		return fmt.Errorf("signal %q not found", name)
	}
	var a any
	if len(args) > 0 {
		a = args[0]
	}
	_, err := m.Signal(GoTenon.SignalRequest{From: from, Target: target, Signal: s, Args: a})
	return err
}
