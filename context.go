// context.go —— 上下文树与作用域：Info 继承、私有/共享槽位、配置层。

package GoTenon

import (
	"sync"
)

// GoTenonContext 是插件的安全上下文：每个插件一份，承载作用域与注册归属。
type GoTenonContext struct {
	Name string         // 来自哪个插件
	Info map[string]any // 运行时信息，仅本上下文私有，读取时沿祖先链回退

	parent   *GoTenonContext   // 父上下文，root 为 nil
	children []*GoTenonContext // 派生的子上下文（弱引用即可，仅用于遍历/调试）

	mu        sync.RWMutex
	slots     map[string]*Slot // 本层 Isolate 出的独立槽位：name -> slot
	labels    map[string]*Slot // label 共享槽：name + "\x00" + label -> slot
	intercept map[string][]any // 本层追加的配置层：name -> 配置栈
	effects   []*effectScope   // effect 栈：effects[0] 为基座，栈顶为当前 scope
}

// Slot 表示一个被隔离出来的槽位。
type Slot struct {
	Name  string
	Label string // 空表示私有 Isolate，非空表示 IsolateLabel 共享
	Value any    // 实际承载的服务实例 / 句柄
	Owner *GoTenonContext
}

// New 创建根上下文。
func New(name string) *GoTenonContext {
	return &GoTenonContext{
		Name:      name,
		Info:      make(map[string]any),
		slots:     make(map[string]*Slot),
		labels:    make(map[string]*Slot),
		intercept: make(map[string][]any),
		effects:   []*effectScope{{label: name}},
	}
}

// Extend 派生子上下文。子继承父的全部能力，永不修改父。
func (c *GoTenonContext) Extend(name string) *GoTenonContext {
	child := &GoTenonContext{
		Name:      name,
		Info:      make(map[string]any),
		parent:    c,
		slots:     make(map[string]*Slot),
		labels:    make(map[string]*Slot),
		intercept: make(map[string][]any),
		effects:   []*effectScope{{label: name}},
	}
	c.mu.Lock()
	c.children = append(c.children, child)
	c.mu.Unlock()
	return child
}

// SetInfo 写入本层运行时信息（只影响自己）。
func (c *GoTenonContext) SetInfo(key string, val any) {
	c.mu.Lock()
	c.Info[key] = val
	c.mu.Unlock()
}

// GetInfo 读取运行时信息，本层没有则沿祖先链向上找。
func (c *GoTenonContext) GetInfo(key string) (any, bool) {
	for ctx := c; ctx != nil; ctx = ctx.parent {
		ctx.mu.RLock()
		v, ok := ctx.Info[key]
		ctx.mu.RUnlock()
		if ok {
			return v, true
		}
	}
	return nil, false
}

// Isolate 在本层为 name 分配私有槽位，使该子树内的解析独立于祖先。
// 同一层重复调用复用同一槽，不会覆盖已写入的值。
func (c *GoTenonContext) Isolate(name string) *GoTenonContext {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.slots[name]; !exists {
		c.slots[name] = &Slot{Name: name, Owner: c}
	}
	return c
}

// IsolateLabel 让相同 label 的子树共享同一槽位。
// label 相同则复用已有槽，不同则新建。
func (c *GoTenonContext) IsolateLabel(name, label string) *GoTenonContext {
	key := name + "\x00" + label // \x00 分隔，避免 name/label 拼接撞车

	// 先看祖先链里有没有同 label 的槽，有就复用（跨子树共享）
	if s := c.findLabelSlot(name, label); s != nil {
		return c
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.labels[key]; !exists {
		c.labels[key] = &Slot{Name: name, Label: label, Owner: c}
	}
	return c
}

// findLabelSlot 沿祖先链查找同 (name,label) 的共享槽。
func (c *GoTenonContext) findLabelSlot(name, label string) *Slot {
	key := name + "\x00" + label
	for ctx := c; ctx != nil; ctx = ctx.parent {
		ctx.mu.RLock()
		s, ok := ctx.labels[key]
		ctx.mu.RUnlock()
		if ok {
			return s
		}
	}
	return nil
}

// Intercept 追加一层配置，解析时沿祖先链 root-first 合并。
func (c *GoTenonContext) Intercept(name string, cfg any) *GoTenonContext {
	c.mu.Lock()
	c.intercept[name] = append(c.intercept[name], cfg)
	c.mu.Unlock()
	return c
}

// Config 返回 name 的配置层，根到深顺序。
// 调用方可据此依次合并，后层覆盖前层。
func (c *GoTenonContext) Config(name string) []any {
	var chain []*GoTenonContext
	for ctx := c; ctx != nil; ctx = ctx.parent {
		chain = append(chain, ctx)
	}
	// 反转成 root-first
	for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
		chain[i], chain[j] = chain[j], chain[i]
	}

	var layers []any
	for _, ctx := range chain {
		ctx.mu.RLock()
		layers = append(layers, ctx.intercept[name]...)
		ctx.mu.RUnlock()
	}
	return layers
}

// SlotOf 返回当前上下文实际解析到的槽位（仅用于诊断）。
// 优先级：本层私有槽 > 祖先链私有槽 > label 共享槽。
func (c *GoTenonContext) SlotOf(name string) *Slot {
	// 1. 沿祖先链找私有槽（越近越优先）
	for ctx := c; ctx != nil; ctx = ctx.parent {
		ctx.mu.RLock()
		s, ok := ctx.slots[name]
		ctx.mu.RUnlock()
		if ok {
			return s
		}
	}
	// 2. 再沿祖先链找共享槽；同层同 name 有多个 label 时返回顺序未定义
	for ctx := c; ctx != nil; ctx = ctx.parent {
		ctx.mu.RLock()
		for _, s := range ctx.labels {
			if s.Name == name {
				ctx.mu.RUnlock()
				return s
			}
		}
		ctx.mu.RUnlock()
	}
	return nil
}
