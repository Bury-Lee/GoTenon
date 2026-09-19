// plugin.go —— 示例插件集：每个插件针对 GoTenon 的一项或多项能力，
// 同时故意覆盖失败、超时、成环、缺依赖等边界路径。
package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"GoTenon"
)

// ---------- 宿主契约 ----------

// Game 是游戏主进程暴露给插件的宿主 API。
type Game interface {
	Log(msg string)
	PlayerLevel() int
	SetHUD(text string)
	HUD() string
	// Service 是跨插件服务解析的逃生口：v0.1 中插件上下文互为兄弟，
	// 无法直接解析对方 Isolate 出来的槽位，只能经宿主中转。
	Service(plugin, slot string) (any, bool)
}

// Renderer 是 render 插件提供的能力。
type Renderer interface {
	Draw(text string)
	Closed() bool
}

// basePlugin 提供 PluginInfo 的默认实现，插件只覆写关心的钩子。
type basePlugin struct{}

func (basePlugin) Desc() map[string]string                  { return nil }
func (basePlugin) Inject() []string                         { return nil }
func (basePlugin) Config() map[string]string                { return nil }
func (basePlugin) Register() error                          { return nil }
func (basePlugin) Apply(*GoTenon.GoTenonContext, any) error { return nil }
func (basePlugin) Start() error                             { return nil }
func (basePlugin) Run() error                               { return nil }
func (basePlugin) End() error                               { return nil }
func (basePlugin) DealWithMessage(context.Context) error    { return nil }

// ---------- storage：槽位 + 副作用 + 配置层 ----------

type storagePlugin struct {
	basePlugin
	mu     sync.Mutex // closed 在卸载协程写、消息协程读
	ctx    *GoTenon.GoTenonContext
	prefix string
	closed bool
}

func (p *storagePlugin) Name() string { return "storage" }
func (p *storagePlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/storage"}
}
func (p *storagePlugin) Config() map[string]string { return map[string]string{"kind": "kv"} }

func (p *storagePlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	p.ctx = ctx
	// 配置层：宿主 Intercept 下发的层序列，root-first 依次合并
	for _, layer := range ctx.Config("storage") {
		if s, ok := layer.(string); ok {
			p.prefix = s
		}
	}
	ctx.Isolate("svc/storage")
	ctx.SlotOf("svc/storage").Value = p
	ctx.Register(func() error {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
		fmt.Println("[storage] disposer：落盘并关闭")
		return nil
	})
	return nil
}

func (p *storagePlugin) Put(k, v string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		fmt.Println("[storage] 已关闭，拒绝写入 ← 边界")
		return
	}
	fmt.Printf("[storage] 写入 %s%s=%s\n", p.prefix, k, v)
}

// ---------- render：宿主 API + 槽位 + 消息 ----------

type renderPlugin struct {
	basePlugin
	host Game
	r    *consoleRenderer
}

func (p *renderPlugin) Name() string { return "render" }
func (p *renderPlugin) Desc() map[string]string {
	return map[string]string{"provides": "game/render"}
}

func (p *renderPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	slot := ctx.SlotOf("game/api")
	if slot == nil {
		return errors.New("render: 宿主 API game/api 不可用")
	}
	p.host = slot.Value.(Game)
	p.r = &consoleRenderer{host: p.host}
	ctx.Isolate("game/render")
	ctx.SlotOf("game/render").Value = p.r
	ctx.Register(func() error {
		p.r.closed = true
		fmt.Println("[render] disposer：渲染器下线")
		return nil
	})
	return nil
}

func (p *renderPlugin) DealWithMessage(ctx context.Context) error {
	p.r.Draw("游戏帧")
	return nil
}

type consoleRenderer struct {
	host   Game
	closed bool
}

func (r *consoleRenderer) Draw(text string) {
	if r.closed {
		fmt.Println("[render] 过期引用：该渲染器已随插件卸载 ← 边界")
		return
	}
	suffix := ""
	if hud := r.host.HUD(); hud != "" {
		suffix = " | " + hud
	}
	r.host.Log(fmt.Sprintf("绘制 %q (level=%d)%s", text, r.host.PlayerLevel(), suffix))
}

func (r *consoleRenderer) Closed() bool { return r.closed }

// ---------- hud：硬依赖 + 跨插件服务 + 卸载清理 ----------

type hudPlugin struct {
	basePlugin
	host Game
	r    Renderer
}

func (p *hudPlugin) Name() string { return "hud" }
func (p *hudPlugin) Desc() map[string]string {
	return map[string]string{"inject": "render"}
}
func (p *hudPlugin) Inject() []string { return []string{"render"} }

func (p *hudPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	slot := ctx.SlotOf("game/api")
	if slot == nil {
		return errors.New("hud: 宿主 API game/api 不可用")
	}
	p.host = slot.Value.(Game)
	v, ok := p.host.Service("render", "game/render")
	if !ok {
		return errors.New("hud: render 服务不可用")
	}
	p.r = v.(Renderer)
	p.r.Draw("HUD 初始化")
	p.host.SetHUD(fmt.Sprintf("HP %d", p.host.PlayerLevel()))
	ctx.Register(func() error {
		p.host.SetHUD("")
		fmt.Println("[hud] disposer：HUD 清空")
		return nil
	})
	return nil
}

func (p *hudPlugin) DealWithMessage(ctx context.Context) error {
	p.r.Draw("HUD 刷新")
	return nil
}

// ---------- quest：Effect 事务组（首次初始化失败，Update 后恢复） ----------

type questConfig struct {
	failInit bool
}

type questService struct{ name string }

type questPlugin struct {
	basePlugin
}

func (p *questPlugin) Name() string { return "quest" }
func (p *questPlugin) Desc() map[string]string {
	return map[string]string{"provides": "svc/quest"}
}
func (p *questPlugin) Inject() []string { return []string{"storage"} }

func (p *questPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	c, _ := cfg.(questConfig)
	// Effect：body 期间注册的副作用是一个可整体回收的组；
	// body 失败时半成品立即逆序回收，成功则整体登记到父 scope。
	_, err := ctx.Effect(func() error {
		ctx.Register(func() error {
			fmt.Println("[quest] disposer：任务索引回收")
			return nil
		})
		if c.failInit {
			return errors.New("任务数据损坏")
		}
		_, err := ctx.Effect(func() error {
			ctx.Register(func() error {
				fmt.Println("[quest] disposer：任务计时器回收")
				return nil
			})
			return nil
		}, "quest-timers")
		return err
	}, "quest-init")
	if err != nil {
		return fmt.Errorf("quest: %w", err)
	}
	ctx.Isolate("svc/quest")
	ctx.SlotOf("svc/quest").Value = &questService{name: "主线任务"}
	return nil
}

// ---------- ai：多依赖 + 常驻协程 + 消息处理 ----------

type aiPlugin struct {
	basePlugin
	host Game
	stop chan struct{}
	mu   sync.Mutex
	tick int
}

func (p *aiPlugin) Name() string { return "ai" }
func (p *aiPlugin) Desc() map[string]string {
	return map[string]string{"inject": "quest, render"}
}
func (p *aiPlugin) Inject() []string { return []string{"quest", "render"} }

func (p *aiPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	slot := ctx.SlotOf("game/api")
	if slot == nil {
		return errors.New("ai: 宿主 API game/api 不可用")
	}
	p.host = slot.Value.(Game)
	p.stop = make(chan struct{})
	// 常驻协程的停止信号登记为副作用：卸载时自动回收，不留死协程
	ctx.Register(func() error {
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
	if v, ok := ctx.GetInfo("version"); ok {
		fmt.Printf("[ai] 沿祖先链读到宿主版本: %v\n", v)
	}
	return nil
}

func (p *aiPlugin) Run() error {
	go func() {
		ticker := time.NewTicker(150 * time.Millisecond)
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

func (p *aiPlugin) DealWithMessage(ctx context.Context) error {
	p.host.Log("ai 收到行动指令")
	if v, ok := p.host.Service("quest", "svc/quest"); ok {
		p.host.Log("ai 读取任务: " + v.(*questService).name)
	}
	if v, ok := p.host.Service("storage", "svc/storage"); ok {
		v.(*storagePlugin).Put("last_action", "explore")
	}
	return nil
}

// ---------- slow：装载超时（Apply 故意超过 Loader.Timeout） ----------

type slowPlugin struct{ basePlugin }

func (p *slowPlugin) Name() string { return "slow" }
func (p *slowPlugin) Desc() map[string]string {
	return map[string]string{"note": "装载超时演示"}
}

func (p *slowPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	ctx.SetInfo("slow/state", "loading")
	time.Sleep(250 * time.Millisecond)
	if _, ok := ctx.GetInfo("slow/state"); !ok {
		fmt.Println("[slow] Apply 超时后自身上下文已被清空，但协程仍在执行 ← 边界")
	}
	return nil
}

func (p *slowPlugin) Start() error {
	fmt.Println("[slow] Start 在超时回滚之后仍被调用 ← 边界")
	return nil
}

func (p *slowPlugin) Run() error {
	fmt.Println("[slow] Run 在超时回滚之后仍被调用 ← 边界")
	return nil
}

// ---------- orphan / late：缺失依赖 → PENDING，依赖补注册后自动装载 ----------

type orphanPlugin struct{ basePlugin }

func (p *orphanPlugin) Name() string { return "orphan" }
func (p *orphanPlugin) Desc() map[string]string {
	return map[string]string{"inject": "late"}
}
func (p *orphanPlugin) Inject() []string { return []string{"late"} }

func (p *orphanPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	fmt.Println("[orphan] 依赖就绪，从 PENDING 自动装载")
	return nil
}

type latePlugin struct{ basePlugin }

func (p *latePlugin) Name() string { return "late" }

func (p *latePlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	fmt.Println("[late] 装载完成，开始为依赖者提供服务")
	return nil
}

// ---------- worker-a/b/c + batch：并行装载演示（扇入依赖） ----------

type workerPlugin struct {
	basePlugin
	id    string
	delay time.Duration
}

func (p *workerPlugin) Name() string { return "worker-" + p.id }

func (p *workerPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	start := time.Now()
	fmt.Printf("[%s] Apply 开始 @%s\n", p.Name(), start.Format("15:04:05.000"))
	time.Sleep(p.delay)
	fmt.Printf("[%s] Apply 结束 @%s（耗时 %s）\n",
		p.Name(), time.Now().Format("15:04:05.000"), time.Since(start).Round(time.Millisecond))
	return nil
}

// batchPlugin 只依赖三个互不依赖的 worker：启用它即可触发一个并行波次。
type batchPlugin struct{ basePlugin }

func (p *batchPlugin) Name() string     { return "batch" }
func (p *batchPlugin) Inject() []string { return []string{"worker-a", "worker-b", "worker-c"} }

func (p *batchPlugin) Apply(ctx *GoTenon.GoTenonContext, cfg any) error {
	fmt.Println("[batch] 依赖全部就绪，装载完成")
	return nil
}

// ---------- cycleA / cycleB：依赖成环，注册期拒绝 ----------

type cycleA struct{ basePlugin }

func (p *cycleA) Name() string     { return "cycleA" }
func (p *cycleA) Inject() []string { return []string{"cycleB"} }

type cycleB struct{ basePlugin }

func (p *cycleB) Name() string     { return "cycleB" }
func (p *cycleB) Inject() []string { return []string{"cycleA"} }
