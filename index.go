package GoTenon

import "sync"

// kernel.go —— 内核私有索引表：能力与运行状态的高速索引。
//
// 表由消息/生命周期事件驱动更新(注册、上下线、声明/撤回能力)，对内保持原子语义，
// 对外只暴露查询(Discover/Capabilities/Status)与订阅(Subscribe)接口，组件不直接触碰表。
// 锁为「非严格锁」：只保证单次读写的原子性，不追求跨多次操作的强一致。
//
// 订阅不建立函数回调通道：内核在索引变化时，把 IndexEvent 以 Message(Type=TypeIndex)
// 投递给订阅者组件，由订阅者自己的 DealWithMessage 处理。通知在锁外派发。

// CapabilityEntry 是索引中的一条能力记录。
type CapabilityEntry struct {
	Plugin    string         // 提供者组件名
	Name      string         // 能力名(组件内唯一)
	Desc      map[string]any // 能力描述(推荐 MCP schema，格式开放)
	Available bool           // 当前是否可用
}

// IndexEvent 是索引变更事件。
type IndexEvent struct {
	Kind   string // "up" 组件上线可用 / "down" 组件下线 / "update" 能力刷新
	Plugin string
}

// Subscription 描述一个索引订阅：订阅者组件 + 可选过滤谓词。
// 内核在索引变化时，把命中的 IndexEvent 以消息(Type=TypeIndex)投递给 Subscriber。
type Subscription struct {
	Subscriber string                // 订阅者组件名(接收通知消息)
	Filter     func(IndexEvent) bool // nil 表示订阅全部事件
}

// kernelIndex 是内核私有索引表。
type kernelIndex struct {
	mu      sync.RWMutex                         // 非严格锁
	caps    map[string]map[string]map[string]any // plugin -> capName -> desc
	status  map[string]map[string]any            // plugin -> 运行状态
	ready   map[string]bool                      // plugin -> 是否可用
	subs    map[uint64]Subscription              // 订阅者
	nextID  uint64
	pending []IndexEvent // 待派发事件(锁外 flush)
}

func newKernelIndex() *kernelIndex {
	return &kernelIndex{
		caps:   make(map[string]map[string]map[string]any),
		status: make(map[string]map[string]any),
		ready:  make(map[string]bool),
		subs:   make(map[uint64]Subscription),
	}
}

// set 原子写入/更新一个组件的索引项，并排入一条变更事件。
func (x *kernelIndex) set(plugin string, status map[string]any, caps map[string]map[string]any, ready bool) {
	x.mu.Lock()
	prev, existed := x.ready[plugin]
	x.status[plugin] = status
	if caps != nil {
		x.caps[plugin] = caps
	}
	x.ready[plugin] = ready

	kind := "update"
	switch {
	case ready && (!existed || !prev):
		kind = "up"
	case !ready && prev:
		kind = "down"
	}
	x.pending = append(x.pending, IndexEvent{Kind: kind, Plugin: plugin})
	x.mu.Unlock()
}

// remove 原子摘除一个组件，并排入下线事件。
func (x *kernelIndex) remove(plugin string) {
	x.mu.Lock()
	_, existed := x.ready[plugin]
	delete(x.status, plugin)
	delete(x.caps, plugin)
	delete(x.ready, plugin)
	if existed {
		x.pending = append(x.pending, IndexEvent{Kind: "down", Plugin: plugin})
	}
	x.mu.Unlock()
}

// lookupCap 查询某组件的某项能力。
func (x *kernelIndex) lookupCap(plugin, name string) (CapabilityEntry, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	byName, ok := x.caps[plugin]
	if !ok {
		return CapabilityEntry{}, false
	}
	desc, ok := byName[name]
	if !ok {
		return CapabilityEntry{}, false
	}
	return CapabilityEntry{Plugin: plugin, Name: name, Desc: desc, Available: x.ready[plugin]}, true
}

// discover 按谓词发现全部能力。
func (x *kernelIndex) discover(filter func(CapabilityEntry) bool) []CapabilityEntry {
	x.mu.RLock()
	defer x.mu.RUnlock()
	var out []CapabilityEntry
	for plugin, byName := range x.caps {
		for name, desc := range byName {
			e := CapabilityEntry{Plugin: plugin, Name: name, Desc: desc, Available: x.ready[plugin]}
			if filter == nil || filter(e) {
				out = append(out, e)
			}
		}
	}
	return out
}

// statusOf 读取某组件的运行状态副本。
func (x *kernelIndex) statusOf(plugin string) (map[string]any, bool) {
	x.mu.RLock()
	defer x.mu.RUnlock()
	s, ok := x.status[plugin]
	return s, ok
}

// subscribe 登记订阅者，返回订阅号。
func (x *kernelIndex) subscribe(sub Subscription) uint64 {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.nextID++
	id := x.nextID
	x.subs[id] = sub
	return id
}

// unsubscribe 注销订阅者。
func (x *kernelIndex) unsubscribe(id uint64) {
	x.mu.Lock()
	delete(x.subs, id)
	x.mu.Unlock()
}

// removeSubscriber 摘除某组件的全部订阅(组件删除时调用)。
func (x *kernelIndex) removeSubscriber(name string) {
	x.mu.Lock()
	for id, s := range x.subs {
		if s.Subscriber == name {
			delete(x.subs, id)
		}
	}
	x.mu.Unlock()
}

// subscriptions 返回订阅者快照。
func (x *kernelIndex) subscriptions() []Subscription {
	x.mu.RLock()
	defer x.mu.RUnlock()
	out := make([]Subscription, 0, len(x.subs))
	for _, s := range x.subs {
		out = append(out, s)
	}
	return out
}

// drainPending 取走并清空待派发事件。
func (x *kernelIndex) drainPending() []IndexEvent {
	x.mu.Lock()
	evs := x.pending
	x.pending = nil
	x.mu.Unlock()
	return evs
}

// ---- Manager 对外接口：查询与订阅 ----

// Capabilities 返回某组件声明的能力描述表(索引副本)。
func (m *Manager) Capabilities(plugin string) (map[string]map[string]any, bool) {
	m.mu.Lock()
	x := m.index
	m.mu.Unlock()
	if x == nil {
		return nil, false
	}
	x.mu.RLock()
	defer x.mu.RUnlock()
	byName, ok := x.caps[plugin]
	return byName, ok
}

// Discover 服务发现：返回满足 filter 的全部能力；filter 为 nil 时返回全部。
func (m *Manager) Discover(filter func(CapabilityEntry) bool) []CapabilityEntry {
	m.mu.Lock()
	x := m.index
	m.mu.Unlock()
	if x == nil {
		return nil
	}
	return x.discover(filter)
}

// Status 返回某组件最近一次上报的运行状态(索引副本)。
func (m *Manager) Status(plugin string) (map[string]any, bool) {
	m.mu.Lock()
	x := m.index
	m.mu.Unlock()
	if x == nil {
		return nil, false
	}
	return x.statusOf(plugin)
}

// Subscribe 登记一个索引订阅：内核在索引变化时，把 IndexEvent 以
// Message{Name: Subscriber, Type: TypeIndex, Data: IndexEvent} 投递给订阅者组件。
// 返回的 Disposer 幂等地退订。
func (m *Manager) Subscribe(sub Subscription) (Disposer, error) {
	if sub.Subscriber == "" {
		return nil, newErr(ErrInvalidPlugin, "subscribe: empty subscriber")
	}
	m.mu.Lock()
	x := m.index
	m.mu.Unlock()
	if x == nil {
		return nil, newErr(ErrInvalidPlugin, "subscribe: no index")
	}
	id := x.subscribe(sub)
	return once(func() error {
		x.unsubscribe(id)
		return nil
	}), nil
}

// flushIndexEvents 在锁外把待派发的索引事件以消息投递给订阅者。
func (m *Manager) flushIndexEvents() {
	m.mu.Lock()
	x := m.index
	m.mu.Unlock()
	if x == nil {
		return
	}
	events := x.drainPending()
	if len(events) == 0 {
		return
	}
	subs := x.subscriptions()
	for _, ev := range events {
		for _, sub := range subs {
			if sub.Filter != nil && !sub.Filter(ev) {
				continue
			}
			msg := Message{Name: sub.Subscriber, Type: TypeIndex, Data: ev}
			if _, err := m.Dispatch(&msg); err != nil {
				m.logf(LevelDebug, "index notify %s -> %q: %v", ev.Kind, sub.Subscriber, err)
			}
		}
	}
}

// refreshIndexLocked 在 m.mu 持有下刷新某组件的索引(内部只锁索引)。
func (m *Manager) refreshIndexLocked(name string) {
	if m.index == nil {
		return
	}
	rt := m.rt[name]
	if rt == nil {
		m.index.remove(name)
		return
	}
	var status map[string]any
	if rt.Plugin != nil {
		if s := rt.Plugin.Status(); s != nil {
			status = s
		}
	}
	var caps map[string]map[string]any
	if fo, ok := rt.Plugin.(FunctionOffer); ok {
		if table := fo.Function(); table != nil {
			caps = make(map[string]map[string]any, len(table))
			for k, v := range table {
				if d, ok := v.(map[string]any); ok {
					caps[k] = d
				} else {
					caps[k] = map[string]any{"desc": v}
				}
			}
		}
	}
	m.index.set(name, status, caps, rt.Loaded())
}

// refreshIndex 自行加锁刷新索引，并在锁外派发变更通知。
func (m *Manager) refreshIndex(name string) {
	m.mu.Lock()
	m.refreshIndexLocked(name)
	m.mu.Unlock()
	m.flushIndexEvents()
}
