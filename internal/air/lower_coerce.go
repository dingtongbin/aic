package air

import (
	"strings"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 隐式转换的**显式化**（IR 收集完备性的一部分）。
//
// 语言层只有一条「值 → 槽位」的隐式转换：**具体类值 → 接口值**（核心设计 §十：
// 接口装箱 + 见证表）。C 没有这条转换（`aic_iface s = aic_ok_Rect *a;` 直接非法：
// "invalid initializer"），所以它必须在 IR 里显式成 `box <值>, <接口>`
// —— 后端只发 IR 里有的东西，**不让后端猜**（红线 10/23）。
//
// 734/787/788/760 实测：AIR 里只有 `var s : ok_Shape = a`，值名是具体类，C 层
// 无从下手。装箱的判据必须是"值的类型 ≠ 槽位类型而且两者是具体类/接口关系"，
// 因为**同一个值**在不同槽位上形态不同：
//
//	var a = Rect{w: 2, h: 5}   // a : ok_Rect*
//	var s Shape = a            // 槽位是接口 ⇒ box a, ok_Shape
//	var b Rect = a             // 槽位同型 ⇒ 原样
//
// 所以转换只能挂在"值流进某个有类型的槽位"的地方（var/赋值/返回/实参/字面量元素）。
// ---------------------------------------------------------------------------

// slotTextKnown 取"槽位类型文本"，**未知返回空串**（不猜）。
//
// 与 placeType/declType 的差别很重要：那两者给复合赋值兜底 `i32`（有意的窄化默认），
// 而隐式转换的判据绝不能拿兜底值当真 —— 把类值装进一个假的 `i32` 槽位会发出
// `box <Buf*>, i32`，verifier 立刻报 V5.3（"守卫存储的值不是引用"，779/748 实测）。
func (l *lowerer) slotTextKnown(e parse.Expr) string {
	if t := l.typeOf(e); t != nil {
		return l.ty(t)
	}
	return ""
}

// arrElemText 取数组字面量元素的类型文本（未知返回空串）。
func (l *lowerer) arrElemText(t types.Type) string {
	if t == nil {
		return ""
	}
	return l.ty(t)
}

// coerce 在值流进槽位时把隐式装箱显式化；不是隐式装箱就原样返回。
// slotText = 槽位的 AIR 类型文本（空 = 无类型信息，不猜）。
func (l *lowerer) coerce(val string, e parse.Expr, slotText string) string {
	if l.info == nil || slotText == "" || val == "" {
		return val
	}
	// 字面量/空值不是引用类值：不涉及装箱。
	if strings.HasPrefix(val, "const ") || val == "nil" {
		return val
	}
	src := l.typeOf(e)
	if src == nil {
		return val
	}
	// 接口 → 接口：C 层同型（`aic_iface`），收窄随 O/S 的见证表映射，不在这一层做。
	if _, isIfc := types.IsInterface(src); isIfc {
		return val
	}
	if !isRefClassValue(src) {
		return val
	}
	if l.ty(src) == slotText {
		return val // 同型：不是转换
	}
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: slotText,
		Rhs: &Box{Val: val, Iface: slotText}, Loc: LocOf(parse.ExprPos(e))})
	return t
}

// isRefClassValue 报告一个类型是不是"引用类值"（非 @packed 类 / 引用类实例 / @packed 类）。
// @packed 类也要装箱（C 侧按值拷进堆盒，见 emit 的 irBox），故两种类都算。
func isRefClassValue(t types.Type) bool {
	if _, ok := types.IsClass(t); ok {
		return true
	}
	if inst, ok := t.(*types.Instance); ok {
		_, isCl := types.IsClass(inst.Base)
		return isCl
	}
	return false
}

// coerceArgs 按被调形参表把实参逐个装箱（实参处是隐式装箱最常出现的地方：
// `describe(b)` 的 b 是具体类，describe 的形参是接口）。
func (l *lowerer) coerceArgs(argExprs []parse.Expr, vals []string, params []types.Type) []string {
	if len(params) == 0 {
		return vals
	}
	for i := range vals {
		if i >= len(params) || i >= len(argExprs) || params[i] == nil {
			continue
		}
		vals[i] = l.coerce(vals[i], argExprs[i], l.ty(params[i]))
	}
	return vals
}

// callParams 取被调签名的形参类型（与 callResults 同源；实参装箱的依据）。
func (l *lowerer) callParams(v *parse.Call) []types.Type {
	switch fn := v.Fn.(type) {
	case *parse.Ident:
		if sig, ok := l.info.Funcs[fn.Name]; ok {
			return sig.ParamTypes
		}
	case *parse.Field:
		if rt := l.typeOf(fn.X); rt != nil {
			if cl, isCl := types.IsClass(rt); isCl {
				if sig, ok := cl.Method(fn.Name); ok {
					return sig.ParamTypes
				}
			}
			if ifc, isIfc := types.IsInterface(rt); isIfc {
				return ifcSlotParams(ifc, fn.Name)
			}
		}
	}
	return nil
}

// ifcSlotParams 取接口槽位的形参类型（槽位表就是 `Methods[name].ParamTypes`）。
func ifcSlotParams(ifc *types.Interface, name string) []types.Type {
	if ifc.SlotOf(name) < 0 {
		return nil
	}
	sig, ok := ifc.Methods[name]
	if !ok {
		return nil
	}
	return sig.ParamTypes
}
