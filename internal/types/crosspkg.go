package types

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// 跨包名解析（核心设计 §四 可见性、§十一 包管理器）。
//
// 目录即包：`import util` 把 util 目录里的包级名带进来，用 `util.Name` 限定访问。
// 可见性（核心设计 §四）：**顶层名首字符是 Unicode 大写字母 = 跨包可见**；
// 小写开头与 `_` 前缀一律包私有（`_Name` 也私有 —— 前导下划线不构成公开）。
// 成员名（字段/方法）：随类型可见性 —— 类型跨包可见则其非 `_` 成员跨包可访问。
//
// 唯一入口：depInfo / depType / depFunc / depConst —— 禁在检查器别处再写一份
// 「去 deps 里翻表」的逻辑（红线 10）。
// ---------------------------------------------------------------------------

// exported 报告名字是否跨包可见（§四：首字符 Unicode 大写 = 公开）。
// 唯一的判定入口：checker 与 emit 都走这里（红线 10：禁第二份）。
func exported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

// memberVisible 判定**跨包成员访问**：`_` 前缀成员是包私有，跨包一律拒（R7 决策一：
// 成员不按大小写判定，但 `_` 前缀仍是私有 —— 类型公开不等于它的 `_` 成员公开）。
// 返回 false 表示已报错，调用方直接返回。
func (c *Checker) memberVisible(ownerPkg, ownerName, member string, at parse.Pos) bool {
	if ownerPkg == "" || ownerPkg == c.pkg {
		return true // 本包：`_` 成员照常可用
	}
	if !strings.HasPrefix(member, "_") {
		return true
	}
	c.errorAt(at, "a package-private member cannot be used across packages", ownerName+"."+member,
		"the `_` prefix keeps a member inside its package; use a public member or a method on "+ownerName+" (core design §4 visibility)")
	return false
}

// depInfo 取已检查完的 import 包 Info（未收集到 = nil，调用方转 std 路径或报错）。
func (c *Checker) depInfo(name string) *Info {
	if c.deps == nil {
		return nil
	}
	return c.deps[name]
}

// depType 在 import 包里查具名类型（含泛型实参）。
// 找不到返回 nil（调用方决定报「包内无此类型」还是转 std 面）。
func (c *Checker) depType(pkg, name string, args []parse.TypeExpr, at parse.Pos) Type {
	dep := c.depInfo(pkg)
	if dep == nil {
		return nil
	}
	base := depNamedType(dep, name)
	if base == nil {
		return nil
	}
	if !exported(name) {
		c.errorAt(at, "a package-private type cannot be used across packages", pkg+"."+name,
			"capitalise the first letter to make it visible across packages (core design §4 visibility)")
		return nil
	}
	if len(args) == 0 {
		return base
	}
	// 泛型实例：实参类型写在导入方（可能引用本包类型），故用本包检查器解析。
	return c.instantiate(base, args, at)
}

// depNamedType 是「包 Info + 名字 → 具名类型」的唯一实现。
func depNamedType(dep *Info, name string) Type {
	if dep == nil {
		return nil
	}
	if cl, ok := dep.Classes[name]; ok {
		return cl
	}
	if en, ok := dep.Enums[name]; ok {
		return en
	}
	if ifc, ok := dep.Interfaces[name]; ok {
		return ifc
	}
	return nil
}

// depFunc 在 import 包里查自由函数签名（含跨包可见性判定）。
func (c *Checker) depFunc(pkg, name string, at parse.Pos) (*FuncSig, bool) {
	dep := c.depInfo(pkg)
	if dep == nil {
		return nil, false
	}
	sig, ok := dep.Funcs[name]
	if !ok {
		return nil, false
	}
	if !exported(name) {
		c.errorAt(at, "a package-private function cannot be called across packages", pkg+"."+name,
			"capitalise the first letter to make it visible across packages (core design §4 visibility)")
		return nil, true
	}
	return sig, true
}

// depConst 在 import 包里查常量（跨包可见性判定同上）。
func (c *Checker) depConst(pkg, name string, at parse.Pos) (*Symbol, bool) {
	dep := c.depInfo(pkg)
	if dep == nil {
		return nil, false
	}
	sym, ok := dep.Consts[name]
	if !ok {
		return nil, false
	}
	if !exported(name) {
		c.errorAt(at, "a package-private constant cannot be used across packages", pkg+"."+name,
			"capitalise the first letter to make it visible across packages (core design §4 visibility)")
		return nil, true
	}
	return sym, true
}

// checkDepCall 类型化跨包函数调用 util.f(args)。
func (c *Checker) checkDepCall(v *parse.Call, pkg, fn string, expect Type) (Type, bound, untyped) {
	sig, found := c.depFunc(pkg, fn, v.Pos)
	if !found {
		dep := c.depInfo(pkg)
		if dep != nil {
			if depNamedType(dep, fn) != nil {
				c.errorAt(v.Pos, "a type cannot be called as a function", pkg+"."+fn,
					"construct with "+pkg+"."+fn+"{...}; a type name takes no argument list")
				return nil, nilBound, untyped{}
			}
		}
		c.errorAt(v.Pos, "no such function in the package", pkg+"."+fn,
			"check the spelling; across packages only top-level names starting with an uppercase letter are visible (core design §4)")
		return nil, nilBound, untyped{}
	}
	if sig == nil {
		return nil, nilBound, untyped{}
	}
	return c.checkInvoke(v, sig, nil, bound{off: 0}, expect)
}

// depVariant 在 import 包的枚举里查变体（pkg.Enum.Variant 的第二段）。
func (c *Checker) depVariant(pkg, enumName, variant string, at parse.Pos) (*Enum, Variant, bool) {
	dep := c.depInfo(pkg)
	if dep == nil {
		return nil, Variant{}, false
	}
	en, ok := dep.Enums[enumName]
	if !ok || !exported(enumName) {
		return nil, Variant{}, false
	}
	idx, ok := en.variantI[variant]
	if !ok {
		return nil, Variant{}, false
	}
	return en, en.Variants[idx], true
}
