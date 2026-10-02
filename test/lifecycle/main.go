// Command lifecycle 验证组件加载(登记、生命周期钩子、失败回滚、超时、更新、删除)。
//
// 运行:go run ./test/lifecycle
package main

import (
	"os"

	"GoTenon/test/harness"
)

func main() { os.Exit(harness.Lifecycle()) }
