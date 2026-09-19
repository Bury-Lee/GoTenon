package GoTenon

// loader.go —— 插件加载的扩展层。
//
// Loader 只按插件名回调，不接触 PluginRuntime 与依赖图；
// 启停状态保存在 PluginRuntime 上，钩子失败可中止对应操作。
// 装载超时由 Loader 负责（Timeout）；并行度可选定制（ConcurrencyLoader）。

import "time"

// Loader 是加载和解析插件的扩展层。
type Loader interface {
	IntoRegister(name string) error // 插件注册时的行为
	Delete(name string) error       // 删除时的行为
	Enable(name string) error       // 启用时的行为（级联加载等）
	Disable(name string) error      // 卸载时的行为（级联删除等）
	Hook(name string) error         // 功能执行后的收尾钩子

	// Timeout 返回单插件装载的超时预算；<=0 表示不限时。
	Timeout(name string) time.Duration
}

// ConcurrencyLoader 是 Loader 的可选能力：定制并行装载度（<=1 串行）。
type ConcurrencyLoader interface {
	Concurrency() int
}
