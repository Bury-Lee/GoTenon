package GoTenon

import "sync"

// Manager 是中央管理器：
// 插件钩子由工作协程执行，状态由调度者统一写回。
// 公开方法由 mu 串行化，保证并发 Register/Enable/Disable/Update 安全；
// 索引变更通知在锁外派发(flushIndexEvents)，避免订阅者回调时自锁。
type Manager struct {
	PluginTable PluginTable
	Loader      Loader
	Logger      Logger // 框架日志出口；nil 静默
	root        *GoTenonContext
	rt          map[string]*PluginRuntime
	mu          sync.Mutex // 串行化公开操作(生命周期锁)

	// router 是消息路由表:名字 → 已装载插件
	router *router

	// Processor 是消息/信号的默认系统实现；nil 时用内核默认实现。
	// 约定:启动后不可再改写(Dispatcher 读取时不加锁)。
	Processor MessageProcesser
	// Signals 是信号注册表；nil 时用内置信号表。
	Signals *SignalTable
	// index 是内核私有索引表：能力与状态的高速索引。
	index *kernelIndex

	// 并行装载上限；<=0 默认 4（Loader 实现 ConcurrencyLoader 时可覆盖）
	Concurrency int
}

// NewManager 创建中央管理器；root 为 nil 时新建根上下文。
func NewManager(root *GoTenonContext) *Manager {
	if root == nil {
		root = New("root")
	}
	m := &Manager{
		PluginTable: PluginTable{Info: make(map[string]PluginInfo)},
		root:        root,
		rt:          make(map[string]*PluginRuntime),
		router:      newRouter(),
		Signals:     DefaultSignalTable(),
		index:       newKernelIndex(),
	}
	m.Processor = &DefaultMessageProcesser{Manager: m}
	return m
}

// lookup 按插件名取运行时；未注册返回 NOT_PROVIDED。
func (m *Manager) lookup(name string) (*PluginRuntime, error) {
	rt := m.rt[name]
	if rt == nil {
		return nil, newErr(ErrNotProvided, "manager: plugin %q not registered", name)
	}
	return rt, nil
}

// TODO:以后减少这样的没必要的检查,出现问题直接报错就是了
// ensure 惰性初始化零值 Manager（enter.go 的 var PluginManager Manager）。
func (m *Manager) ensure() {
	if m.PluginTable.Info == nil {
		m.PluginTable.Info = make(map[string]PluginInfo)
	}
	if m.rt == nil {
		m.rt = make(map[string]*PluginRuntime)
	}
	if m.router == nil {
		m.router = newRouter()
	}
	if m.root == nil {
		m.root = New("root")
	}
	if m.Signals == nil {
		m.Signals = DefaultSignalTable()
	}
	if m.index == nil {
		m.index = newKernelIndex()
	}
	if m.Processor == nil {
		m.Processor = &DefaultMessageProcesser{Manager: m}
	}
}

// Register 登记插件：查重 → 插件自身的 Register 钩子 → 写入插件表 → 依赖成环检测 → Loader 扩展钩子。任一步失败都回滚
func (m *Manager) Register(p PluginInfo, cfg any) (*PluginRuntime, error) {
	m.mu.Lock()
	rt, err := m.registerLocked(p, cfg)
	m.mu.Unlock()
	m.flushIndexEvents()
	return rt, err
}

// registerLocked 在 m.mu 持有下执行注册。
func (m *Manager) registerLocked(p PluginInfo, cfg any) (*PluginRuntime, error) {
	m.ensure()
	if p == nil {
		return nil, newErr(ErrInvalidPlugin, "manager: nil plugin")
	}
	name := p.Name()
	if name == "" {
		return nil, newErr(ErrInvalidPlugin, "manager: plugin without name")
	}
	if _, exists := m.PluginTable.Info[name]; exists {
		return nil, newErr(ErrDuplicate, "manager: plugin %q already registered", name)
	}
	if err := p.Register(); err != nil {
		return nil, err
	}
	rt := &PluginRuntime{Plugin: p, Config: cfg}
	m.PluginTable.Info[name] = p
	m.rt[name] = rt
	m.relink()

	// 落表之后任一步失败都要回滚，插件表保持干净
	rollback := func() {
		delete(m.PluginTable.Info, name)
		delete(m.rt, name)
		m.relink()
		m.refreshIndexLocked(name)
	}
	if err := m.checkCycle(rt, nil); err != nil {
		rollback()
		return nil, err
	}
	if m.Loader != nil {
		if err := m.Loader.IntoRegister(name); err != nil {
			rollback()
			return nil, err
		}
	}
	m.refreshIndexLocked(name)
	m.logf(LevelDebug, "plugin %q registered", name)
	return rt, nil
}

// Delete 删除插件：卸载自身与失去引用的依赖，从插件表移除，并通知 Loader。
// 仍有已装载的依赖者时拒绝（先禁用它们）。
func (m *Manager) Delete(name string) error {
	m.mu.Lock()
	err := m.deleteLocked(name)
	m.mu.Unlock()
	m.flushIndexEvents()
	return err
}

func (m *Manager) deleteLocked(name string) error {
	rt, err := m.lookup(name)
	if err != nil {
		return err
	}
	rt.Enable = false
	if err := m.converge(); err != nil {
		return err
	}
	rt.settle()
	if rt.Loaded() {
		return newErr(ErrInvalidPlugin, "manager: plugin %q still has loaded dependents", name)
	}
	if m.Loader != nil {
		if err := m.Loader.Delete(name); err != nil {
			return err
		}
	}
	delete(m.PluginTable.Info, name)
	delete(m.rt, name)
	m.relink()
	m.routerRemove(name)
	m.refreshIndexLocked(name)
	if m.index != nil {
		m.index.removeSubscriber(name)
	}
	m.logf(LevelDebug, "plugin %q deleted", name)
	return nil
}

// Enable 启用插件：置启用标志并通知 Loader，随后收敛——依赖闭包自动装载（依赖先行），未注册的依赖使插件保持 PENDING而不报错。
func (m *Manager) Enable(name string) error {
	m.mu.Lock()
	err := m.enableLocked(name)
	m.mu.Unlock()
	m.flushIndexEvents()
	return err
}

func (m *Manager) enableLocked(name string) error {
	rt, err := m.lookup(name)
	if err != nil {
		return err
	}
	if err := m.checkCycle(rt, nil); err != nil {
		return err
	}
	rt.Enable = true
	rt.Err = nil
	if m.Loader != nil {
		if err := m.Loader.Enable(name); err != nil {
			rt.Enable = false
			return err
		}
	}
	err = m.converge()
	rt.settle()
	m.refreshIndexLocked(name)
	if err != nil {
		return err
	}
	m.logf(LevelDebug, "plugin %q enabled", name)
	return m.hook(name)
}

// Disable 禁用插件：清启用标志并通知 Loader，随后收敛——没有依赖者时插件卸载，其依赖因引用归零自动卸载，上下文一并清理；仍被启用者依赖时保持装载。
func (m *Manager) Disable(name string) error {
	m.mu.Lock()
	err := m.disableLocked(name)
	m.mu.Unlock()
	m.flushIndexEvents()
	return err
}

func (m *Manager) disableLocked(name string) error {
	rt, err := m.lookup(name)
	if err != nil {
		return err
	}
	rt.Enable = false
	if m.Loader != nil {
		if err := m.Loader.Disable(name); err != nil {
			rt.Enable = true
			return err
		}
	}
	err = m.converge()
	rt.settle()
	m.refreshIndexLocked(name)
	if err != nil {
		return err
	}
	m.logf(LevelDebug, "plugin %q disabled", name)
	return m.hook(name)
}

// hook 操作完成后通知 Loader 的收尾钩子（只传插件名）。
func (m *Manager) hook(name string) error {
	if m.Loader != nil {
		return m.Loader.Hook(name)
	}
	return nil
}

// Get 按插件名（句柄）读取运行时信息。
func (m *Manager) Get(name string) (*PluginRuntime, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt := m.rt[name]
	return rt, rt != nil
}

// Update 按插件名（句柄）更新配置；插件更新自己时传自己的名字。
// 已装载的插件按新配置重载；新配置装载失败时回滚旧配置（事务化）。
func (m *Manager) Update(name string, cfg any) error {
	m.mu.Lock()
	err := m.updateLocked(name, cfg)
	m.mu.Unlock()
	m.flushIndexEvents()
	return err
}

func (m *Manager) updateLocked(name string, cfg any) error {
	rt, err := m.lookup(name)
	if err != nil {
		return err
	}
	oldCfg := rt.Config
	rt.Config = cfg
	rt.Err = nil
	if rt.Loaded() {
		if err := m.unloadBatch([]*PluginRuntime{rt}); err != nil {
			return err
		}
	}
	err = m.converge()
	rt.settle()
	if err != nil {
		// 事务化：新配置失败则回滚旧配置并重载
		rt.Config = oldCfg
		rt.Err = nil
		if e2 := m.converge(); e2 != nil {
			m.logf(LevelWarn, "plugin %q rollback after failed update: %v", name, e2)
		}
		rt.settle()
		m.refreshIndexLocked(name)
		return err
	}
	m.refreshIndexLocked(name)
	return m.hook(name)
}

// Available 服务可用性检查：任一已装载插件的上下文槽位提供了 name（值非空）。
// 只做存在性判断，遍历顺序无关紧要。
func (m *Manager) Available(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, rt := range m.rt {
		if !rt.Loaded() || rt.Context == nil {
			continue
		}
		if slot := rt.Context.SlotOf(name); slot != nil && slot.Value != nil {
			return true
		}
	}
	return false
}

// Send 把消息交给内核消息处理器(默认实现)：系统分支或原封投递目标组件。
// 插件不存在/未装载返回错误；运行期 panic 被隔离为 error。
func (m *Manager) Send(msg Message) error {
	_, err := m.Dispatch(&msg)
	return err
}

// Dispatch 把消息交给 MessageProcesser 处理，返回可选的应答消息。
// 不取生命周期锁:挂在消息面上,故装载期发消息不会死锁(Processor 启动后不可改写)。
func (m *Manager) Dispatch(msg *Message) (*Message, error) {
	p := m.Processor
	if p == nil {
		p = &DefaultMessageProcesser{Manager: m}
	}
	return p.Handle(msg)
}

// Signal 向调度内核投递一个信号。
func (m *Manager) Signal(req SignalRequest) (*Message, error) {
	return m.Dispatch(&Message{Name: "", Type: TypeSignal, Data: &req})
}

// handleSystem 处理发往系统/内核的消息(Name=="")。
func (m *Manager) handleSystem(msg *Message) (*Message, error) {
	switch msg.Type {
	case TypeSignal:
		req, ok := msg.Data.(*SignalRequest)
		if !ok {
			return nil, newErr(ErrMessageTypeMismatch, "system: TypeSignal requires *SignalRequest, got %T", msg.Data)
		}
		return m.applySignal(req)
	default:
		return nil, nil
	}
}

// applySignal 执行信号对应的内核动作。
func (m *Manager) applySignal(req *SignalRequest) (*Message, error) {
	if req == nil {
		return nil, newErr(ErrSignalUnhandled, "system: nil signal request")
	}
	switch req.Signal.Kind {
	case SigReady:
		return nil, nil
	case SigShutdown:
		return nil, m.Disable(req.From)
	case SigRequestStop:
		return nil, m.Disable(req.Target)
	case SigRequestEnable:
		return nil, m.Enable(req.Target)
	case SigRequestReload:
		return nil, m.Update(req.Target, req.Args)
	case SigRequestDelete:
		return nil, m.Delete(req.Target)
	case SigFault, SigPanic:
		return nil, m.markFault(req.From)
	case SigDeclare:
		m.refreshIndex(req.From)
		return nil, nil
	case SigRetract:
		m.mu.Lock()
		if m.index != nil {
			m.index.remove(req.From)
		}
		m.mu.Unlock()
		m.flushIndexEvents()
		return nil, nil
	default:
		return nil, newErr(ErrSignalUnhandled, "system: unhandled signal %q", req.Signal.Name)
	}
}

// markFault 把组件标记为 Failed(故障隔离)，不自动重试。
func (m *Manager) markFault(name string) error {
	m.mu.Lock()
	err := m.markFaultLocked(name)
	m.mu.Unlock()
	m.flushIndexEvents()
	return err
}

func (m *Manager) markFaultLocked(name string) error {
	rt := m.rt[name]
	if rt == nil {
		return newErr(ErrNotProvided, "manager: plugin %q not registered", name)
	}
	rt.State = Failed
	rt.Err = newErr(ErrSignalUnhandled, "signal: fault reported by %q", name)
	m.routerRemove(name) // Failed 不再可投递
	m.refreshIndexLocked(name)
	return nil
}
