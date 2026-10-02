package GoTenon

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// ---- context 作用域 ----

func TestContextScope(t *testing.T) {
	root := New("root")
	child := root.Extend("child")
	root.SetInfo("env", "dev")
	if v, ok := child.GetInfo("env"); !ok || v != "dev" {
		t.Fatalf("info inherit failed: %v %v", v, ok)
	}

	root.Isolate("svc")
	root.SlotOf("svc").Value = 1
	if got := child.SlotOf("svc"); got == nil || got.Value != 1 {
		t.Fatal("isolate slot resolve failed")
	}

	root.Intercept("http", "l1")
	child.Intercept("http", "l2")
	layers := child.Config("http")
	if len(layers) != 2 || layers[0] != "l1" || layers[1] != "l2" {
		t.Fatalf("config order root-first failed: %v", layers)
	}
}

// ---- effect 可逆副作用 ----

func TestEffectReverseAndIdempotent(t *testing.T) {
	ctx := New("p")
	var order []int
	ctx.Register(func() error { order = append(order, 1); return nil })
	ctx.Register(func() error { order = append(order, 2); return nil })

	if err := ctx.dispose(); err != nil {
		t.Fatal(err)
	}
	if len(order) != 2 || order[0] != 2 || order[1] != 1 {
		t.Fatalf("reverse order failed: %v", order)
	}
	_ = ctx.dispose() // 幂等
	if len(order) != 2 {
		t.Fatal("dispose not idempotent")
	}
}

func TestEffectPanicIsolation(t *testing.T) {
	ctx := New("p")
	ran := false
	ctx.Register(func() error { panic("boom") })
	ctx.Register(func() error { ran = true; return nil })

	if err := ctx.dispose(); err == nil {
		t.Fatal("panic disposer should produce error")
	}
	if !ran {
		t.Fatal("panic should not block subsequent disposers")
	}
}

func TestEffectGroupRollback(t *testing.T) {
	ctx := New("p")
	rolled := false
	_, err := ctx.Effect(func() error {
		ctx.Register(func() error { rolled = true; return nil })
		return errors.New("body failed")
	}, "grp")
	if err == nil {
		t.Fatal("expected body error")
	}
	if !rolled {
		t.Fatal("group should roll back on body failure")
	}
}

func TestRegisterAfterDispose(t *testing.T) {
	ctx := New("p")
	_ = ctx.dispose()
	d := ctx.Register(func() error { return nil })
	if err := d(); !IsCode(err, ErrInactiveEffect) {
		t.Fatalf("want ErrInactiveEffect, got %v", err)
	}
}

// ---- graph / 环检测 ----

func TestCycleDetection(t *testing.T) {
	m := NewManager(nil)
	must(t, func() error { _, e := m.Register(&testPlugin{name: "a", inject: []string{"b"}}, nil); return e }())
	_, err := m.Register(&testPlugin{name: "b", inject: []string{"a"}}, nil)
	if err == nil || !IsCode(err, ErrInvalidPlugin) {
		t.Fatalf("want cycle error, got %v", err)
	}
}

// ---- converge: 依赖闭包与 PENDING ----

func TestEnableLoadsClosureAndPending(t *testing.T) {
	m := NewManager(nil)
	must(t, func() error { _, e := m.Register(&testPlugin{name: "a"}, nil); return e }())
	must(t, func() error { _, e := m.Register(&testPlugin{name: "b", inject: []string{"a"}}, nil); return e }())
	must(t, func() error { _, e := m.Register(&testPlugin{name: "c", inject: []string{"missing"}}, nil); return e }())

	must(t, m.Enable("b"))
	if rt, _ := m.Get("a"); !rt.Loaded() {
		t.Fatal("dependency a should be loaded")
	}
	if rt, _ := m.Get("b"); !rt.Loaded() {
		t.Fatal("b should be loaded")
	}

	must(t, m.Enable("c"))
	if rt, _ := m.Get("c"); rt.State != Pending {
		t.Fatalf("c should stay Pending, got %v", rt.State)
	}
}

// ---- lifecycle: 失败回滚 / 超时协作取消 ----

func TestLoadFailureRollback(t *testing.T) {
	m := NewManager(nil)
	disposed := int32(0)
	p := &testPlugin{name: "p", startErr: errors.New("start fail"), disposed: &disposed}
	must(t, func() error { _, e := m.Register(p, nil); return e }())

	if err := m.Enable("p"); err == nil {
		t.Fatal("expected enable failure")
	}
	if atomic.LoadInt32(&disposed) == 0 {
		t.Fatal("half-registered side effects should be rolled back")
	}
	if rt, _ := m.Get("p"); rt.State != Failed {
		t.Fatalf("want Failed, got %v", rt.State)
	}
}

func TestLoadTimeoutRejectsLateRegister(t *testing.T) {
	done := make(chan struct{})
	slow := &slowPlugin{name: "slow", block: 200 * time.Millisecond, done: done}
	m := NewManager(nil)
	m.Loader = &timeoutLoader{d: 30 * time.Millisecond}
	must(t, func() error { _, e := m.Register(slow, nil); return e }())

	err := m.Enable("slow")
	if err == nil || !IsCode(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	<-done // 等慢协程结束
	if !IsCode(slow.lateErr, ErrInactiveEffect) {
		t.Fatalf("late register should be rejected, got %v", slow.lateErr)
	}
}

// ---- message / 运行期 panic 隔离 ----

func TestSend(t *testing.T) {
	m := NewManager(nil)
	must(t, func() error { _, e := m.Register(&testPlugin{name: "p"}, nil); return e }())
	must(t, m.Enable("p"))

	if err := m.Send(Message{Name: "p", Data: context.Background()}); err != nil {
		t.Fatal(err)
	}
	if err := m.Send(Message{Name: "nope"}); err == nil || !IsCode(err, ErrNotProvided) {
		t.Fatalf("want NotProvided, got %v", err)
	}
}

func TestSendPanicIsolated(t *testing.T) {
	m := NewManager(nil)
	must(t, func() error { _, e := m.Register(&testPlugin{name: "p", dealPanic: true}, nil); return e }())
	must(t, m.Enable("p"))
	if err := m.Send(Message{Name: "p"}); err == nil {
		t.Fatal("panic should be converted to error")
	}
}

// ---- 事务化 Update ----

func TestUpdateRollbackPolicy(t *testing.T) {
	m := NewManager(nil)
	p := &testPlugin{name: "p", failCfg: "bad"}
	must(t, func() error { _, e := m.Register(p, "good"); return e }())
	must(t, m.Enable("p"))

	if err := m.Update("p", "bad"); err == nil {
		t.Fatal("update with bad cfg should fail")
	}
	rt, _ := m.Get("p")
	if rt.Config != "good" {
		t.Fatalf("config should roll back to good, got %v", rt.Config)
	}
	if !rt.Loaded() {
		t.Fatalf("should be reloaded with old config, state=%v err=%v", rt.State, rt.Err)
	}
}
