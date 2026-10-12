package pkg

import "strings"

// SymbolPath 把包身份路径折成「符号可用片段」：`/` `.` `-` 一律变 `_`。
//
// 为什么需要它：包身份 = import 路径（`app/core`），而符号同时出现在 IR 文本
// 与 C 标识符里 —— 不能含路径分隔符。折法必须**唯一实现**：air 造符号、emit
// 反解符号前缀都要用同一套，否则 `app/core` 一边折成 `app_core`、另一边还按
// 原样找不到，跨包类型在编译期静默失配。
func SymbolPath(pkgPath string) string {
	if pkgPath == "" {
		return pkgPath
	}
	return strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(pkgPath)
}
