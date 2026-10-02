package GoTenon

import "slices"

// relink 重建全部依赖边：Inject() 的服务名按插件名解析。
func (m *Manager) relink() {
	for _, rt := range m.rt {
		rt.Dependence = nil
		rt.Dependenced = nil
	}
	for _, rt := range m.rt {
		for _, name := range rt.Plugin.Inject() {
			dep := m.rt[name]
			if dep == nil {
				continue
			}
			rt.Dependence = append(rt.Dependence, dep)
			dep.Dependenced = append(dep.Dependenced, rt)
		}
	}
}

// checkCycle 深度优先检查依赖成环；path 记录当前 DFS 路径，命中即环。
func (m *Manager) checkCycle(rt *PluginRuntime, path []*PluginRuntime) error {
	for _, p := range path {
		if p == rt {
			return newErr(ErrInvalidPlugin, "manager: dependency cycle at %q", rt.Plugin.Name())
		}
	}
	path = append(path, rt)
	for _, dep := range rt.Dependence {
		if err := m.checkCycle(dep, path); err != nil {
			return err
		}
	}
	return nil
}

// names 返回按名字排序的插件名，保证收敛顺序确定。
func (m *Manager) names() []string {
	names := make([]string, 0, len(m.rt))
	for name := range m.rt {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// loadSet 计算本轮应装载的闭包：从启用标志出发，沿 Dependence 扩散。
// 不在闭包内的插件不该保持装载，引用归零即自动卸载。
func (m *Manager) loadSet() map[*PluginRuntime]bool {
	// 从启用标志出发，沿依赖边扩散出加载闭包
	set := make(map[*PluginRuntime]bool)
	var walk func(*PluginRuntime)
	walk = func(rt *PluginRuntime) {
		if rt == nil || set[rt] {
			return
		}
		set[rt] = true
		for _, dep := range rt.Dependence {
			walk(dep)
		}
	}
	for _, name := range m.names() {
		if rt := m.rt[name]; rt.Enable {
			walk(rt)
		}
	}
	return set
}

// missingDeps 返回 Inject() 中尚未就绪的依赖（未注册或未装载）。
func (m *Manager) missingDeps(rt *PluginRuntime) []string {
	var missing []string
	for _, name := range rt.Plugin.Inject() {
		dep := m.rt[name]
		if dep == nil || dep == rt || !dep.Loaded() {
			missing = append(missing, name)
		}
	}
	return missing
}

// hasLoadedDependent 判断插件是否仍有已装载的依赖者。
func hasLoadedDependent(rt *PluginRuntime) bool {
	for _, dep := range rt.Dependenced {
		if dep.Loaded() {
			return true
		}
	}
	return false
}
