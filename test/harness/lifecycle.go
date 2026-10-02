package harness

import (
	"errors"
	"time"

	"GoTenon"
)

// Lifecycle 验证组件加载:登记校验、生命周期钩子、失败回滚、超时、事务化更新、删除与级联。
func Lifecycle() int {
	t := New("lifecycle")
	m := Manager()

	Section("登记校验")
	_, err := m.Register(nil, nil)
	t.Check(GoTenon.IsCode(err, GoTenon.ErrInvalidPlugin), "nil 插件 → ErrInvalidPlugin")
	Register(t, m, &Plugin{PluginName: "a"}, nil)
	_, err = m.Register(&Plugin{PluginName: "a"}, nil)
	t.Check(GoTenon.IsCode(err, GoTenon.ErrDuplicate), "重复注册 → ErrDuplicate")

	Section("生命周期钩子 Apply → Start → Run")
	a := &Plugin{PluginName: "life"}
	Register(t, m, a, nil)
	Enable(t, m, "life")
	ap, st, rn, _ := a.Counts()
	t.Check(ap == 1 && st == 1 && rn == 1, "Apply/Start/Run 各一次, got %d/%d/%d", ap, st, rn)

	Section("失败回滚")
	panicP := &Plugin{PluginName: "panic", ApplyFn: func(*GoTenon.GoTenonContext, any) error { panic("apply boom") }}
	Register(t, m, panicP, nil)
	err = m.Enable("panic")
	t.Check(err != nil, "Apply panic → error")
	if rt, _ := m.Get("panic"); rt != nil {
		t.Eq(rt.State, GoTenon.Failed, "panic 后 State")
	}

	rb := &Plugin{PluginName: "rb", StartFn: func() error { return errors.New("start fail") }}
	rb.ApplyFn = func(ctx *GoTenon.GoTenonContext, _ any) error { rb.UseDisposer(ctx); return nil }
	Register(t, m, rb, nil)
	err = m.Enable("rb")
	t.Check(err != nil, "Start 失败 → error")
	t.Check(rb.Disposed() == 1, "半成品 Disposer 被逆序回收, got %d", rb.Disposed())
	if _, _, _, ed := rb.Counts(); ed == 1 {
		t.Check(true, "")
	} else {
		t.Check(false, "Start 失败后应调用 End, got %d", ed)
	}

	Section("缺失依赖 → PENDING")
	pending := &Plugin{PluginName: "pending", Deps: []string{"missing"}}
	Register(t, m, pending, nil)
	Enable(t, m, "pending")
	if rt, _ := m.Get("pending"); rt != nil {
		t.Eq(rt.State, GoTenon.Pending, "缺失依赖 → Pending")
		t.Check(len(rt.Missing) == 1 && rt.Missing[0] == "missing", "Missing 记录缺口: %v", rt.Missing)
	}

	Section("装载超时 → 回滚并拒绝晚到的注册")
	slow := &SlowPlugin{PluginName: "slow", Block: 200 * time.Millisecond, Done: make(chan struct{})}
	m.Loader = Loader{D: 30 * time.Millisecond}
	Register(t, m, slow, nil)
	err = m.Enable("slow")
	t.Check(GoTenon.IsCode(err, GoTenon.ErrTimeout), "装载超时 → ErrTimeout: %v", err)
	<-slow.Done
	t.Check(GoTenon.IsCode(slow.LateErr, GoTenon.ErrInactiveEffect), "超时后注册被拒: %v", slow.LateErr)
	m.Loader = nil

	Section("事务化 Update")
	up := &Plugin{PluginName: "up", ApplyFn: func(_ *GoTenon.GoTenonContext, cfg any) error {
		if s, ok := cfg.(string); ok && s == "bad" {
			return errors.New("bad cfg")
		}
		return nil
	}}
	Register(t, m, up, "good")
	Enable(t, m, "up")
	err = m.Update("up", "bad")
	t.Check(err != nil, "Update(bad) 失败")
	if rt, _ := m.Get("up"); rt != nil {
		t.Eq(rt.Config, "good", "回滚到旧配置")
		t.Check(rt.Loaded(), "回滚后仍装载")
	}

	Section("删除与级联")
	base := &Plugin{PluginName: "base"}
	dep := &Plugin{PluginName: "dep", Deps: []string{"base"}}
	Register(t, m, base, nil)
	Register(t, m, dep, nil)
	Enable(t, m, "dep")
	err = m.Delete("base")
	t.Check(err != nil, "删除仍有装载依赖者的插件被拒")
	_ = m.Disable("dep")
	if rt, _ := m.Get("base"); rt != nil {
		t.Check(!rt.Loaded(), "依赖者卸载后 base 引用归零自动卸载")
	}
	err = m.Delete("base")
	t.Check(err == nil, "依赖者卸载后可删除 base: %v", err)
	if _, ok := m.Get("base"); ok {
		t.Check(false, "base 应已从插件表移除")
	} else {
		t.Check(true, "")
	}

	return t.Done()
}
