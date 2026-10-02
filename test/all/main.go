// Command all 一次性运行全部分类验证并汇总。
//
// 运行:go run ./test/all
package main

import (
	"fmt"
	"os"

	"GoTenon/test/harness"
)

func main() {
	failed := 0
	failed += harness.Messages()
	failed += harness.Lifecycle()
	failed += harness.Deps()
	failed += harness.Capability()
	if failed > 0 {
		fmt.Printf("\n合计失败 %d 项\n", failed)
	} else {
		fmt.Println("\n全部通过")
	}
	os.Exit(failed)
}
