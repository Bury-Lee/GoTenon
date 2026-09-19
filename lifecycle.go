package GoTenon

import (
	"errors"
	"sync"
	"time"
)

// loadBatch 并行装载一批互不依赖的插件；状态由调度者统一写回。
func (m *Manager) loadBatch(batch []*PluginRuntime) error {
	limit := m.concurrency()
	if limit > len(batch) {
		limit = len(batch)
	}
	timeouts := make([]time.Duration, len(batch))
	names := make([]string, len(batch))
	for i, rt := range batch {
		names[i] = rt.Plugin.Name()
		timeouts[i] = m.timeoutFor(names[i])
		rt.State = Loading // 先置为装载中，成功/失败稍后统一写回
	}
	m.logf(LevelDebug, "load batch: %d plugins, parallel limit %d: %v", len(batch), limit, names)

	errs := make([]error, len(batch))
	durations := make([]time.Duration, len(batch))
	run := func(i int) {
		start := time.Now()
		errs[i] = m.loadOne(batch[i], timeouts[i])
		durations[i] = time.Since(start)
	}
	parallel(limit, len(batch), run)

	// 状态由调度者统一写回：成功的置 Ready，失败的置 Failed 并记 Err
	var firstErr error
	for i, rt := range batch {
		m.logf(LevelDebug, "plugin %q load finished in %s", names[i], durations[i].Round(time.Millisecond))
		if errs[i] != nil {
			rt.State = Failed
			rt.Err = errs[i]
			rt.Missing = nil
			m.logf(LevelWarn, "%v", errs[i])
			if firstErr == nil {
				firstErr = errs[i]
			}
			continue
		}
		rt.State = Ready
		rt.Err = nil
		rt.Missing = nil
	}
	return firstErr
}

// unloadBatch 并行卸载一批互不依赖的叶子插件；状态由调度者统一写回。
func (m *Manager) unloadBatch(batch []*PluginRuntime) error {
	limit := m.concurrency()
	if limit > len(batch) {
		limit = len(batch)
	}
	errs := make([]error, len(batch))
	for _, rt := range batch {
		rt.State = Unloading // 先置为卸载中，收尾状态稍后统一写回
	}
	parallel(limit, len(batch), func(i int) { errs[i] = m.unloadOne(batch[i]) })

	// 卸载完成后统一写回：上下文已清理；仍启用则回到 Pending 等重装
	var firstErr error
	for i, rt := range batch {
		rt.Context = nil
		if rt.Enable {
			rt.State = Pending
		} else {
			rt.State = Disabled
		}
		if errs[i] != nil {
			m.logf(LevelWarn, "plugin %q unload: %v", rt.Plugin.Name(), errs[i])
			if firstErr == nil {
				firstErr = errs[i]
			}
		}
	}
	return firstErr
}

// parallel 以给定上限并行执行 n 个任务；limit<=1 时串行（省去协程开销）。
func parallel(limit, n int, run func(i int)) {
	if limit <= 1 {
		for i := 0; i < n; i++ {
			run(i)
		}
		return
	}
	sem := make(chan struct{}, limit) // 信号量控制同时运行的任务数
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			run(i)
		}(i)
	}
	wg.Wait()
}

// loadOne 装载单个插件：创建子上下文 → Apply → Start → Run。
// 超时预算由 Loader.Timeout 提供；超时后放弃等待、回滚上下文并返回 ErrTimeout。
// 装载协程无法强杀，插件应自行控制装载耗时。
func (m *Manager) loadOne(rt *PluginRuntime, timeout time.Duration) error {
	name := rt.Plugin.Name()
	ctx := m.root.Extend(name)

	// 钩子在独立协程执行：超时只能放弃等待，无法强杀，插件应自行控制耗时
	done := make(chan error, 1)
	go func() {
		err := rt.Plugin.Apply(ctx, rt.Config)
		if err == nil {
			if err = rt.Plugin.Start(); err != nil {
				_ = rt.Plugin.End()
			}
		}
		if err == nil {
			if err = rt.Plugin.Run(); err != nil {
				_ = rt.Plugin.End()
			}
		}
		done <- err
	}()

	var err error
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case err = <-done:
		case <-timer.C:
			err = newErr(ErrTimeout, "plugin %q load timed out after %s", name, timeout)
		}
	} else {
		err = <-done
	}
	if err != nil {
		_ = cleanContext(ctx) // 失败回滚：回收已注册副作用并清空上下文
		return err
	}
	rt.Context = ctx
	return nil
}

// unloadOne 卸载单个插件：End → 逆序回收副作用 → 清理上下文。
func (m *Manager) unloadOne(rt *PluginRuntime) error {
	return errors.Join(rt.Plugin.End(), cleanContext(rt.Context))
}

// cleanContext 递归清理并摘除上下文：逆序回收副作用、断开 children、清空槽位与配置层。
func cleanContext(ctx *GoTenonContext) error {
	if ctx == nil {
		return nil
	}

	// 逆序回收本层副作用
	var errs []error
	if err := ctx.dispose(); err != nil {
		errs = append(errs, err)
	}

	// 先摘下子节点，再递归清理，避免边遍历边修改
	ctx.mu.Lock()
	children := ctx.children
	ctx.children = nil
	parent := ctx.parent
	ctx.mu.Unlock()
	for _, child := range children {
		if err := cleanContext(child); err != nil {
			errs = append(errs, err)
		}
	}

	// 从父链摘除自己
	if parent != nil {
		parent.mu.Lock()
		for i, child := range parent.children {
			if child == ctx {
				parent.children = append(parent.children[:i], parent.children[i+1:]...)
				break
			}
		}
		parent.mu.Unlock()
	}

	// 清空本层数据：槽位、共享槽、配置层、Info
	ctx.mu.Lock()
	ctx.Info = make(map[string]any)
	ctx.slots = make(map[string]*Slot)
	ctx.labels = make(map[string]*Slot)
	ctx.intercept = make(map[string][]any)
	ctx.mu.Unlock()
	return errors.Join(errs...)
}

// concurrency 返回并行装载上限：Loader 可定制，缺省 4。
func (m *Manager) concurrency() int {
	if l, ok := m.Loader.(ConcurrencyLoader); ok {
		if n := l.Concurrency(); n > 0 {
			return n
		}
	}
	if m.Concurrency > 0 {
		return m.Concurrency
	}
	return 4
}

// timeoutFor 返回单插件装载超时：由 Loader 负责；无 Loader 时不限时。
func (m *Manager) timeoutFor(name string) time.Duration {
	if m.Loader == nil {
		return 0
	}
	return m.Loader.Timeout(name)
}
