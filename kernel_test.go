package GoTenon

import (
	"testing"
	"time"
)

// recPlugin 记录收到的 Message，并可声明能力与状态。
type recPlugin struct {
	name string
	got  chan Message
	fn   map[string]any
	st   map[string]any
}

func (p *recPlugin) Name() string            { return p.name }
func (p *recPlugin) Desc() map[string]string { return nil }
func (p *recPlugin) Inject() []string        { return nil }
func (p *recPlugin) Status() *map[string]any { return &p.st }
func (p *recPlugin) Register() error         { return nil }
func (p *recPlugin) Apply(*GoTenonContext, any) error {
	return nil
}
func (p *recPlugin) Start() error { return nil }
func (p *recPlugin) Run() error   { return nil }
func (p *recPlugin) DealWithMessage(m Message) error {
	if p.got != nil {
		p.got <- m
	}
	return nil
}
func (p *recPlugin) End() error             { return nil }
func (p *recPlugin) Function() map[string]any { return p.fn }
func (p *recPlugin) ExecuteFunction(any)    {}

func mustRegister(t *testing.T, m *Manager, p PluginInfo) {
	t.Helper()
	if _, err := m.Register(p, nil); err != nil {
		t.Fatalf("register %s: %v", p.Name(), err)
	}
	if err := m.Enable(p.Name()); err != nil {
		t.Fatalf("enable %s: %v", p.Name(), err)
	}
}

// 内核把 Message 原封投递给目标组件，组件自行解包。
func TestDispatchDeliversRawMessage(t *testing.T) {
	m := NewManager(nil)
	ch := make(chan Message, 1)
	p := &recPlugin{name: "p", got: ch}
	mustRegister(t, m, p)

	if err := m.Send(Message{Name: "p", Type: TypeRaw, Data: 42}); err != nil {
		t.Fatal(err)
	}
	got := <-ch
	if got.Type != TypeRaw || got.Data != 42 {
		t.Fatalf("raw message not delivered intact: %+v", got)
	}
}

// 发往系统的未知类型不报错(交给系统分支)。
func TestSystemUnknownType(t *testing.T) {
	m := NewManager(nil)
	if _, err := m.Dispatch(&Message{Name: "", Type: TypeContext}); err != nil {
		t.Fatalf("system message should be accepted: %v", err)
	}
}

// 信号解析失败返回类型不匹配错误。
func TestSignalTypeMismatch(t *testing.T) {
	m := NewManager(nil)
	_, err := m.Dispatch(&Message{Name: "", Type: TypeSignal, Data: "not-a-request"})
	if !IsCode(err, ErrMessageTypeMismatch) {
		t.Fatalf("want ErrMessageTypeMismatch, got %v", err)
	}
}

// SHUTDOWN 信号使组件主动关闭自身。
func TestSignalShutdownSelf(t *testing.T) {
	m := NewManager(nil)
	mustRegister(t, m, &recPlugin{name: "p"})

	sig, ok := m.Signals.Lookup("SHUTDOWN")
	if !ok {
		t.Fatal("SHUTDOWN signal missing")
	}
	if _, err := m.Signal(SignalRequest{From: "p", Signal: sig}); err != nil {
		t.Fatal(err)
	}
	rt, _ := m.Get("p")
	if rt.State != Disabled {
		t.Fatalf("self shutdown should disable plugin, got %v", rt.State)
	}
}

// REQ_STOP 信号使组件申请关闭其他组件。
func TestSignalRequestStopOther(t *testing.T) {
	m := NewManager(nil)
	mustRegister(t, m, &recPlugin{name: "a"})
	mustRegister(t, m, &recPlugin{name: "b"})

	sig, _ := m.Signals.Lookup("REQ_STOP")
	if _, err := m.Signal(SignalRequest{From: "a", Target: "b", Signal: sig}); err != nil {
		t.Fatal(err)
	}
	if rt, _ := m.Get("b"); rt.Loaded() {
		t.Fatal("b should be stopped by signal")
	}
}

// 索引表：能力可被查询与发现；订阅者通过消息(Type=TypeIndex)收到变更事件。
func TestIndexDiscoverAndSubscribe(t *testing.T) {
	m := NewManager(nil)
	subCh := make(chan Message, 16)
	sub := &recPlugin{name: "watcher", got: subCh}
	mustRegister(t, m, sub)

	if _, err := m.Subscribe(Subscription{
		Subscriber: "watcher",
		Filter:     func(e IndexEvent) bool { return e.Plugin == "p" },
	}); err != nil {
		t.Fatal(err)
	}

	p := &recPlugin{name: "p", fn: map[string]any{"draw": map[string]any{"desc": "draw"}}}
	mustRegister(t, m, p)

	caps, ok := m.Capabilities("p")
	if !ok || caps["draw"] == nil {
		t.Fatalf("capability not indexed: %v %v", caps, ok)
	}
	if found := m.Discover(func(e CapabilityEntry) bool { return e.Name == "draw" && e.Available }); len(found) != 1 {
		t.Fatalf("discover failed: %+v", found)
	}

	// 订阅者应通过消息收到 p 上线的索引事件。
	sawUp := false
	for i := 0; i < 16 && !sawUp; i++ {
		select {
		case msg := <-subCh:
			if msg.Type != TypeIndex {
				continue
			}
			if ev, ok := msg.Data.(IndexEvent); ok && ev.Plugin == "p" && ev.Kind == "up" {
				sawUp = true
			}
		case <-time.After(time.Second):
			i = 16
		}
	}
	if !sawUp {
		t.Fatal("subscriber did not receive index 'up' message")
	}
}
