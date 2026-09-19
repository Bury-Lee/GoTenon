package GoTenon

import (
	"context"
	"sync"
)

// message.go —— 事件与消息处理。
//
// Message 是插件间通信的消息：Name 呼叫哪个插件，Data 携带信号（context）。
// MessageProcessor 是独立的事件处理层，默认实现为 DefaultMessageProcessor：
// 注册事件发送管道，并把消息交给 Manager 路由。

// Message 是插件间通信的消息。
type Message struct {
	Name string          // 目标插件名
	Data context.Context // 携带的信号
}

// MessageProcessor 是独立的事件处理层，提供默认实现 DefaultMessageProcessor。
//
//   - RegisterPipe：注册事件发送管道；
//   - Handle：调用 Manager 处理事件并返回 error（插件不存在/未装载等）。
type MessageProcessor interface {
	// RegisterPipe 注册事件发送管道：写进管道的消息被依次处理；
	// 返回的取消函数发出停止信号，消费协程退出（不死协程）。
	RegisterPipe(buffer int) (chan<- Message, func())
	// Handle 调用 Manager 处理事件并返回 error（插件不存在/未装载/处理失败）。
	Handle(msg Message) error
}

// DefaultMessageProcessor 是 MessageProcessor 的默认实现。
// OnError 可选，用于接收管道消费中的错误（Logger 落地前的出口）。
type DefaultMessageProcessor struct {
	Manager *Manager
	OnError func(msg Message, err error)
}

// NewMessageProcessor 创建默认处理器。
func NewMessageProcessor(m *Manager) *DefaultMessageProcessor {
	return &DefaultMessageProcessor{Manager: m}
}

// RegisterPipe 注册一条事件发送管道：写进管道的消息被依次 Handle。
// 返回的取消函数幂等：发出停止信号，消费协程退出。
func (p *DefaultMessageProcessor) RegisterPipe(buffer int) (chan<- Message, func()) {
	in := make(chan Message, buffer)
	stop := make(chan struct{})
	var once sync.Once

	// 消费协程：串行处理管道消息，收到 stop 信号即退出
	go func() {
		for {
			select {
			case msg, ok := <-in:
				if !ok {
					return
				}
				if err := p.Handle(msg); err != nil && p.OnError != nil {
					p.OnError(msg, err)
				}
			case <-stop:
				return
			}
		}
	}()

	cancel := func() {
		once.Do(func() { close(stop) })
	}
	return in, cancel
}

// Handle 调用 Manager 处理事件：目标插件不存在或未装载返回错误。
func (p *DefaultMessageProcessor) Handle(msg Message) error {
	if p.Manager == nil {
		return newErr(ErrInvalidPlugin, "processor: nil manager")
	}
	return p.Manager.Send(msg)
}
