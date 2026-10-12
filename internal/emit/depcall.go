package emit

import (
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 跨包调用（核心设计 §四 可见性、§十一 包管理器）。
//
// 调用点所在的包 ≠ 被调函数所在的包，故符号名必须用**被调包**的包名拼
// （c.pkg() 是调用者的包名，直接用会拼错）——mangling 单表照旧，只是喂对包名。
// ---------------------------------------------------------------------------

// depFuncResults 取跨包函数的结果类型（emit 定宽/选打印原语用）。
func (c *Ctx) depFuncResults(pkg, fn string) ([]types.Type, bool) {
	dep := c.depInfo(pkg)
	if dep == nil {
		return nil, false
	}
	sig, ok := dep.Funcs[fn]
	if !ok || sig == nil {
		return nil, false
	}
	return sig.Results, true
}

// depNamedType 取跨包具名类型（类型位 util.Reader / util.Box[i32] 用）。
func (c *Ctx) depNamedType(pkg, name string) types.Type {
	dep := c.depInfo(pkg)
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
