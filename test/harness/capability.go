package harness

import "GoTenon"

// Capability 验证能力声明与内核治理:能力/状态索引、服务发现、订阅(TypeIndex 消息)、信号表与信号动作。
func Capability() int {
	t := New("capability")
	m := Manager()

	Section("能力声明与索引")
	render := &Plugin{
		PluginName: "render",
		Caps: map[string]any{
			"render.draw": map[string]any{"description": "绘制文本", "inputSchema": map[string]any{"type": "object"}},
		},
		StatusMap: map[string]any{"state": "ready"},
	}
	Register(t, m, render, nil)
	Enable(t, m, "render")

	caps, ok := m.Capabilities("render")
	t.Check(ok && caps["render.draw"] != nil, "能力入索引: %v %v", caps, ok)
	found := m.Discover(func(e GoTenon.CapabilityEntry) bool { return e.Name == "render.draw" && e.Available })
	t.Check(len(found) == 1, "Discover 命中 = %d, want 1", len(found))
	if st, ok := m.Status("render"); ok {
		t.Eq(st["state"], "ready", "Status 入索引")
	} else {
		t.Check(false, "Status 不可读")
	}

	Section("订阅:内核用 TypeIndex 消息通知订阅者")
	watcher := &Plugin{PluginName: "watcher"}
	Register(t, m, watcher, nil)
	Enable(t, m, "watcher")
	stop, err := m.Subscribe(GoTenon.Subscription{Subscriber: "watcher"})
	t.Check(err == nil, "Subscribe: %v", err)

	before := len(watcher.Messages())
	_ = m.Disable("render") // ready true→false → down
	_ = m.Enable("render")  // false→true → up
	kinds := map[string]int{}
	for _, msg := range watcher.Messages()[before:] {
		if msg.Type != GoTenon.TypeIndex {
			continue
		}
		if ev, ok := msg.Data.(GoTenon.IndexEvent); ok {
			kinds[ev.Kind]++
		}
	}
	t.Check(kinds["down"] > 0, "收到 down 事件: %v", kinds)
	t.Check(kinds["up"] > 0, "收到 up 事件: %v", kinds)

	stop() // 退订
	before = len(watcher.Messages())
	_ = m.Disable("render")
	_ = m.Enable("render")
	t.Eq(len(watcher.Messages()), before, "退订后不再收到索引消息")

	Section("信号表:自定义与重名")
	if err := m.Signals.Register(GoTenon.Signal{Kind: GoTenon.SigCustom, Name: "SHUTDOWN"}); err == nil {
		t.Check(false, "重名信号应被拒")
	} else {
		t.Check(true, "")
	}

	Section("信号 → 内核动作")
	t.Check(Signal(m, "render", "", "SHUTDOWN") == nil, "SHUTDOWN 投递")
	if rt, _ := m.Get("render"); rt != nil {
		t.Check(!rt.Enable, "SHUTDOWN 后 render 未启用")
	}

	leaf := &Plugin{PluginName: "leaf"}
	Register(t, m, leaf, nil)
	Enable(t, m, "leaf")
	t.Check(Signal(m, "host", "leaf", "REQ_STOP") == nil, "REQ_STOP 投递")
	if rt, _ := m.Get("leaf"); rt != nil {
		t.Check(!rt.Loaded(), "REQ_STOP 后 leaf 卸载")
	}

	t.Check(Signal(m, "host", "leaf", "REQ_ENABLE") == nil, "REQ_ENABLE 投递")
	if rt, _ := m.Get("leaf"); rt != nil {
		t.Check(rt.Loaded(), "REQ_ENABLE 后 leaf 装载")
	}

	t.Check(Signal(m, "host", "leaf", "REQ_DELETE") == nil, "REQ_DELETE 投递")
	if _, ok := m.Get("leaf"); ok {
		t.Check(false, "REQ_DELETE 后 leaf 应移除")
	} else {
		t.Check(true, "")
	}

	t.Check(Signal(m, "render", "", "FAULT") == nil, "FAULT 投递")
	if rt, _ := m.Get("render"); rt != nil {
		t.Eq(rt.State, GoTenon.Failed, "FAULT 后 State=Failed")
	}

	return t.Done()
}
