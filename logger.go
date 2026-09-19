package GoTenon

// logger.go —— 框架日志出口：记录装载/卸载与超时，未设置 Logger 时静默。

import "fmt"

// Level 日志级别。
type Level uint8

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Logger 是框架日志出口；为 nil 时静默。
type Logger interface {
	Log(level Level, msg string)
}

// LoggerFunc 把函数适配为 Logger。
type LoggerFunc func(level Level, msg string)

// Log 实现 Logger。
func (f LoggerFunc) Log(level Level, msg string) { f(level, msg) }

// logf 记录一条带 [GoTenon] 头的框架日志；无 Logger 时静默。
func (m *Manager) logf(level Level, format string, args ...any) {
	if m.Logger == nil {
		return
	}
	m.Logger.Log(level, "[GoTenon] "+fmt.Sprintf(format, args...))
}
