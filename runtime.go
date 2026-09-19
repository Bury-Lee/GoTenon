// runtime.go —— 插件运行时状态：插件信息表与单个插件的运行时快照。
//
// PluginTable 是插件的唯一注册处；PluginRuntime 承载依赖边、启用意图、
// 生命周期状态、上下文与配置，由 Manager 按插件名索引，状态由调度者统一写回。

package GoTenon

// PluginTable 插件信息表：所有已注册的插件都在此保留信息（唯一注册处）。
type PluginTable struct {
	Info map[string]PluginInfo
}

// State 是插件的生命周期状态，由调度者统一写回；启用意图由 Enable 字段单独表达。
//
//	Disabled   未启用且未装载
//	Pending    已启用，等待依赖就绪（依赖装载中）
//	Loading    正在装载：Apply → Start → Run
//	Ready      装载完成，服务就绪
//	Failed     装载失败（原因见 Err），Update / Enable 可重试
//	Kept       未启用但被依赖者持有，保持装载
//	Unloading  正在卸载：End → 逆序回收
//
// 典型流转：
//
//	Disabled --Enable--> Pending --依赖就绪--> Loading --成功--> Ready
//	                        ↑                    └--失败--> Failed
//	                        └--仍启用-- Unloading <--Disable / Update--
type State int8

const (
	Disabled  State = iota // 未启用且未装载
	Pending                // 已启用，等待依赖就绪
	Loading                // 正在装载
	Ready                  // 装载完成，服务就绪
	Failed                 // 装载失败
	Kept                   // 未启用但被依赖者持有
	Unloading              // 正在卸载
)

// String 返回状态名，便于日志与诊断输出。
func (s State) String() string {
	switch s {
	case Disabled:
		return "Disabled"
	case Pending:
		return "Pending"
	case Loading:
		return "Loading"
	case Ready:
		return "Ready"
	case Failed:
		return "Failed"
	case Kept:
		return "Kept"
	case Unloading:
		return "Unloading"
	default:
		return "Unknown"
	}
}

// PluginRuntime 是 Manager 持有的单个插件运行时。
//
// Dependence/Dependenced 是插件级依赖边：Inject() 返回的名字按插件名解析，
// 注册时由 Manager 自动建边。边必须用指针：状态是同一份，用值会在 append 时
// 复制出互不相干的副本。
//
// 卸载规则：未被启用且无已装载依赖者的插件会被自动卸载，上下文一并清理。
type PluginRuntime struct {
	Plugin  PluginInfo      // 插件本体（身份）
	Context *GoTenonContext // 挂载上下文；卸载时清理
	Config  any             // 当前配置：Apply 的入参，Update 可替换
	Enable  bool            // 启用意图：Enable/Disable 设置
	State   State           // 生命周期状态：调度者统一写回
	Err     error           // 最近一次装载失败原因；非 nil 时保持 Failed，不再自动重试
	Missing []string        // 当前未就绪的依赖（未注册或未装载），诊断用

	Dependence  []*PluginRuntime // 依赖
	Dependenced []*PluginRuntime // 被什么依赖
}

// Loaded 报告插件是否持有已装载实例：Ready / Kept / Unloading 为真。
func (rt *PluginRuntime) Loaded() bool {
	switch rt.State {
	case Ready, Kept, Unloading:
		return true
	default:
		return false
	}
}

// settle 在收敛完成后校正状态：
// 已启用且已装载 → Ready；未启用但保持装载 → Kept；未启用且未装载 → Disabled。
func (rt *PluginRuntime) settle() {
	if rt.Enable {
		if rt.Loaded() {
			rt.State = Ready
		}
		return
	}
	if rt.Loaded() {
		rt.State = Kept
	} else {
		rt.State = Disabled
		rt.Err = nil
	}
}
