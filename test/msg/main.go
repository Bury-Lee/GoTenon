// Command msg 验证消息系统(类型化信封、路由、异常隔离、系统分支)。
//
// 运行:go run ./test/msg
package main

import (
	"os"

	"GoTenon/test/harness"
)

func main() { os.Exit(harness.Messages()) }
