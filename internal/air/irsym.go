package air

import (
	"strings"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// IR 侧符号名与类型文本的**唯一定义**（红线 10：同一条规则禁止两处实现）。
//
// 为什么放在 air：符号名是 IR 的属性（`.air` 文本里 `func <sym>` 就是它），
// 三处都要用同一条规则 ——
//   ① lower：给函数/实例定符号；
//   ② mono：实例复制的符号拼接；
//   ③ 后端（emit）：把检查产物里的签名与 IR 函数体对上。
// 任何一处自己拼一遍，都会在某个语料上静静地对不上（体丢失或错配）。
//
// 形态（不带 `aic_` 前缀；后端按需加，见 emit.MangleFunc）：
//
//	自由函数  <pkg>_<name>
//	方法      <pkg>_<Recv>_<name>
//	泛型实例  <pkg>_<name>[<T1>, <T2>]   ← 键与名同源：实参文本就是实例键
// ---------------------------------------------------------------------------

// FuncSym 是函数/方法在 IR 里的符号名（pkg 为空 = 不带包前缀）。
func FuncSym(pkg, recv, name string) string {
	switch {
	case pkg == "" && recv == "":
		return name
	case recv == "":
		return pkg + "_" + name
	case pkg == "":
		return recv + "_" + name
	}
	return pkg + "_" + recv + "_" + name
}

// FuncInstSym 是泛型函数/方法**实例**的符号名（Mono 单态化后每个实例各占一个符号）。
func FuncInstSym(pkg, recv, name string, args []types.Type) string {
	parts := make([]string, 0, len(args))
	for _, a := range args {
		parts = append(parts, TyText(a))
	}
	return FuncSym(pkg, recv, name) + "[" + strings.Join(parts, ", ") + "]"
}

// TyText 是类型在 IR 文本里的规范拼写（`tyText` 的导出形式：
// 后端拿它做「类型文本 → 语义类型」表的主键）。
func TyText(t types.Type) string { return tyText(t) }

// TypeSym 是具名类型在 IR 里的符号名（`<pkg>_<Name>`；后端加 `aic_` 前缀）。
func TypeSym(pkg, name string) string { return mangleType(pkg, name) }
