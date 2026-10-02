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

	Status() *map[string]any //返回一个json,这个json应该记录各种动态运行时信息,包括运行状态等,由插件自己管理.为空时表示不可用

	// Register 在写入插件表之前调用,
	Register() error
	// Apply 在依赖就绪后执行；期间通过 ctx 完成的一切注册归该 Fiber 所有，
	// 卸载时自动逆序回收。返回错误使 Fiber 进入 FAILED（已注册的半成品会被回收）。
	Apply(ctx *GoTenonContext, cfg any) error
	// Start 在装载完成后调用。
	Start() error
	// Run 是主动运行入口，作为「常驻功能」的实现，装载后调用。
	Run() error
	// DealWithMessage 处理发给本插件的消息。内核原封投递 Message，
	// 组件自行按 Type 解包 Data(不认识的消息类型应返回明确错误)。
	DealWithMessage(Message) error
	// End 在卸载前调用，负责资源回收与持久化。
	End() error
}

// Contextual 是可选接口：插件实现后，装载器会传入一个可取消的 context，
// 超时/取消时可协作退出（v0.2 协作式取消）。
// 未实现时装载器回退调用 Apply。
type Contextual interface {
	ApplyContext(ctx context.Context, gctx *GoTenonContext, cfg any) error
}

type FunctionOffer interface {
	//使用字符串描述的向系统提供的共有功能,同时包括功能调用
	Function() map[string]any //可以输入一些状态信息等,返回工具描述表,用于表示:在xxx状态下,xxx功能可用.
	ExecuteFunction(any)      //接收系统输入的参数,进行反序列化解码并执行工具
}
