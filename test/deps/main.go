// Command deps 验证依赖图(成环、闭包顺序、引用计数、级联卸载、并行装载)。
//
// 运行:go run ./test/deps
package main

import (
	"os"

	"GoTenon/test/harness"
)

func main() { os.Exit(harness.Deps()) }
