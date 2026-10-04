package GoTenon

import (
	"sync/atomic"
	"testing"
	"time"
)

// recvPlugin 记录收到的消息数(用于验证装载期投递)。
type recvPlugin struct {
	name string
	got  *int32
}

func (p *recvPlugin) Name() string            { return p.name }
func (p *recvPlugin) Desc() map[string]string { return nil }
func (p *recvPlugin) Inject() []string        { return nil }
func (p *recvPlugin) Status() *map[string]any { return nil }
func (p *recvPlugin) Register() error         { return nil }
func (p *recvPlugin) Apply(*GoTenonContext, any) error {
	return nil
}
func (p *recvPlugin) Start() error { return nil }
func (p *recvPlugin) Run() error   { return nil }
func (p *recvPlugin) DealWithMessage(Message) error {
	atomic.AddInt32(p.got, 1)
	return nil
}
func (p *recvPlugin) End() error { return nil }

// senderPlugin 在自己的 Apply(装载期)向已装载的目标发一条消息。
// 旧实现里这会因生命周期锁重入而死锁;新实现应成功投递。
type senderPlugin struct {
	name   string
	m      *Manager
	target string
}

func (p *senderPlugin) Name() string            { return p.name }
func (p *senderPlugin) Desc() map[string]string { return nil }
func (p *senderPlugin) Inject() []string        { return nil }
func (p *senderPlugin) Status() *map[string]any { return nil }
func (p *senderPlugin) Register() error         { return nil }
func (p *senderPlugin) Apply(*GoTenonContext, any) error {
	return p.m.Send(Message{Name: p.target, Type: TypeRaw, Data: "hi"})
}
func (p *senderPlugin) Start() error                          { return nil }
func (p *senderPlugin) Run() error                            { return nil }
func (p *senderPlugin) DealWithMessage(Message) error         { return nil }
func (p *senderPlugin) End() error                            { return nil }

// TestSendDuringLoadNoDeadlock 验证:消息面与生命周期锁解耦后,
// 组件在装载期(Apply)发消息给已装载组件不会死锁,且能成功投递。
func TestSendDuringLoadNoDeadlock(t *testing.T) {
	m := NewManager(New("root"))

	var got int32
	recv := &recvPlugin{name: "recv", got: &got}
	sender := &senderPlugin{name: "sender", m: m, target: "recv"}

	if _, err := m.Register(recv, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Register(sender, nil); err != nil {
		t.Fatal(err)
	}
	if err := m.Enable("recv"); err != nil { // 目标先上线
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- m.Enable("sender") }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("装载期发消息失败: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("装载期发消息死锁:Enable(sender) 未在超时内返回")
	}

	if n := atomic.LoadInt32(&got); n != 1 {
		t.Fatalf("目标应收 1 条装载期消息,实际 %d", n)
	}
}
