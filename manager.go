package GoTenon

// Manager 是中央管理器：
// 插件钩子由工作协程执行，状态由调度者统一写回。
type Manager struct {
	PluginTable PluginTable
	Loader      Loader
	Logger      Logger // 框架日志出口；nil 静默
	root        *GoTenonContext
	rt          map[string]*PluginRuntime

	// 并行装载上限；<=0 默认 4（Loader 实现 ConcurrencyLoader 时可覆盖）
	Concurrency int
}

// NewManager 创建中央管理器；root 为 nil 时新建根上下文。
func NewManager(root *GoTenonContext) *Manager {
	if root == nil {
		root = New("root")
	}
	return &Manager{
		PluginTable: PluginTable{Info: make(map[string]PluginInfo)},
		root:        root,
		rt:          make(map[string]*PluginRuntime),
	}
}

// lookup 按插件名取运行时；未注册返回 NOT_PROVIDED。
func (m *Manager) lookup(name string) (*PluginRuntime, error) {
	rt := m.rt[name]
	if rt == nil {
		return nil, newErr(ErrNotProvided, "manager: plugin %q not registered", name)
	}
	return rt, nil
}

// ensure 惰性初始化零值 Manager（enter.go 的 var PluginManager Manager）。
func (m *Manager) ensure() {
	if m.PluginTable.Info == nil {
		m.PluginTable.Info = make(map[string]PluginInfo)
	}
	if m.rt == nil {
		m.rt = make(map[string]*PluginRuntime)
	}
	if m.root == nil {
		m.root = New("root")
	}
}

// Register 登记插件：查重 → 插件自身的 Register 钩子 → 写入插件表 → 依赖成环检测 → Loader 扩展钩子。任一步失败都回滚
func (m *Manager) Register(p PluginInfo, cfg any) (*PluginRuntime, error) {
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
	m.logf(LevelDebug, "plugin %q registered", name)
	return rt, nil
}

// Delete 删除插件：卸载自身与失去引用的依赖，从插件表移除，并通知 Loader。
// 仍有已装载的依赖者时拒绝（先禁用它们）。
func (m *Manager) Delete(name string) error {
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
	m.logf(LevelDebug, "plugin %q deleted", name)
	return nil
}

// Enable 启用插件：置启用标志并通知 Loader，随后收敛——依赖闭包自动装载（依赖先行），未注册的依赖使插件保持 PENDING而不报错。
func (m *Manager) Enable(name string) error {
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
	if err != nil {
		return err
	}
	m.logf(LevelDebug, "plugin %q enabled", name)
	return m.hook(name)
}

// Disable 禁用插件：清启用标志并通知 Loader，随后收敛——没有依赖者时插件卸载，其依赖因引用归零自动卸载，上下文一并清理；仍被启用者依赖时保持装载。
func (m *Manager) Disable(name string) error {
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
	rt := m.rt[name]
	return rt, rt != nil
}

// Update 按插件名（句柄）更新配置；插件更新自己时传自己的名字。
// 已装载的插件按新配置重载（重载会更换上下文）。
func (m *Manager) Update(name string, cfg any) error {
	rt, err := m.lookup(name)
	if err != nil {
		return err
	}
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
		return err
	}
	return m.hook(name)
}

// Available 服务可用性检查：任一已装载插件的上下文槽位提供了 name（值非空）。
// 只做存在性判断，遍历顺序无关紧要。
func (m *Manager) Available(name string) bool {
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

// Send 把消息发给目标插件：调用它的 DealWithMessage(msg.Data)；
// 插件不存在或未装载返回错误。
func (m *Manager) Send(msg Message) error {
	rt := m.rt[msg.Name]
	if rt == nil {
		return newErr(ErrNotProvided, "manager: plugin %q not registered", msg.Name)
	}
	if !rt.Loaded() {
		return newErr(ErrNotProvided, "manager: plugin %q not loaded", msg.Name)
	}
	return rt.Plugin.DealWithMessage(msg.Data)
}
