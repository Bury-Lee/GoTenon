package GoTenon

// converge 把加载集驱动到稳定态：按依赖波次装载/卸载，同波次并行。
// 返回首个错误。
func (m *Manager) converge() error {
	var firstErr error
	for {
		// 装载优先：依赖就绪的插件先上线，再处理该卸载的插件
		set := m.loadSet()
		if batch := m.pickLoadBatch(set); len(batch) > 0 {
			if err := m.loadBatch(batch); err != nil && firstErr == nil {
				firstErr = err
			}
			continue
		}
		batch := m.pickUnloadBatch(set)
		if len(batch) == 0 {
			return firstErr
		}
		if err := m.unloadBatch(batch); err != nil && firstErr == nil {
			firstErr = err
		}
	}
}

// pickLoadBatch 返回本轮可并行装载的全部插件：在加载闭包内、未装载、未失败，
// 且依赖全部就绪；未就绪的依赖记入 Missing，插件保持 PENDING。
func (m *Manager) pickLoadBatch(set map[*PluginRuntime]bool) []*PluginRuntime {
	var batch []*PluginRuntime
	for _, name := range m.names() {
		rt := m.rt[name]
		if rt.Loaded() || rt.Err != nil || !set[rt] {
			continue
		}
		if missing := m.missingDeps(rt); len(missing) > 0 {
			rt.Missing = missing
			rt.State = Pending // 已启用但依赖未就绪，等依赖装载后自动继续
			continue
		}
		rt.Missing = nil
		batch = append(batch, rt)
	}
	return batch
}

// pickUnloadBatch 返回本轮可并行卸载的插件：不在加载闭包内、无已装载依赖者。
// 只挑叶子，天然保证依赖者先于提供方卸载。
func (m *Manager) pickUnloadBatch(set map[*PluginRuntime]bool) []*PluginRuntime {
	var batch []*PluginRuntime
	for _, name := range m.names() {
		rt := m.rt[name]
		if !rt.Loaded() || set[rt] || hasLoadedDependent(rt) {
			continue
		}
		batch = append(batch, rt)
	}
	return batch
}
