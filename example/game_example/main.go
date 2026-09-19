// main.go —— 游戏主进程模拟：宿主 API、插件注册与收敛、消息循环、动态治理。
//
// 运行：cd example && go run .
// 本文件只负责「主进程」侧：宿主实现、Loader / Logger、演练脚本；
// 插件实现见 plugin.go。
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
	m     *GoTenon.Manager
}

func (h *host) Log(msg string)     { fmt.Println("[game]", msg) }
func (h *host) PlayerLevel() int   { return h.level }
func (h *host) SetHUD(text string) { h.mu.Lock(); h.hud = text; h.mu.Unlock() }
func (h *host) HUD() string        { h.mu.Lock(); defer h.mu.Unlock(); return h.hud }

// Service 跨插件解析逃生口：v0.1 没有一等公民的跨插件服务 API。
func (h *host) Service(plugin, slot string) (any, bool) {
	if h.m == nil {
		return nil, false
	}
	rt, ok := h.m.Get(plugin)
	if !ok || rt.Context == nil {
		return nil, false
	}
	s := rt.Context.SlotOf(slot)
	if s == nil || s.Value == nil {
		return nil, false
	}
	return s.Value, true
}

type gameLoader struct {
	concurrency int // 0 表示缺省 3；可切换以对照串行/并行装载
}

func (l *gameLoader) IntoRegister(name string) error {
	fmt.Printf("[loader] 注册 %s\n", name)
	return nil
}
func (l *gameLoader) Delete(name string) error  { fmt.Printf("[loader] 删除 %s\n", name); return nil }
func (l *gameLoader) Enable(name string) error  { fmt.Printf("[loader] 启用 %s\n", name); return nil }
func (l *gameLoader) Disable(name string) error { fmt.Printf("[loader] 禁用 %s\n", name); return nil }
func (l *gameLoader) Hook(name string) error    { fmt.Printf("[loader] 收尾 %s\n", name); return nil }

func (l *gameLoader) Timeout(name string) time.Duration {
	if name == "slow" {
		return 80 * time.Millisecond
	}
	return 2 * time.Second
}

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

func dumpStates(m *GoTenon.Manager) {
	fmt.Println("[宿主] 插件状态:")
	for _, name := range []string{"storage", "render", "hud", "quest", "ai", "orphan", "slow", "late"} {
		rt, ok := m.Get(name)
		if !ok {
			continue
		}
		fmt.Printf("  %-7s State=%-9s Enable=%-5v Loaded=%-5v Deps=%d Refs=%d Missing=%v Err=%v\n",
			name, rt.State, rt.Enable, rt.Loaded(), len(rt.Dependence), len(rt.Dependenced), rt.Missing, rt.Err)
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
	fmt.Printf("[上下文] 私有槽位：子 svc/db=%v，父 svc/db=%v（父未受影响）\n",
		child.SlotOf("svc/db").Value, root.SlotOf("svc/db"))

	// 共享槽位：先在共同祖先声明 label，兄弟子树解析到同一槽
	root.IsolateLabel("svc/cache", "shared")
	root.SlotOf("svc/cache").Value = "shared-cache"
	a := root.Extend("tenant-a")
	b := root.Extend("tenant-b")
	fmt.Printf("[上下文] label 共享槽：兄弟同一槽 = %v，值=%v\n",
		a.SlotOf("svc/cache") == b.SlotOf("svc/cache"), a.SlotOf("svc/cache").Value)

	// 兄弟各自 IsolateLabel 不会共享：共享要求先在共同祖先声明
	x := root.Extend("x")
	y := root.Extend("y")
	x.IsolateLabel("svc/extra", "s")
	y.IsolateLabel("svc/extra", "s")
	fmt.Printf("[上下文] 兄弟各自 IsolateLabel 同 label 同一槽 = %v ← 边界\n",
		x.SlotOf("svc/extra") == y.SlotOf("svc/extra"))

	// 配置层：root-first 合并顺序
	root.Intercept("net", "layer-root")
	a.Intercept("net", "layer-a")
	fmt.Printf("[上下文] Config(net) root-first = %v\n", a.Config("net"))

	// 私有槽优先级：子私有槽遮蔽祖先的共享槽
	a.Isolate("svc/cache")
	a.SlotOf("svc/cache").Value = "private"
	fmt.Printf("[上下文] 私有槽遮蔽共享槽：a=%v，b=%v\n",
		a.SlotOf("svc/cache").Value, b.SlotOf("svc/cache").Value)
}

func main() {
	section("0. 主进程启动")
	h := &host{level: 7}
	root := GoTenon.New("game")
	root.SetInfo("version", "0.1.0")
	root.SetInfo("env", "dev")
	root.Isolate("game/api")
	root.SlotOf("game/api").Value = Game(h)
	root.Intercept("storage", "v1:")

	loader := &gameLoader{}
	m := GoTenon.NewManager(root)
	h.m = m
	m.Loader = loader
	m.Logger = GoTenon.LoggerFunc(logf)

	section("1. 上下文能力")
	demoContext(root)

	section("2. 注册与依赖成环")
	storage := &storagePlugin{}
	plugins := []GoTenon.PluginInfo{
		storage, &renderPlugin{}, &hudPlugin{},
		&aiPlugin{}, &slowPlugin{}, &orphanPlugin{},
	}
	for _, p := range plugins {
		if _, err := m.Register(p, nil); err != nil {
			panic(err)
		}
	}
	// quest 首次装载故意失败：Effect 半成品回滚，依赖者 ai 保持 PENDING
	if _, err := m.Register(&questPlugin{}, questConfig{failInit: true}); err != nil {
		panic(err)
	}
	// 重复注册：按错误码拒绝
	if _, err := m.Register(&storagePlugin{}, nil); err != nil {
		fmt.Printf("[宿主] 重复注册被拒绝: %v（IsCode(DUPLICATE)=%v）\n", err, GoTenon.IsCode(err, GoTenon.ErrDuplicate))
	}
	// 成环：cycleA ↔ cycleB，注册期拒绝并回滚
	if _, err := m.Register(&cycleA{}, nil); err != nil {
		panic(err)
	}
	if _, err := m.Register(&cycleB{}, nil); err != nil {
		fmt.Printf("[宿主] 成环被拒绝: %v（IsCode(INVALID_PLUGIN)=%v）\n", err, GoTenon.IsCode(err, GoTenon.ErrInvalidPlugin))
	}
	if _, ok := m.Get("cycleB"); !ok {
		fmt.Println("[宿主] cycleB 已回滚，未留在插件表")
	}

	section("3. 启用与收敛")
	for _, name := range []string{"storage", "render", "hud", "quest", "ai", "orphan", "slow"} {
		err := m.Enable(name)
		fmt.Printf("[宿主] Enable(%s) → err=%v\n", name, err)
	}
	time.Sleep(300 * time.Millisecond) // 等 slow 的超时协程跑完
	dumpStates(m)

	section("4. 修复 quest、补注册依赖，观察自动装配")
	if err := m.Update("quest", questConfig{failInit: false}); err != nil {
		panic(err)
	}
	fmt.Println("[宿主] Update(quest) 成功；ai 的依赖恢复后自动装载")
	if _, err := m.Register(&latePlugin{}, nil); err != nil {
		panic(err)
	}
	if err := m.Enable("late"); err != nil {
		panic(err)
	}
	dumpStates(m)

	section("5. 运行：消息、管道与游戏循环")
	fmt.Println("[宿主] Available(game/api) =", m.Available("game/api"))
	fmt.Println("[宿主] Available(svc/quest) =", m.Available("svc/quest"))
	fmt.Println("[宿主] Available(svc/storage) =", m.Available("svc/storage"))

	if v, ok := h.Service("render", "game/render"); ok {
		v.(Renderer).Draw("宿主直连")
	}
	_ = m.Send(GoTenon.Message{Name: "render", Data: context.Background()})
	_ = m.Send(GoTenon.Message{Name: "ai", Data: context.Background()})

	processor := GoTenon.NewMessageProcessor(m)
	processor.OnError = func(msg GoTenon.Message, err error) {
		fmt.Printf("[宿主] 管道错误 → %s: %v\n", msg.Name, err)
	}
	pipe, cancel := processor.RegisterPipe(4)
	pipe <- GoTenon.Message{Name: "hud", Data: context.Background()}
	pipe <- GoTenon.Message{Name: "ghost", Data: context.Background()}
	time.Sleep(50 * time.Millisecond)
	cancel()

	section("6. 动态治理")
	// 6.1 禁用有已装载依赖者的插件：启用位关闭，但装载保持
	if err := m.Disable("render"); err != nil {
		panic(err)
	}
	rt, _ := m.Get("render")
	fmt.Printf("[宿主] Disable(render)：State=%s Loaded=%v（被 hud/ai 依赖，转 Kept 保留装载）← 边界\n",
		rt.State, rt.Loaded())
	fmt.Printf("[宿主] Send(render) → %v（State=Kept 但仍可收消息）← 边界\n",
		m.Send(GoTenon.Message{Name: "render", Data: context.Background()}))

	// 6.2 Update 无视依赖者强制卸载并重载：依赖者持有过期引用
	if err := m.Update("render", nil); err != nil {
		panic(err)
	}
	_ = m.Send(GoTenon.Message{Name: "hud", Data: context.Background()})
	if err := m.Enable("render"); err != nil {
		panic(err)
	}

	// 6.3 禁用 hud：副作用清空 HUD，插件卸载
	if err := m.Disable("hud"); err != nil {
		panic(err)
	}
	fmt.Printf("[宿主] Disable(hud) 后 HUD=%q\n", h.HUD())
	fmt.Printf("[宿主] Send(hud) → %v（未装载）\n",
		m.Send(GoTenon.Message{Name: "hud", Data: context.Background()}))

	// 6.4 删除被依赖的插件：拒绝。注意 Delete 在检查依赖之前就已清掉启用位，
	// 所以 render 此后处于 Kept（未启用但被依赖保留）状态（第 7 节会看到它的连带卸载）。
	fmt.Printf("[宿主] Delete(render) → %v（仍有已装载依赖者）\n", m.Delete("render"))
	rt, _ = m.Get("render")
	fmt.Printf("[宿主] Delete 被拒后 render：State=%s Enable=%v Loaded=%v ← 边界\n",
		rt.State, rt.Enable, rt.Loaded())

	// 6.5 重新启用 hud：重新解析依赖，恢复运行
	if err := m.Enable("hud"); err != nil {
		panic(err)
	}
	fmt.Printf("[宿主] Enable(hud) 后 HUD=%q\n", h.HUD())

	section("7. 卸载、级联与回收顺序")
	_ = m.Disable("ai") // 停止思考协程
	_ = m.Disable("hud")
	_ = m.Disable("quest") // Effect 组逆序回收
	_ = m.Disable("storage")

	_ = m.Disable("late") // 仍被 orphan 依赖，保持装载
	rt, _ = m.Get("late")
	fmt.Printf("[宿主] Disable(late)：State=%s Loaded=%v（被 orphan 依赖）\n", rt.State, rt.Loaded())
	_ = m.Disable("orphan") // 依赖者先走，late 引用归零自动卸载
	rt, _ = m.Get("late")
	fmt.Printf("[宿主] Disable(orphan) 后 late：State=%s Loaded=%v（引用归零自动卸载）\n", rt.State, rt.Loaded())

	fmt.Printf("[宿主] Delete(slow) → %v\n", m.Delete("slow"))
	fmt.Printf("[宿主] Delete(render) → %v\n", m.Delete("render"))
	if _, ok := m.Get("render"); !ok {
		fmt.Println("[宿主] render 已从插件表移除")
	}
	fmt.Printf("[宿主] storage 卸载后上下文槽位已清空 = %v\n", storage.ctx.SlotOf("svc/storage") == nil)

	section("8. 并行装载验证（3 个互不依赖的 worker 扇入 batch）")
	for _, id := range []string{"a", "b", "c"} {
		if _, err := m.Register(&workerPlugin{id: id, delay: 120 * time.Millisecond}, nil); err != nil {
			panic(err)
		}
	}
	if _, err := m.Register(&batchPlugin{}, nil); err != nil {
		panic(err)
	}

	// 8.1 并行：只启用扇入插件 batch，worker 闭包按波次并行装载
	loader.concurrency = 3
	start := time.Now()
	if err := m.Enable("batch"); err != nil {
		panic(err)
	}
	parallelCost := time.Since(start)
	fmt.Printf("[宿主] 并行装载（limit=3）总耗时 %s；串行理论下限 %s\n",
		parallelCost.Round(time.Millisecond), (3 * 120 * time.Millisecond))

	// 8.2 卸载后切到串行：同样的 3 个 worker 逐个装载
	if err := m.Disable("batch"); err != nil {
		panic(err)
	}
	loader.concurrency = 1
	start = time.Now()
	if err := m.Enable("batch"); err != nil {
		panic(err)
	}
	serialCost := time.Since(start)
	fmt.Printf("[宿主] 串行装载（limit=1）总耗时 %s\n", serialCost.Round(time.Millisecond))
	fmt.Printf("[宿主] 对照结论：并行 %s vs 串行 %s（3 个 120ms 的 worker）\n",
		parallelCost.Round(time.Millisecond), serialCost.Round(time.Millisecond))
}
