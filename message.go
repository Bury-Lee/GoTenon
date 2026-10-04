package GoTenon

// message.go —— 内核消息模型与默认系统消息处理器。
//
// Message 是内核统一消息信封 {Name, Type, Data}：
//   - Name == "" 表示发往系统/内核，由 MessageProcesser 默认实现按 Type 解析；
//   - Name != "" 表示发往指定组件，内核只做查表与原封投递，由组件自行解包。
//
// MessageType 是信封上的类型标签：0–15 为系统保留，16+ 为组件自定义。

// MessageType 是消息类型码(discriminator)。
type MessageType int

const (
	// ---- 系统保留段：0–15(共 16 个)----
	TypeUnknown MessageType = 0 // 未指定
	TypeContext MessageType = 1 // Data: context.Context(取消/超时/追踪)
	TypeMessage MessageType = 2 // Data: *Message(嵌套/转发)
	TypeRaw     MessageType = 3 // Data: any(自定义/能力调用载荷)
	TypeSignal  MessageType = 4 // Data: *SignalRequest(系统信号)
	TypeReply   MessageType = 5 // Data: *Message(应答)
	TypeJSON    MessageType = 6 // Data: []byte 或 map(JSON 载荷)
	TypeIndex   MessageType = 7 // Data: IndexEvent(索引变更通知)
	// 8–15 继续由系统保留
	TypeSystemReservedEnd MessageType = 15

	// ---- 组件自定义段：16+ ----
	TypeCustomBase MessageType = 16
)

// Message 是内核统一消息信封。
type Message struct {
	Name string      // 目标；空 = 发往系统/内核
	Type MessageType // 类型码：信封标签，语义由接收方解释
	Data any         // 载荷
}

// MessageProcesser 是系统/内核的默认消息处理器。
type MessageProcesser interface {
	// Handle 处理一条消息：系统消息，或把 Message 原封投递给目标组件。
	// 返回的 reply 为 nil 表示无需应答。
	Handle(msg *Message) (*Message, error)
}

// DefaultMessageProcesser 是 MessageProcesser 的默认系统实现。
type DefaultMessageProcesser struct {
	Manager *Manager
}

// NewMessageProcessor 创建默认处理器。
func NewMessageProcessor(m *Manager) *DefaultMessageProcesser {
	return &DefaultMessageProcesser{Manager: m}
}

// Handle 实现 MessageProcesser：系统分支按 Type 解析，其它原封投递。
func (p *DefaultMessageProcesser) Handle(msg *Message) (*Message, error) {
	if msg == nil {
		return nil, newErr(ErrInvalidPlugin, "processor: nil message")
	}
	if p.Manager == nil {
		return nil, newErr(ErrInvalidPlugin, "processor: nil manager")
	}
	if msg.Name == "" {
		return p.Manager.handleSystem(msg)
	}
	return p.deliver(msg)
}

// deliver 把 Message 原封投递给目标组件；内核不替组件解包。
// 走消息面 router(独立锁)而非生命周期锁,故装载期(Apply/Start/Run)也能安全投递。
func (p *DefaultMessageProcesser) deliver(msg *Message) (*Message, error) {
	m := p.Manager
	plugin, ok := m.router.lookup(msg.Name)
	if !ok {
		return nil, newErr(ErrNotProvided, "manager: plugin %q not provided", msg.Name)
	}
	err := recoverToError(func() error { return plugin.DealWithMessage(*msg) })
	return nil, err
}
