package GoTenon

import "sync"

// signal.go —— 信号表：组件向调度内核表达意图。
//
// 信号随 Message(Type=TypeSignal, Name="") 发往内核，由 MessageProcesser 默认实现解析；
// 内核据信号执行相应动作(关闭/启用/重载/删除、故障隔离、刷新索引等)。

// SignalKind 是信号的稳定编码。
type SignalKind uint16

const (
	SigNone          SignalKind = iota
	SigReady                    // 组件就绪上报
	SigShutdown                 // 主动关闭自身
	SigRequestStop              // 申请关闭指定组件
	SigRequestEnable            // 申请启用指定组件
	SigRequestReload            // 申请重载指定组件
	SigRequestDelete            // 申请删除指定组件
	SigFault                    // 故障上报
	SigPanic                    // 运行期崩溃上报
	SigDeclare                  // 声明/刷新能力(驱动索引表)
	SigRetract                  // 撤回能力(驱动索引表)
	SigCustom                   // 自定义信号起始
)

// Signal 是一种信号：稳定编码 + 名称 + 语义。
type Signal struct {
	Kind SignalKind
	Name string
	Desc string
}

// SignalRequest 是信号消息(Type=TypeSignal)的载荷。
type SignalRequest struct {
	From   string // 发送者组件名；空 = 内核/宿主
	Target string // 目标组件(为空表示发给系统处理)
	Signal Signal
	Args   any
}

// SignalTable 是信号注册表：名称/Kind 双向索引，可扩展自定义信号。
type SignalTable struct {
	mu     sync.RWMutex
	byName map[string]Signal
	byKind map[SignalKind]Signal
}

// DefaultSignalTable 返回内置信号表。
func DefaultSignalTable() *SignalTable {
	t := &SignalTable{
		byName: make(map[string]Signal),
		byKind: make(map[SignalKind]Signal),
	}
	for _, s := range []Signal{
		{SigReady, "READY", "组件就绪"},
		{SigShutdown, "SHUTDOWN", "主动关闭自身"},
		{SigRequestStop, "REQ_STOP", "申请关闭指定组件"},
		{SigRequestEnable, "REQ_ENABLE", "申请启用指定组件"},
		{SigRequestReload, "REQ_RELOAD", "申请重载指定组件"},
		{SigRequestDelete, "REQ_DELETE", "申请删除指定组件"},
		{SigFault, "FAULT", "故障上报"},
		{SigPanic, "PANIC", "运行期崩溃上报"},
		{SigDeclare, "DECLARE", "声明/刷新能力"},
		{SigRetract, "RETRACT", "撤回能力"},
	} {
		t.byName[s.Name] = s
		t.byKind[s.Kind] = s
	}
	return t
}

// Register 登记一个自定义信号；重名返回 ErrDuplicate。
func (t *SignalTable) Register(s Signal) error {
	if s.Name == "" {
		return newErr(ErrInvalidPlugin, "signal: empty name")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, exists := t.byName[s.Name]; exists {
		return newErr(ErrDuplicate, "signal: %q already registered", s.Name)
	}
	t.byName[s.Name] = s
	t.byKind[s.Kind] = s
	return nil
}

// Lookup 按名称查找信号。
func (t *SignalTable) Lookup(name string) (Signal, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.byName[name]
	return s, ok
}

// LookupKind 按编码查找信号。
func (t *SignalTable) LookupKind(k SignalKind) (Signal, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	s, ok := t.byKind[k]
	return s, ok
}

// NewSignalMessage 构造一条发往系统的信号消息。
func NewSignalMessage(from, target string, sig Signal, args any) Message {
	return Message{
		Name: "", // 发给系统，由内核处理
		Type: TypeSignal,
		Data: &SignalRequest{From: from, Target: target, Signal: sig, Args: args},
	}
}
