package GoTenon

import "sync"

// route.go —— 消息路由:与生命周期调度解耦。
//
// 存储用 sync.Map:典型「读多写少」——每条消息都 Load(基本无锁),
// 而登记/摘除只在装载/卸载/删除/故障时发生(增量 O(1),不做整表重建),
// 适合上百上千插件、频繁热插拔的场景。
//
// 锁序:生命周期锁 mu → router 内部同步(单向)。router 永不回调 Manager,避免锁反转。

// router 是消息面的路由表:名字 → 已装载(可投递)的插件,增量维护。
type router struct {
	loaded sync.Map // name(string) -> PluginInfo
}

func newRouter() *router { return &router{} }

// set 登记一个可投递插件(幂等)。
func (r *router) set(name string, p PluginInfo) {
	if r == nil || p == nil {
		return
	}
	r.loaded.Store(name, p)
}

// remove 摘除一个插件(幂等)。
func (r *router) remove(name string) {
	if r == nil {
		return
	}
	r.loaded.Delete(name)
}

// lookup 查一个可投递插件;nil 接收者视为空。
func (r *router) lookup(name string) (PluginInfo, bool) {
	if r == nil {
		return nil, false
	}
	v, ok := r.loaded.Load(name)
	if !ok {
		return nil, false
	}
	p, _ := v.(PluginInfo)
	return p, ok
}

// routerAdd / routerRemove 由调度器在持有 m.mu 时调用,做增量登记/摘除。
func (m *Manager) routerAdd(name string, plugin PluginInfo) {
	if m.router != nil {
		m.router.set(name, plugin)
	}
}

func (m *Manager) routerRemove(name string) {
	if m.router != nil {
		m.router.remove(name)
	}
}
