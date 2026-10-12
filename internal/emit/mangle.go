package emit

import (
	"aic/internal/pkg"
)

// ---------------------------------------------------------------------------
// mangling 命名（**全仓唯一实现 = internal/pkg/mangle.go**，这里只是转发）。
//
// 形态：
//
//	export / extern  aic_<原名>            —— 原名直出（外部 ABI 的一部分，不能哈希）
//	内部 函数         aic_<16 个十六进制>    —— sha256(包身份路径::recv::name) 前 8 字节
//	内部 方法         aic_<16 hex>          —— 同上，recv 非空
//	内部 泛型实例      aic_<16 hex>          —— 实参后缀拼进哈希输入
//	内部 具名类型      aic_<16 hex>          —— sha256(包身份路径::Name)
//
// 为什么哈希见 pkg/mangle.go 的说明。补充三条这里的纪律：
//
//  1. 哈希不可逆 ⇒ `aic dump syms` 的对照表是**对价**（emit.Result.Syms），
//     不是可选项。崩溃现场看到 aic_9f3a... 必须能回到源码限定名。
//  2. `pkgPath` 是**包身份路径**（import 路径 / 入口模块名），不是目录名 ——
//     目录名在多模块工程里必然重复。见 Ctx.pkgPath / Ctx.depPath。
//  3. 运行期容器实例后缀**不走这里**（它不含 `aic_` 前缀、必须无下划线，
//     否则调用点符号 `chan_<后缀>_recv` 的按 `_` 切段会乱）——
//     那一条在 pkg.RuntimeSym，types.RuntimeSuffix 是它的唯一调用点。
// ---------------------------------------------------------------------------

// MangleFunc 是内部函数/方法的 C 符号。
func MangleFunc(pkgPath, recv, name string) string { return pkg.FuncSym(pkgPath, recv, name) }

// mangleType 是内部具名类型的 C typedef 名。
func mangleType(pkgPath, name string) string { return pkg.TypeSym(pkgPath, name) }

// MangleGeneric 是泛型实例的符号：实参后缀拼进哈希输入（同一实参集 → 同一符号）。
func MangleGeneric(pkgPath, name string, argSuffixes []string) string {
	return pkg.GenericSym(pkgPath, name, argSuffixes)
}

// ExternName 是 extern/export 的 C 名（原名直出，不 mangle）。
func ExternName(name string) string { return name }
