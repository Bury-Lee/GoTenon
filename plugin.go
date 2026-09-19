// plugin.go —— 插件契约：PluginInfo 是宿主与插件之间唯一的接口约定。

package GoTenon

import "context"

// PluginInfo 是可复用的行为单元。
//
// 插件身份是接口指针：同一指针多次挂载共享一个 Runtime，
// 各自创建独立 Fiber（互不影响的挂载实例）。
type PluginInfo interface {
	// Name 返回展示名，用于诊断、日志与父子命名继承；同时作为管理句柄。
	Name() string
	// Desc 返回插件描述信息。
	Desc() map[string]string
	// Inject 返回依赖的服务名列表。
	// 全部依赖就绪前 Fiber 保持 PENDING，Apply 不会执行。
	Inject() []string
	// Config 返回基础配置信息，供调度器做特殊化处理。
	Config() map[string]string

	// Register 在写入插件表之前调用。
	Register() error
	// Apply 在依赖就绪后执行；期间通过 ctx 完成的一切注册归该 Fiber 所有，
	// 卸载时自动逆序回收。返回错误使 Fiber 进入 FAILED（已注册的半成品会被回收）。
	Apply(ctx *GoTenonContext, cfg any) error
	// Start 在装载完成后调用。
	Start() error
	// Run 是主动运行入口，作为「常驻功能」的实现，装载后调用。
	Run() error
	// DealWithMessage 处理发给本插件的消息（消息携带的 context 即 Data）。
	DealWithMessage(context.Context) error
	// End 在卸载前调用，负责资源回收与持久化。
	End() error
}
