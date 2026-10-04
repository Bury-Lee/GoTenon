// base.go —— 公共基座与工具。
//
// 提供 PluginInfo 的默认实现(basePlugin)与示例通用工具:
// 能力描述(cap)、槽位解析(service)、索引日志(logIndex)、JSON 输出(writeJSON)。
//
// 约定:组件在装载期(Apply/Start/Run)只经 ctx 槽位拿到依赖,绝不回调 Manager
// (装载期消费者持有 Manager 锁,回调会重入死锁)。Discover/Subscribe/Signal 等
// 内核演示一律由 main 在 Enable 之后调用。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"GoTenon"
)

// basePlugin 提供 PluginInfo 的默认实现,插件只覆写关心的钩子。
type basePlugin struct{}

func (basePlugin) Desc() map[string]string                  { return nil }
func (basePlugin) Inject() []string                         { return nil }
func (basePlugin) Status() map[string]any                  { return nil }
func (basePlugin) Register() error                          { return nil }
func (basePlugin) Apply(*GoTenon.GoTenonContext, any) error { return nil }
func (basePlugin) Start() error                             { return nil }
func (basePlugin) Run() error                               { return nil }
func (basePlugin) End() error                               { return nil }
func (basePlugin) DealWithMessage(GoTenon.Message) error    { return nil }

// statusOf 把 kv 包成 PluginInfo.Status 需要的指针,空值防护由调用方保证。
func statusOf(kv map[string]any) map[string]any { return kv }

// cap 构造 MCP 风格的能力描述(推荐但非强制)。
func cap(desc string, props ...string) map[string]any {
	properties := map[string]any{}
	required := make([]string, 0, len(props))
	for _, p := range props {
		properties[p] = map[string]any{"type": "string"}
		required = append(required, p)
	}
	return map[string]any{
		"description": desc,
		"inputSchema": map[string]any{"type": "object", "properties": properties, "required": required},
	}
}

// service 从上下文沿祖先链解析共享槽,拿不到返回错误。
func service(ctx *GoTenon.GoTenonContext, name string) (any, error) {
	slot := ctx.SlotOf(name)
	if slot == nil || slot.Value == nil {
		return nil, fmt.Errorf("required service %q unavailable", name)
	}
	return slot.Value, nil
}

// logIndex 是组件对索引通知消息(TypeIndex)的统一处理。
func logIndex(p string, m GoTenon.Message) bool {
	ev, ok := m.Data.(GoTenon.IndexEvent)
	if !ok {
		return false
	}
	fmt.Printf("[%s] 索引事件: %-6s %s\n", p, ev.Kind, ev.Plugin)
	return true
}

// writeJSON 统一 JSON 输出。
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
