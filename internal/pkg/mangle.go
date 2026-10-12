package pkg

import (
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// 符号命名（**全仓唯一实现**）。
//
// 为什么必须收在一处：符号出现在三层，各层拼一遍必然在某个语料上对不上 ——
//  ① air 造 IR 符号（可读，`dump-ir` / `aic dump syms` 左边那一列）；
//  ② emit 造 C 符号（哈希，定长合法标识符）；
//  ③ types 造**运行期容器实例后缀**（`chan_<后缀>`，`_recv` 接在后面）。
// 三者的**命名依据完全相同**：包身份路径 + 名字（+ 接收者 / 泛型实参）。
//
// 为什么 C 符号是哈希而不是 `aic_<pkg>_<name>`：包名（目录名）在多模块工程里
// 必然重复（每个模块都有 `util`），拼出来会撞名 —— 链接撞符号、缓存键互相污染、
// `aic dump syms` 无法区分。包**身份路径**（import 路径 / 入口模块名）全局唯一。
// 哈希不可逆 ⇒ 必须有反向表（`aic dump syms`），那是哈希的对价，不是可选项。
//
// 为什么运行期后缀是**定长十六进制**（不带 `aic_` 前缀）：调用点符号形如
// `chan_<后缀>_recv`，解析方按 `_` 切段；`aic_` 自带一个下划线会把段数打乱。
// ---------------------------------------------------------------------------

const symLen = 16

// FuncSym 是内部函数/方法的 C 符号。
func FuncSym(pkgPath, recv, name string) string {
	return "aic_" + Hash("aic-func-v1", pkgPath, recv, name)[:symLen]
}

// TypeSym 是内部具名类型的 C 名（typedef）。
func TypeSym(pkgPath, name string) string {
	return "aic_" + Hash("aic-type-v1", pkgPath, name)[:symLen]
}

// GenericSym 是泛型实例的 C 名（同一实参集 ⇒ 同一符号）。
func GenericSym(pkgPath, name string, args []string) string {
	parts := append([]string{"aic-gen-v1", pkgPath, name}, args...)
	return "aic_" + Hash(parts...)[:symLen]
}

// RuntimeSym 是运行时容器/通道实例的后缀：定长十六进制、**不含下划线**。
// 元素是 struct（值类型，按 C struct 布局）时，容器实例按类型唯一一份。
func RuntimeSym(pkgPath, name string) string {
	return Hash("aic-rt-v1", pkgPath, name)[:symLen]
}

// QualPath 是可读的 IR/AIC 限定名（`aic dump syms` 左列）。
// 把包身份路径折成符号片段后接上接收者/名字 —— 与 SymbolPath 同一套折法。
func QualPath(pkgPath, recv, name string) string {
	p := SymbolPath(pkgPath)
	switch {
	case p != "" && recv != "":
		return p + "_" + recv + "_" + name
	case p != "":
		return p + "_" + name
	case recv != "":
		return recv + "_" + name
	}
	return name
}

// SortQuals 把限定名排序（诊断与符号表必须确定性可复现，H2）。
func SortQuals(s []string) { sort.Strings(s) }

// JoinArgs 把泛型实参文本拼成一段（实例键的实参段；空列表 = 空串）。
func JoinArgs(args []string) string { return strings.Join(args, "_") }
