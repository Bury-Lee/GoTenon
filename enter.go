package GoTenon

// PluginManager 是包级默认管理器：Run 成功后写入，宿主经此访问。
var PluginManager Manager

// Run 启动默认核心调度器：创建根上下文与 Manager，注册插件并驱动到收敛。
//
// 这是「独自启动」的入口；宿主进程内嵌时直接用 NewManager。
// 默认启用全部已注册插件；WithEnabled 可只启用指定插件（依赖仍会自动装载）。
func Run(opts ...Option) error {
	cfg := runConfig{}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	m := NewManager(cfg.root)
	m.Loader = cfg.loader
	m.Logger = cfg.logger
	for _, entry := range cfg.plugins {
		if _, err := m.Register(entry.plugin, entry.cfg); err != nil {
			return err
		}
	}
	// 未指定启用名单时启用全部已注册插件
	names := cfg.enabled
	if len(names) == 0 {
		names = m.names()
	}
	for _, name := range names {
		if err := m.Enable(name); err != nil {
			return err
		}
	}
	PluginManager = *m
	return nil
}

// Option 定制一次 Run。
type Option func(*runConfig)

type runConfig struct {
	root    *GoTenonContext
	loader  Loader
	logger  Logger
	plugins []pluginEntry
	enabled []string
}

type pluginEntry struct {
	plugin PluginInfo
	cfg    any
}

// WithRoot 指定根上下文（宿主已有上下文树时使用）。
func WithRoot(ctx *GoTenonContext) Option {
	return func(c *runConfig) { c.root = ctx }
}

// WithLoader 挂载 Loader 扩展钩子。
func WithLoader(l Loader) Option {
	return func(c *runConfig) { c.loader = l }
}

// WithLogger 挂载框架日志出口。
func WithLogger(l Logger) Option {
	return func(c *runConfig) { c.logger = l }
}

// WithPlugin 注册一个带配置的插件。
func WithPlugin(p PluginInfo, cfg any) Option {
	return func(c *runConfig) { c.plugins = append(c.plugins, pluginEntry{plugin: p, cfg: cfg}) }
}

// WithPlugins 批量注册无配置插件。
func WithPlugins(ps ...PluginInfo) Option {
	return func(c *runConfig) {
		for _, p := range ps {
			c.plugins = append(c.plugins, pluginEntry{plugin: p})
		}
	}
}

// WithEnabled 指定要启用的插件名；不指定则启用全部已注册插件。
func WithEnabled(names ...string) Option {
	return func(c *runConfig) { c.enabled = append(c.enabled, names...) }
}
