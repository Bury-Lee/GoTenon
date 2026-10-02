// main.go —— 游戏主进程模拟：演示内核四件套与可逆生命周期。
//
// 运行：cd example/game_example && go run .
// 本文件只负责宿主侧：宿主 API、Loader/Logger、演练脚本；组件见 plugin.go。
package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"GoTenon"
)

// ---------- 宿主实现 ----------

type host struct {
	mu    sync.Mutex
	level int
	hud   string
	svc   map[string]any // 宿主自持的服务表：组件在装载期经此互通
}

func (h *host) Log(msg string)   { fmt.Println("[game]", msg) }
func (h *host) PlayerLevel() int { return h.level }
func (h *host) SetHUD(text string) {
	h.mu.Lock()
	h.hud = text
	h.mu.Unlock()
}
func (h *host) HUD() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.hud
}
func (h *host) Provide(name string, v any) {
	h.mu.Lock()
	h.svc[name] = v
	h.mu.Unlock()
}
func (h *host) Service(name string) (any, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	v, ok := h.svc[name]
	return v, ok
}

// ---------- Loader / Logger ----------

type gameLoader struct{ concurrency int }

func (l *gameLoader) IntoRegister(name string) error { fmt.Printf("[loader] 注册 %s\n", name); return nil }
func (l *gameLoader) Delete(name string) error       { fmt.Printf("[loader] 删除 %s\n", name); return nil }
func (l *gameLoader) Enable(name string) error       { fmt.Printf("[loader] 启用 %s\n", name); return nil }
func (l *gameLoader) Disable(name string) error      { fmt.Printf("[loader] 禁用 %s\n", name); return nil }
func (l *gameLoader) Hook(name string) error         { return nil }
func (l *gameLoader) Timeout(string) time.Duration   { return 2 * time.Second }
func (l *gameLoader) Concurrency() int {
	if l.concurrency <= 0 {
		return 3
	}
	return l.concurrency
}

func logf(level GoTenon.Level, msg string) {
	names := [...]string{"DEBUG", "INFO", "WARN", "ERROR"}
	fmt.Printf("[GoTenon][%s] %s\n", names[level], msg)
}

func section(title string) { fmt.Printf("\n===== %s =====\n", title) }

var dumpList = []string{"storage", "render", "hud", "quest", "ai", "watcher", "orphan", "late"}

func dump(m *GoTenon.Manager) {
	fmt.Println("[宿主] 插件状态:")
	for _, name := range dumpList {
		rt, ok := m.Get(name)
		if !ok {
			continue
		}
		fmt.Printf("  %-8s State=%-9s Enable=%-5v Loaded=%-5v Refs=%d Missing=%v\n",
			name, rt.State, rt.Enable, rt.Loaded(), len(rt.Dependenced), rt.Missing)
	}
}

// demoContext 演示上下文能力：Info 继承、私有/共享槽位、配置层。
func demoContext(root *GoTenon.GoTenonContext) {
	child := root.Extend("child")
	child.SetInfo("env", "prod")
	v1, _ := child.GetInfo("env")
	v2, _ := root.GetInfo("env")
	fmt.Printf("[上下文] Info 覆盖：子 env=%v，父 env=%v（子不影响父）\n", v1, v2)

	child.Isolate("svc/db")
	child.SlotOf("svc/db").Value = "child-db"
	fmt.Printf("[上下文] 私有槽位：子 svc/db=%v，父 svc/db=%v\n",
		child.SlotOf("svc/db").Value, root.SlotOf("svc/db"))

	root.IsolateLabel("svc/cache", "shared")
	root.SlotOf("svc/cache").Value = "shared-cache"
	a := root.Extend("tenant-a")
	b := root.Extend("tenant-b")
	fmt.Printf("[上下文] label 共享槽：兄弟同一槽=%v，值=%v\n",
		a.SlotOf("svc/cache") == b.SlotOf("svc/cache"), a.SlotOf("svc/cache").Value)

	root.Intercept("net", "layer-root")
	a.Intercept("net", "layer-a")
	fmt.Printf("[上下文] Config(net) root-first = %v\n", a.Config("net"))
}

func main() {
	section("0. 宿主启动")
	h := &host{level: 7, svc: map[string]any{}}
	root := GoTenon.New("game")
	root.SetInfo("version", "0.3.0")
	root.Isolate("game/api")
	root.SlotOf("game/api").Value = Game(h)
	root.Intercept("storage", "v1:")

	m := GoTenon.NewManager(root)
	m.Loader = &gameLoader{concurrency: 3}
	m.Logger = GoTenon.LoggerFunc(logf)

	section("1. 上下文能力")
	demoContext(root)

	section("2. 注册与成环检测")
	entries := []struct {
		p   GoTenon.PluginInfo
		cfg any
	}{
		{&storage{}, nil},
		{&render{}, nil},
		{&hud{}, nil},
		{&quest{}, questConfig{Fail: true}}, // 首次故意失败：Effect 回滚
		{&ai{}, nil},
		{&watcher{}, nil},
		{&orphan{}, nil}, // 依赖 late，暂未注册 → PENDING
		{&cycleA{}, nil},
	}
	for _, e := range entries {
		if _, err := m.Register(e.p, e.cfg); err != nil {
			panic(err)
		}
	}
	if _, err := m.Register(&storage{}, nil); err != nil {
		fmt.Printf("[宿主] 重复注册被拒: %v (DUPLICATE=%v)\n", err, GoTenon.IsCode(err, GoTenon.ErrDuplicate))
	}
	if _, err := m.Register(&cycleB{}, nil); err != nil {
		fmt.Printf("[宿主] 成环被拒: %v (INVALID_PLUGIN=%v)\n", err, GoTenon.IsCode(err, GoTenon.ErrInvalidPlugin))
	}
	if _, ok := m.Get("cycleB"); !ok {
		fmt.Println("[宿主] cycleB 已回滚，未留在插件表")
	}

	section("3. 启用与收敛")
	for _, name := range []string{"storage", "render", "hud", "quest", "ai", "watcher", "orphan"} {
		fmt.Printf("[宿主] Enable(%s) → %v\n", name, m.Enable(name))
	}
	time.Sleep(250 * time.Millisecond) // 让 ai 的思考协程跑几拍
	dump(m)

	section("4. 能力：索引是消息驱动的，可查询与发现")
	for _, name := range []string{"storage", "render", "quest", "ai"} {
		if caps, ok := m.Capabilities(name); ok {
			names := make([]string, 0, len(caps))
			for k := range caps {
				names = append(names, k)
			}
			fmt.Printf("[宿主] %-8s 能力=%v\n", name, names)
		}
	}
	fmt.Println("[宿主] Discover(available) →")
	for _, e := range m.Discover(func(e GoTenon.CapabilityEntry) bool { return e.Available }) {
		fmt.Printf("  %-14s @%-8s\n", e.Name, e.Plugin)
	}

	section("5. 消息：类型化信封，内核原封投递")
	_ = m.Send(GoTenon.Message{Name: "render", Type: GoTenon.TypeRaw, Data: "游戏帧"})
	_ = m.Send(GoTenon.Message{Name: "storage", Type: GoTenon.TypeRaw, Data: map[string]any{"key": "k", "value": "v"}})
	_ = m.Send(GoTenon.Message{Name: "ai", Type: GoTenon.TypeRaw, Data: context.Background()})
	_, _ = m.Dispatch(&GoTenon.Message{Name: "", Type: GoTenon.TypeContext, Data: context.Background()})
	fmt.Printf("[宿主] Send(ghost) → %v (NOT_PROVIDED=%v)\n",
		m.Send(GoTenon.Message{Name: "ghost"}), GoTenon.IsCode(m.Send(GoTenon.Message{Name: "ghost"}), GoTenon.ErrNotProvided))

	section("6. 订阅：内核用 TypeIndex 消息通知订阅者")
	stop, err := m.Subscribe(GoTenon.Subscription{
		Subscriber: "watcher",
		Filter:     func(e GoTenon.IndexEvent) bool { return e.Plugin == "render" || e.Plugin == "hud" },
	})
	if err != nil {
		panic(err)
	}
	_ = m.Disable("hud")   // hud 卸载 → render 引用归零自动卸载
	_ = m.Enable("render") // 重新启用 → 再次上下线
	stop()

	section("7. 修复 quest、补注册依赖，观察自动装配")
	if err := m.Update("quest", questConfig{Fail: false}); err != nil {
		panic(err)
	}
	fmt.Println("[宿主] Update(quest) 成功；ai 的依赖恢复后自动装载")
	if _, err := m.Register(&late{}, nil); err != nil {
		panic(err)
	}
	_ = m.Enable("late") // orphan 依赖恢复后自动装载
	dump(m)

	section("8. 信号：组件向内核表达意图")
	reqStop, _ := m.Signals.Lookup("REQ_STOP")
	fmt.Printf("[宿主] 发 REQ_STOP(watcher) → %v\n", signalErr(m.Signal(GoTenon.SignalRequest{From: "host", Target: "watcher", Signal: reqStop})))
	shutdown, _ := m.Signals.Lookup("SHUTDOWN")
	fmt.Printf("[宿主] 发 SHUTDOWN(from=storage) → %v\n", signalErr(m.Signal(GoTenon.SignalRequest{From: "storage", Signal: shutdown})))
	fault, _ := m.Signals.Lookup("FAULT")
	fmt.Printf("[宿主] 发 FAULT(from=ai) → %v\n", signalErr(m.Signal(GoTenon.SignalRequest{From: "ai", Signal: fault})))
	dump(m)

	section("9. 卸载与级联")
	for _, name := range []string{"ai", "hud", "quest", "storage", "late", "orphan", "render", "watcher"} {
		_ = m.Disable(name)
	}
	dump(m)

	section("10. 并行装载（3 个互不依赖的 worker 扇入 batch）")
	for _, id := range []string{"a", "b", "c"} {
		if _, err := m.Register(&worker{id: id, delay: 120 * time.Millisecond}, nil); err != nil {
			panic(err)
		}
	}
	if _, err := m.Register(&batch{}, nil); err != nil {
		panic(err)
	}
	start := time.Now()
	if err := m.Enable("batch"); err != nil {
		panic(err)
	}
	fmt.Printf("[宿主] 并行装载总耗时 %s（串行理论下限 %s）\n",
		time.Since(start).Round(time.Millisecond), 3*120*time.Millisecond)
}

func signalErr(_ *GoTenon.Message, err error) error { return err }
