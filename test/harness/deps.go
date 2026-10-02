package harness

import (
	"fmt"
	"time"

	"GoTenon"
)

// Deps 验证依赖图:成环检测、依赖闭包装载顺序、引用计数与 Kept、级联卸载、并行装载。
func Deps() int {
	t := New("deps")
	m := Manager()

	Section("成环检测")
	Register(t, m, &Plugin{PluginName: "ca", Deps: []string{"cb"}}, nil)
	_, err := m.Register(&Plugin{PluginName: "cb", Deps: []string{"ca"}}, nil)
	t.Check(GoTenon.IsCode(err, GoTenon.ErrInvalidPlugin), "成环 → ErrInvalidPlugin: %v", err)
	if _, ok := m.Get("cb"); ok {
		t.Check(false, "成环插件应回滚")
	} else {
		t.Check(true, "")
	}

	Section("依赖闭包与装载顺序(依赖先行)")
	var order []string
	mk := func(name string, deps []string) *Plugin {
		p := &Plugin{PluginName: name, Deps: deps}
		p.ApplyFn = func(*GoTenon.GoTenonContext, any) error { order = append(order, name); return nil }
		return p
	}
	Register(t, m, mk("a", nil), nil)
	Register(t, m, mk("b", []string{"a"}), nil)
	Register(t, m, mk("c", []string{"b"}), nil)
	Enable(t, m, "c")
	for _, n := range []string{"a", "b", "c"} {
		if rt, _ := m.Get(n); !rt.Loaded() {
			t.Check(false, "%s 应装载", n)
		} else {
			t.Check(true, "")
		}
	}
	t.Eq(fmt.Sprint(order), fmt.Sprint([]string{"a", "b", "c"}), "装载顺序")

	Section("引用计数与 Kept")
	prov := &Plugin{PluginName: "prov"}
	Register(t, m, prov, nil)
	Register(t, m, &Plugin{PluginName: "u1", Deps: []string{"prov"}}, nil)
	Register(t, m, &Plugin{PluginName: "u2", Deps: []string{"prov"}}, nil)
	Enable(t, m, "u1")
	Enable(t, m, "u2")
	if rt, _ := m.Get("prov"); rt != nil {
		t.Eq(len(rt.Dependenced), 2, "prov 引用数")
	}
	_ = m.Disable("prov")
	if rt, _ := m.Get("prov"); rt != nil {
		t.Eq(rt.State, GoTenon.Kept, "被依赖的禁用者 → Kept")
	}
	_ = m.Disable("u1")
	_ = m.Disable("u2")
	if rt, _ := m.Get("prov"); rt != nil {
		t.Check(!rt.Loaded(), "引用归零后自动卸载")
	}

	Section("级联卸载(依赖者先走)")
	_ = m.Disable("c")
	for _, n := range []string{"a", "b", "c"} {
		if rt, _ := m.Get(n); rt.Loaded() {
			t.Check(false, "%s 应卸载", n)
		} else {
			t.Check(true, "")
		}
	}

	Section("并行装载(同波次)")
	for _, id := range []string{"w1", "w2", "w3"} {
		Register(t, m, &Plugin{PluginName: id, ApplyFn: func(*GoTenon.GoTenonContext, any) error {
			time.Sleep(100 * time.Millisecond)
			return nil
		}}, nil)
	}
	Register(t, m, &Plugin{PluginName: "batch", Deps: []string{"w1", "w2", "w3"}}, nil)
	start := time.Now()
	Enable(t, m, "batch")
	elapsed := time.Since(start)
	t.Check(elapsed < 250*time.Millisecond, "并行耗时 %s 应 < 250ms(串行下限约 300ms)", elapsed.Round(time.Millisecond))

	return t.Done()
}
