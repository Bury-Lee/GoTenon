// plugin.go —— 组件集：演示内核四件套(状态 / 能力 / 消息 / 信号)与可逆生命周期。
//
// 约定：组件在装载期(Apply/Start/Run)只经 ctx 槽位拿到宿主 API，
// 不回调 Manager(装载期消费者持有 Manager 锁，回调会重入死锁)。
package main

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"GoTenon"
)

// ---------- 宿主契约 ----------

// Game 是宿主暴露给组件的 API。宿主自持服务表(Provide/Service)，
// 组件借此在装载期互通，而不必回调 Manager。
type Game interface {
	Log(msg string)
	PlayerLevel() int
	SetHUD(text string)
	HUD() string
	Provide(name string, v any)
	Service(name string) (any, bool)
}

// cap 构造 MCP 风格的能力描述(推荐但非强制)。
func cap(desc string, props ...string) map[string]any {
	properties := map[string]any{}
	required := make([]string, 0, len(props))
	for _, p := range props {
		properties[p] = map[string]any{"type": "string"}
		required = append(required, p)
	}
	return map[string]any{
		"description": desc,
		"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required},
	}
}

func statusOf(kv map[string]any) *map[string]any { return &kv }

// logIndex 是组件对索引通知消息(TypeIndex)的统一处理。
func logIndex(p string, m GoTenon.Message) {
	if ev, ok := m.Data.(GoTenon.IndexEvent); ok {
		fmt.Printf("[%s] 索引事件: %-6s %s\n", p, ev.Kind, ev.Plugin)
	}
}

// base 提供 PluginInfo 默认实现，组件只覆写关心的钩子。
type base struct{}

func (base) Desc() map[string]string                  { return nil }
func (base) Inject() []string                         { return nil }
func (base) Status() *map[string]any                  { return nil }
func (base) Register() error                          { return nil }
func (base) Apply(*GoTenon.GoTenonContext, any) error { return nil }
func (base) Start() error                             { return nil }
func (base) Run() error                               { return nil }
func (base) DealWithMessage(GoTenon.Message) error    { return nil }
func (base) End() error                               { return nil }

// ---------- storage：槽位 + 副作用 + 能力 ----------

type storage struct {
	base
	mu   sync.Mutex
	host Game
	kv   map[string]string
}

func (p *storage) Name() string            { return "storage" }
func (p *storage) Desc() map[string]string { return map[string]string{"provides": "svc/storage"} }
func (p *storage) Status() *map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return statusOf(map[string]any{"state": "ready", "keys": len(p.kv)})
}

func (p *storage) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	slot := ctx.SlotOf("game/api")
	if slot == nil {
		return errors.New("storage: 宿主 API game/api 不可用")
	}
	p.host = slot.Value.(Game)
	p.kv = map[string]string{}
	ctx.Isolate("svc/storage")
	ctx.SlotOf("svc/storage").Value = p
	p.host.Provide("svc/storage", p)
	ctx.Register(func() error { // 卸载时逆序回收
		p.host.Log("[storage] disposer：落盘并关闭")
		return nil
	})
	return nil
}

func (p *storage) Put(k, v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.kv[k] = v
	fmt.Printf("[storage] 写入 %s=%s\n", k, v)
}

func (p *storage) Get(k string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.kv[k]
	return v, ok
}

func (p *storage) DealWithMessage(m GoTenon.Message) error {
	switch m.Type {
	case GoTenon.TypeIndex:
		logIndex("storage", m)
	case GoTenon.TypeRaw:
		if args, ok := m.Data.(map[string]any); ok {
			k, _ := args["key"].(string)
			v, _ := args["value"].(string)
			p.Put(k, v)
		}
	}
	return nil
}

func (p *storage) Function() map[string]any {
	return map[string]any{
		"storage.put": cap("写入键值", "key", "value"),
		"storage.get": cap("读取键值", "key"),
	}
}
func (p *storage) ExecuteFunction(any) {}

// ---------- render：能力 + 类型化消息 ----------

type render struct {
	base
	host Game
}

func (p *render) Name() string            { return "render" }
func (p *render) Desc() map[string]string { return map[string]string{"provides": "game/render"} }
func (p *render) Status() *map[string]any {
	return statusOf(map[string]any{"state": "ready", "fps": 60})
}

func (p *render) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	slot := ctx.SlotOf("game/api")
	if slot == nil {
		return errors.New("render: 宿主 API game/api 不可用")
	}
	p.host = slot.Value.(Game)
	ctx.Isolate("game/render")
	ctx.SlotOf("game/render").Value = p
	p.host.Provide("game/render", p)
	ctx.Register(func() error {
		p.host.Log("[render] disposer：渲染器下线")
		return nil
	})
	return nil
}

func (p *render) Draw(text string) {
	p.host.Log(fmt.Sprintf("绘制 %q (level=%d)", text, p.host.PlayerLevel()))
}

func (p *render) DealWithMessage(m GoTenon.Message) error {
	switch m.Type {
	case GoTenon.TypeIndex:
		logIndex("render", m)
	case GoTenon.TypeRaw:
		if s, ok := m.Data.(string); ok {
			p.Draw(s)
		} else {
			p.Draw("游戏帧")
		}
	}
	return nil
}

func (p *render) Function() map[string]any {
	return map[string]any{"render.draw": cap("绘制文本", "text")}
}
func (p *render) ExecuteFunction(any) {}

// ---------- hud：硬依赖 + 卸载清理 ----------

type hud struct {
	base
	host Game
	r    *render
}

func (p *hud) Name() string            { return "hud" }
func (p *hud) Desc() map[string]string { return map[string]string{"inject": "render"} }
func (p *hud) Inject() []string        { return []string{"render"} }
func (p *hud) Status() *map[string]any {
	if p.host == nil {
		return statusOf(map[string]any{"state": "pending"})
	}
	return statusOf(map[string]any{"state": "ready", "hud": p.host.HUD()})
}

func (p *hud) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	slot := ctx.SlotOf("game/api")
	if slot == nil {
		return errors.New("hud: 宿主 API game/api 不可用")
	}
	p.host = slot.Value.(Game)
	v, ok := p.host.Service("game/render")
	if !ok {
		return errors.New("hud: render 服务不可用")
	}
	p.r = v.(*render)
	p.r.Draw("HUD 初始化")
	p.host.SetHUD(fmt.Sprintf("HP %d", p.host.PlayerLevel()))
	ctx.Register(func() error {
		p.host.SetHUD("")
		p.host.Log("[hud] disposer：HUD 清空")
		return nil
	})
	return nil
}

func (p *hud) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("hud", m)
		return nil
	}
	p.r.Draw("HUD 刷新")
	return nil
}

// ---------- quest：Effect 事务组(首次失败，Update 后恢复) ----------

type questConfig struct{ Fail bool }

type quest struct{ base }

func (p *quest) Name() string            { return "quest" }
func (p *quest) Desc() map[string]string { return map[string]string{"provides": "svc/quest"} }
func (p *quest) Inject() []string        { return []string{"storage"} }
func (p *quest) Status() *map[string]any {
	return statusOf(map[string]any{"state": "ready", "quests": 1})
}

func (p *quest) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(questConfig)
	_, err := ctx.Effect(func() error {
		ctx.Register(func() error { fmt.Println("[quest] disposer：任务索引回收"); return nil })
		if c.Fail {
			return errors.New("任务数据损坏")
		}
		_, err := ctx.Effect(func() error {
			ctx.Register(func() error { fmt.Println("[quest] disposer：任务计时器回收"); return nil })
			return nil
		}, "quest-timers")
		return err
	}, "quest-init")
	if err != nil {
		return fmt.Errorf("quest: %w", err)
	}
	ctx.Isolate("svc/quest")
	ctx.SlotOf("svc/quest").Value = p
	return nil
}

func (p *quest) Function() map[string]any {
	return map[string]any{"quest.summary": cap("生成任务摘要", "quest_id")}
}
func (p *quest) ExecuteFunction(any) {}

// ---------- ai：多依赖 + 常驻协程 + 消息处理 ----------

type ai struct {
	base
	mu      sync.Mutex
	stop    chan struct{}
	tick    int
	host    Game
	storage *storage
}

func (p *ai) Name() string      { return "ai" }
func (p *ai) Inject() []string  { return []string{"quest", "storage"} }
func (p *ai) Desc() map[string]string {
	return map[string]string{"inject": "quest, storage"}
}
func (p *ai) Status() *map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return statusOf(map[string]any{"state": "running", "tick": p.tick})
}

func (p *ai) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	slot := ctx.SlotOf("game/api")
	if slot == nil {
		return errors.New("ai: 宿主 API game/api 不可用")
	}
	p.host = slot.Value.(Game)
	if v, ok := p.host.Service("svc/storage"); ok {
		p.storage = v.(*storage)
	}
	p.stop = make(chan struct{})
	ctx.Register(func() error { // 常驻协程停止信号登记为副作用
		p.mu.Lock()
		defer p.mu.Unlock()
		select {
		case <-p.stop:
		default:
			close(p.stop)
			fmt.Println("[ai] disposer：思考协程已停止")
		}
		return nil
	})
	return nil
}

func (p *ai) Run() error {
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-ticker.C:
				p.mu.Lock()
				p.tick++
				n := p.tick
				p.mu.Unlock()
				p.host.Log(fmt.Sprintf("ai 思考 #%d", n))
			}
		}
	}()
	return nil
}

func (p *ai) DealWithMessage(m GoTenon.Message) error {
	if m.Type == GoTenon.TypeIndex {
		logIndex("ai", m)
		return nil
	}
	p.host.Log("ai 收到行动指令")
	if p.storage != nil {
		p.storage.Put("last_action", "explore")
	}
	return nil
}

func (p *ai) Function() map[string]any {
	return map[string]any{"ai.think": cap("思考下一步行动")}
}
func (p *ai) ExecuteFunction(any) {}

// ---------- watcher：订阅索引通知(TypeIndex 消息) ----------

type watcher struct {
	base
	mu   sync.Mutex
	seen []string
}

func (p *watcher) Name() string            { return "watcher" }
func (p *watcher) Desc() map[string]string { return map[string]string{"subscribes": "index"} }
func (p *watcher) Status() *map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return statusOf(map[string]any{"state": "ready", "seen": len(p.seen)})
}

func (p *watcher) DealWithMessage(m GoTenon.Message) error {
	if m.Type != GoTenon.TypeIndex {
		return nil
	}
	ev, ok := m.Data.(GoTenon.IndexEvent)
	if !ok {
		return nil
	}
	p.mu.Lock()
	p.seen = append(p.seen, ev.Kind+" "+ev.Plugin)
	p.mu.Unlock()
	fmt.Printf("[watcher] 收到索引通知: %-6s %s\n", ev.Kind, ev.Plugin)
	return nil
}

// ---------- orphan / late：缺失依赖 → PENDING，补注册后自动装载 ----------

type orphan struct{ base }

func (p *orphan) Name() string            { return "orphan" }
func (p *orphan) Desc() map[string]string { return map[string]string{"inject": "late"} }
func (p *orphan) Inject() []string        { return []string{"late"} }
func (p *orphan) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	fmt.Println("[orphan] 依赖就绪，从 PENDING 自动装载")
	return nil
}

type late struct{ base }

func (p *late) Name() string { return "late" }
func (p *late) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	fmt.Println("[late] 装载完成，开始为依赖者提供服务")
	return nil
}

// ---------- cycleA / cycleB：依赖成环，注册期拒绝 ----------

type cycleA struct{ base }

func (p *cycleA) Name() string     { return "cycleA" }
func (p *cycleA) Inject() []string { return []string{"cycleB"} }

type cycleB struct{ base }

func (p *cycleB) Name() string     { return "cycleB" }
func (p *cycleB) Inject() []string { return []string{"cycleA"} }

// ---------- worker / batch：并行装载演示 ----------

type worker struct {
	base
	id    string
	delay time.Duration
}

func (p *worker) Name() string { return "worker-" + p.id }
func (p *worker) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	start := time.Now()
	fmt.Printf("[%s] Apply 开始 @%s\n", p.Name(), start.Format("15:04:05.000"))
	time.Sleep(p.delay)
	fmt.Printf("[%s] Apply 结束(耗时 %s)\n", p.Name(), time.Since(start).Round(time.Millisecond))
	return nil
}

type batch struct{ base }

func (p *batch) Name() string     { return "batch" }
func (p *batch) Inject() []string { return []string{"worker-a", "worker-b", "worker-c"} }
func (p *batch) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	fmt.Println("[batch] 依赖全部就绪，装载完成")
	return nil
}
