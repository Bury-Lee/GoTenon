package GoTenon

import (
	"errors"
	"fmt"
	"sync"
)

// Disposer 是幂等的撤销函数：重复调用是 no-op；panic 会被 recover 并转为 error。
// 返回的 error 只用于日志，不阻断其余 disposer。
type Disposer func() error

// Noop 返回一个什么都不做的 Disposer。
func Noop() Disposer { return func() error { return nil } }

// once 把 fn 包装成至多执行一次的幂等 Disposer。
func once(fn func() error) Disposer {
	var o sync.Once
	var err error
	return func() error {
		o.Do(func() {
			err = runDisposer(fn)
		})
		return err
	}
}

// runDisposer 执行一个 Disposer，把 panic 转为 error。
func runDisposer(d Disposer) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("GoTenon: disposer panic: %v", r)
		}
	}()
	return d()
}

// runBody 执行 effect body，把 panic 转为 error。
func runBody(body func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("GoTenon: effect body panic: %v", r)
		}
	}()
	return body()
}

// effectScope 收集一段 body 期间注册的 Disposer，作为一个整体逆序回收。
type effectScope struct {
	mu     sync.Mutex
	parent *effectScope
	label  string
	items  []Disposer
	once   sync.Once
	err    error
}

func (s *effectScope) add(d Disposer) {
	if s == nil || d == nil {
		return
	}
	s.mu.Lock()
	s.items = append(s.items, d)
	s.mu.Unlock()
}

// dispose 逆序执行全部 Disposer；单个失败不阻断其余。
func (s *effectScope) dispose() error {
	if s == nil {
		return nil
	}
	s.once.Do(func() {
		s.mu.Lock()
		items := s.items
		s.items = nil
		s.mu.Unlock()

		var errs []error
		for i := len(items) - 1; i >= 0; i-- {
			if err := runDisposer(items[i]); err != nil {
				errs = append(errs, err)
			}
		}
		s.err = errors.Join(errs...)
	})
	return s.err
}

// Register 把一个 Disposer 挂到当前 effect scope；没有活动 scope 时挂到 ctx 基座。
// 卸载时按注册顺序逆序执行。返回幂等包装后的 Disposer。
func (c *GoTenonContext) Register(d Disposer) Disposer {
	if d == nil {
		return Noop()
	}
	d = once(d)
	// 挂到栈顶 scope；栈空（已 dispose）时补一个基座，避免 nil 解引用
	c.mu.Lock()
	if len(c.effects) == 0 {
		c.effects = append(c.effects, &effectScope{label: c.Name})
	}
	scope := c.effects[len(c.effects)-1]
	c.mu.Unlock()

	scope.add(d)
	return d
}

// Effect 执行 body，把 body 期间注册的 Disposer 归为一组；body 失败（含 panic）
// 时立即逆序回收半成品并返回错误。返回的组 Disposer 挂在父 scope 上，
// 嵌套 Effect 形成一棵可逆序拆解的树。
func (c *GoTenonContext) Effect(body func() error, label string) (Disposer, error) {
	if body == nil {
		return nil, errors.New("GoTenon: Effect: nil body")
	}
	scope := c.pushScope(label)
	err := runBody(body)
	c.popScope(scope)
	if err != nil {
		_ = scope.dispose()
		return nil, err
	}
	return c.Register(scope.dispose), nil
}

// dispose 逆序回收 ctx 的全部 Disposer（卸载时由 Manager 调用）。
func (c *GoTenonContext) dispose() error {
	c.mu.Lock()
	scopes := c.effects
	c.effects = nil
	c.mu.Unlock()

	var errs []error
	for i := len(scopes) - 1; i >= 0; i-- {
		if err := scopes[i].dispose(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// pushScope 压入一个子 scope。
func (c *GoTenonContext) pushScope(label string) *effectScope {
	c.mu.Lock()
	defer c.mu.Unlock()
	scope := &effectScope{label: label}
	if n := len(c.effects); n > 0 {
		scope.parent = c.effects[n-1]
	}
	c.effects = append(c.effects, scope)
	return scope
}

// popScope 弹出指定 scope（异常路径下也保证摘除）。
func (c *GoTenonContext) popScope(scope *effectScope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.effects); n > 0 && c.effects[n-1] == scope {
		c.effects = c.effects[:n-1]
		return
	}
	for i, s := range c.effects {
		if s == scope {
			c.effects = append(c.effects[:i], c.effects[i+1:]...)
			return
		}
	}
}
