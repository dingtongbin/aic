package emit

import (
	"aic/internal/pkg"
)

// ---------------------------------------------------------------------------
// mangling 命名（**全仓唯一实现**，见 emit_test.go 的表驱动断言）。
//
// 形态：
//
//	export / extern  aic_<原名>            —— 原名直出（外部 ABI 的一部分，不能哈希）
//	内部 函数         aic_<16 个十六进制>    —— sha256(包身份路径::recv::name) 前 8 字节
//	内部 方法         aic_<16 hex>          —— 同上，recv 非空
//	内部 泛型实例      aic_<16 hex>          —— 实参后缀拼进哈希输入
//	内部 具名类型      aic_<16 hex>          —— sha256(包身份路径::Name)
//
// 为什么哈希而不是 `aic_<pkg>_<name>`：包名（目录名）在多模块工程里必然重复
// （每个模块都有 `util`），拼出来会撞名 —— 链接期撞符号、缓存键互相污染、
// `aic dump syms` 无法区分。包**身份路径**（import 路径 / 入口模块名）全局唯一，
// 哈希把任意长度、任意字符的路径压成定长合法 C 标识符。
//
// 哈希不可逆：`aic dump syms` 输出反向表（qualified → C 符号），崩溃现场的符号名
// 才能回到源码。这张表是哈希代价的对价，不是可选项。
// ---------------------------------------------------------------------------

// qualifiedOf 是哈希输入：包身份路径 :: recv :: name（\x00 分隔由 pkg.Hash 保证）。
func qualifiedOf(pkgPath, recv, name string) []string {
	return []string{"aic-sym-v1", pkgPath, recv, name}
}

// MangleFunc 是内部函数/方法的 C 符号。
// pkgPath = 包**身份路径**（不是目录名）；recv = 接收者类名（自由函数为空）。
func MangleFunc(pkgPath, recv, name string) string {
	return "aic_" + pkg.Hash(qualifiedOf(pkgPath, recv, name)...)[:16]
}

// mangleType 是内部具名类型的 C typedef 名。
func mangleType(pkgPath, name string) string {
	return "aic_" + pkg.Hash("aic-ty-v1", pkgPath, name)[:16]
}

// MangleGeneric 是泛型实例的符号：实参后缀拼进哈希输入（同一实参集 → 同一符号）。
func MangleGeneric(pkgPath, name string, argSuffixes []string) string {
	parts := append([]string{"aic-gen-v1", pkgPath, name}, argSuffixes...)
	return "aic_" + pkg.Hash(parts...)[:16]
}

// ExternName 是 extern/export 的 C 名（原名直出，不 mangle）。
func ExternName(name string) string { return name }
