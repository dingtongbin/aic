package emit

import (
	"fmt"
	"strings"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 跨包调用（核心设计 §四 可见性、§十一 包管理器）。
//
// 调用点所在的包 ≠ 被调函数所在的包，故符号名必须用**被调包**的包名拼
// （c.pkg() 是调用者的包名，直接用会拼错）——mangling 单表照旧，只是喂对包名。
// ---------------------------------------------------------------------------

// depCall 发射跨包函数调用 util.f(...) → aic_<pkg>_<f>(...)。
func (c *Ctx) depCall(v *parse.Call, pkg, fn string, want types.Type) (string, bool, error) {
	dep := c.depInfo(pkg)
	if dep == nil {
		return "", false, nil
	}
	sig, ok := dep.Funcs[fn]
	if !ok || sig == nil {
		return "", true, fmt.Errorf("emit: no function %s in package %s (line %d)", pkg, fn, v.Pos.Line)
	}
	args := make([]string, 0, len(v.Args))
	for i := range v.Args {
		var wt types.Type
		if i < len(sig.ParamTypes) {
			wt = sig.ParamTypes[i]
		}
		a, err := c.arg(v, i, wt)
		if err != nil {
			return "", true, err
		}
		args = append(args, a)
	}
	return fmt.Sprintf("%s(%s)", MangleFunc(pkg, "", fn), strings.Join(args, ", ")), true, nil
}

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

// isUserPkg 报告 pkg 是否为本程序里的用户包（非 std）。
func (c *Ctx) isUserPkg(pkg string) bool { return c.depInfo(pkg) != nil }

// qualifiedVariant 解析限定变体构造的类型头：E.Variant / pkg.E.Variant。
func (c *Ctx) qualifiedVariant(field *parse.Field) (*types.Enum, types.Type, bool) {
	// E.Variant（本包枚举）
	if id, ok := field.X.(*parse.Ident); ok && c.Info != nil {
		if en, isEnum := c.Info.Enums[id.Name]; isEnum {
			if p, has := en.VariantPayload(field.Name); has {
				return en, p, true
			}
		}
	}
	// pkg.E.Variant（跨包枚举）
	if inner, ok := field.X.(*parse.Field); ok {
		if pid, isID := inner.X.(*parse.Ident); isID {
			dep := c.depInfo(pid.Name)
			if dep == nil {
				return nil, nil, false
			}
			if en, has := dep.Enums[inner.Name]; has {
				if p, has := en.VariantPayload(field.Name); has {
					return en, p, true
				}
			}
		}
	}
	return nil, nil, false
}

// compositeClassOf 解析复合字面量的类型头 → (类, 泛型实例或 nil)。
// 本包裸名 / 跨包限定名 pkg.C / 泛型实例 Box[i32] 三种形态。
func (c *Ctx) compositeClassOf(head parse.Expr) (*types.Class, *types.Instance) {
	switch h := head.(type) {
	case *parse.Ident:
		return c.recvClass(h.Name), nil
	case *parse.Field:
		id, ok := h.X.(*parse.Ident)
		if !ok {
			return nil, nil
		}
		if t := c.depNamedType(id.Name, h.Name); t != nil {
			if cl, isCl := types.IsClass(t); isCl {
				return cl, nil
			}
		}
		return nil, nil
	case *parse.Index:
		cl, _ := c.compositeClassOf(h.X)
		if cl == nil {
			return nil, nil
		}
		args := make([]types.Type, 0, 1+len(h.TypeArgs))
		for _, a := range append([]parse.Expr{h.Index}, h.TypeArgs...) {
			if at := c.typeOfTypeExpr(typeExprOfExpr(a)); at != nil {
				args = append(args, at)
			}
		}
		if len(args) != len(cl.TypeParams) {
			return cl, nil
		}
		return cl, &types.Instance{Base: cl, Args: args}
	}
	return nil, nil
}

// typeExprOfExpr 把表达式位的类型写法还原成类型表达式 —— **委托给 types 侧的唯一实现**
// （红线 10：同一条规则禁两处各写一份；此前 emit 抄的那份漏了嵌套 Index，
// `Box[Box[i32]]{…}` 因此发出未定义的 typedef 名）。
func typeExprOfExpr(e parse.Expr) parse.TypeExpr {
	return types.TypeExprOfExpr(e)
}
