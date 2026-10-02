// Command capability 验证能力声明与内核治理(索引、发现、订阅、信号)。
//
// 运行:go run ./test/capability
package main

import (
	"os"

	"GoTenon/test/harness"
)

func main() { os.Exit(harness.Capability()) }
