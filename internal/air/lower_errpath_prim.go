// check / catch / assert / interp 的 IR-1 降低（§13.3 去糖封闭清单）。
//
// 这些形态在**表达式位**出现（`var x = check f()`、`f() catch { 0 }`、`f()!`、
// `"{x}"`），因此原语是 RHS（Let 的右值），而不是 Term。
//
// 共有的不变式（ir_prim.go）：发射原语后把 l.cur 切到新建的 After 块，pre 块的
// Term 由 desugar 填（check/catch）；assert / interp 无控制流，Term 原文保留。

package air

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// checkExpr（IR-1）：`check f()` → CheckRHS 原语。
func (l *lowerer) checkExpr(v *parse.CheckExpr) (string, error) {
	res, err := l.value(v.X)
	if err != nil {
		return "", err
	}
	mt := types.MultiElems(l.typeOf(v.X))
	if len(mt) < 2 {
		return "", fmt.Errorf("air: the check operand does not return multiple values (line %d)", v.Pos.Line)
	}
	after := l.newBlock("check", v.Pos)
	// 前置返回位的值名：check 表达式的值 = 末位之前那一项。仅 Err 的调用没有
	// 前置位（此时留空，desugar 给常量占位）。
	payload := ""
	if len(mt) > 1 {
		payload = l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: payload, Ty: l.ty(mt[0]),
			Rhs: &MultiExtract{Val: res, Idx: 0}, Loc: LocOf(v.Pos)})
	}
	res2 := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{
		Tmp: res2, Ty: l.ty(mt[0]),
		Rhs: &CheckRHS{
			Operand: res, Mt: l.tyList(mt), Payload: payload,
			After: after.Label, Loc: LocOf(v.Pos),
		},
		Loc: LocOf(v.Pos),
	})
	l.cur = after
	return res2, nil
}

// assertExpr（IR-1）：`expr!` → AssertRHS 原语（E-4 断言：出错即 trap）。
func (l *lowerer) assertExpr(v *parse.AssertExpr) (string, error) {
	res, payload, _, mt, err := l.errSplit(v.X, v.Pos)
	if err != nil {
		return "", err
	}
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{
		Tmp: t, Ty: l.tyOf(v),
		Rhs: &AssertRHS{
			Operand: res, Mt: l.tyList(mt), Payload: payload,
			Loc: LocOf(v.Pos),
		},
		Loc: LocOf(v.Pos),
	})
	return t, nil
}

// catchExpr（IR-1）：`expr catch e { … }` → CatchRHS 原语（E-3 就地处理）。
func (l *lowerer) catchExpr(v *parse.CatchExpr) (string, error) {
	res, _, errVal, mt, err := l.errSplit(v.X, v.Pos)
	if err != nil {
		return "", err
	}
	slot := l.tmp("catch")
	l.cur.Insts = append(l.cur.Insts, &Var{Name: slot, Ty: l.ty(mt[0]), Loc: LocOf(v.Pos)})

	// 处理块（IR-1 的 Body）：Err 绑定 + 用户处理体 + 写结果槽 + Br After。
	body := l.newBlock("catchbody", v.Pos)
	after := l.newBlock("catchjoin", v.Pos)
	saved := l.cur
	l.cur = body
	if v.Name != "" && v.Name != "_" {
		l.cur.Insts = append(l.cur.Insts, &Var{Name: v.Name,
			Ty: l.ty(mt[len(mt)-1]), Init: errVal, Loc: LocOf(v.Pos)})
	}
	exits := types.BlockAlwaysExits(v.Block)
	if err := l.catchBody(v.Block, slot, exits); err != nil {
		return "", err
	}
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: after.Label, Loc: LocOf(v.Pos)}
	}
	l.cur = saved

	res2 := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{
		Tmp: res2, Ty: l.ty(mt[0]),
		Rhs: &CatchRHS{
			Operand: res, Mt: l.tyList(mt), Bind: v.Name,
			Body: body.Label, Slot: slot, After: after.Label, Loc: LocOf(v.Pos),
		},
		Loc: LocOf(v.Pos),
	})
	l.cur = after
	return res2, nil
}

// interpLit（IR-1）：插值 → InterpRHS 原语（去糖期做拼接 + 逐分量格式化）。
//
// ValueTys 逐位给出占位符值的**类型文本**：泛型模板里它是 `T`，mono 替换成实参，
// 去糖才据已实参化的类型机械选格式化器。这正是"插值也必须是一等原语"的理由：
// 提前拼成 str 会丢掉类型，泛型函数里的插值在实例化后无从还原（§13.2）。
func (l *lowerer) interpLit(v *parse.InterpLit) (string, error) {
	if len(v.Texts) != len(v.Values)+1 {
		return "", fmt.Errorf("air: malformed interpolation (line %d)", v.Pos.Line)
	}
	vals := make([]string, 0, len(v.Values))
	tys := make([]string, 0, len(v.Values))
	for _, val := range v.Values {
		s, err := l.value(val)
		if err != nil {
			return "", err
		}
		// 走 typeOf（而非直查 info）：插值占位符常是**未定型字面量**
		// （`"{1000}"` / `"{0xff}"`），检查器不给它登记类型，typeOf 才有
		// 与 types.LiteralTy 同源的兜底（lower.go 的 typeOf 注释）。
		vt := l.typeOf(val)
		if vt == nil {
			return "", fmt.Errorf("air: interpolated %T has no recorded type (line %d)",
				val, v.Pos.Line)
		}
		if !types.InterpFormattable(vt) {
			return "", fmt.Errorf("air: type %s cannot be interpolated (line %d)", vt.String(), v.Pos.Line)
		}
		vals = append(vals, s)
		tys = append(tys, l.ty(vt))
	}
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{
		Tmp: t, Ty: "str",
		Rhs: &InterpRHS{
			Parts: append([]string{}, v.Texts...), Values: vals, ValueTys: tys,
			Loc: LocOf(v.Pos),
		},
		Loc: LocOf(v.Pos),
	})
	return t, nil
}

// mtTexts 取运算对象的多返回类型文本表（含末位 Err）。
func (l *lowerer) mtTexts(e parse.Expr) []string {
	mt := types.MultiElems(l.typeOf(e))
	if len(mt) == 0 {
		return []string{"Err"}
	}
	return l.tyList(mt)
}
