package harness

import (
	"context"
	"errors"

	"GoTenon"
)

// Messages 验证消息系统:类型化信封原封投递、路由错误、异常隔离、系统分支。
func Messages() int {
	t := New("messages")
	m := Manager()

	Section("类型化消息:内核原封投递")
	rec := &Plugin{PluginName: "rec"}
	Register(t, m, rec, nil)
	Enable(t, m, "rec")

	_ = m.Send(GoTenon.Message{Name: "rec", Type: GoTenon.TypeRaw, Data: 42})
	got := rec.Messages()
	t.Check(len(got) == 1, "raw 消息计数 = %d, want 1", len(got))
	if len(got) == 1 {
		t.Eq(got[0].Type, GoTenon.TypeRaw, "raw Type")
		t.Eq(got[0].Data, 42, "raw Data")
	}

	_ = m.Send(GoTenon.Message{Name: "rec", Type: GoTenon.TypeContext, Data: context.Background()})
	got = rec.Messages()
	t.Eq(got[len(got)-1].Type, GoTenon.TypeContext, "context Type")

	Section("路由错误")
	t.Check(GoTenon.IsCode(m.Send(GoTenon.Message{Name: "ghost"}), GoTenon.ErrNotProvided), "未注册 → ErrNotProvided")
	idle := &Plugin{PluginName: "idle"}
	Register(t, m, idle, nil) // 注册但不启用
	t.Check(GoTenon.IsCode(m.Send(GoTenon.Message{Name: "idle"}), GoTenon.ErrNotProvided), "未装载 → ErrNotProvided")

	Section("异常隔离")
	panicP := &Plugin{PluginName: "panic", OnMessage: func(GoTenon.Message) error { panic("boom") }}
	Register(t, m, panicP, nil)
	Enable(t, m, "panic")
	t.Check(m.Send(GoTenon.Message{Name: "panic"}) != nil, "处理器 panic 转为 error")
	errP := &Plugin{PluginName: "err", OnMessage: func(GoTenon.Message) error { return errors.New("nope") }}
	Register(t, m, errP, nil)
	Enable(t, m, "err")
	t.Check(m.Send(GoTenon.Message{Name: "err"}) != nil, "处理器 error 透传")

	Section("系统分支(Name == \"\")")
	_, err := m.Dispatch(nil)
	t.Check(err != nil, "nil 消息被拒")
	_, err = m.Dispatch(&GoTenon.Message{Name: "", Type: GoTenon.TypeContext, Data: context.Background()})
	t.Check(err == nil, "系统上下文消息被接受")
	_, err = m.Dispatch(&GoTenon.Message{Name: "", Type: GoTenon.TypeSignal, Data: "bad"})
	t.Check(GoTenon.IsCode(err, GoTenon.ErrMessageTypeMismatch), "信号载荷类型不匹配 → ErrMessageTypeMismatch")

	return t.Done()
}
