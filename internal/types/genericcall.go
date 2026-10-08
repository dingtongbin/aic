package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// 泛型函数的调用点检查（T1 单态化：推断 → 实例化 → 登记实例）。
//
// §十 终止性前提：类型形参不得只出现在返回口，故每个 T 必须能从**实参类型**
// 解出；解不出 = 编译错，禁止发明隐式推断（否则单态化不终止）。
// ---------------------------------------------------------------------------

// checkGenericCallExplicit 检查**显式泛型实参**的调用 `id[i32](x)` / `id[Box[i32]](b)`。
// 与推断版共用同一套实例化（SubstSig + noteFuncInst + 约束校验），差别只在
// 类型实参来自调用点写的 `[...]`（而不是从实参类型反推）。
func (c *Checker) checkGenericCallExplicit(v *parse.Call, ix *parse.Index, sig *FuncSig, expect Type) (Type, bound, untyped) {
	// 类型实参个数：Index 是第一个，TypeArgs 是其余（`f[T, U](...)`）。
	argExprs := append([]parse.Expr{ix.Index}, ix.TypeArgs...)
	if len(argExprs) != len(sig.TypeParams) {
		c.errorAt(v.Pos, "wrong number of type arguments",
			sigName(sig)+" takes "+itoa(len(sig.TypeParams))+" type arguments, got "+itoa(len(argExprs)),
			"write "+sigName(sig)+"["+joinComma(sig.TypeParams)+"](...) (core design §4)")
		return nil, nilBound, untyped{}
	}
	m := map[string]Type{}
	for i, te := range argExprs {
		at := c.resolveTypeExpr(te)
		if at == nil {
			c.errorAt(v.Pos, "cannot resolve the type argument", exprText(te),
				"a type argument must be a type name (core design §4)")
			return nil, nilBound, untyped{}
		}
		m[sig.TypeParams[i]] = at
	}
	// N3：约束校验的位置 = 调用点（与推断版同一判据）。
	c.checkCallConstraints(v.Pos, sigName(sig), sig.Constraints, m)
	inst := SubstSig(sig, m)
	c.noteFuncInst(sig, m)
	c.noteCallTypeArgs(v, sig, m)
	return c.checkInvoke(v, inst, nil, bound{off: 0}, expect)
}


// noteCallTypeArgs 登记调用点的实例实参（按形参声明序）：单态化（mair）与 C 发端
// 都要按它决定给哪个实例发符号 —— 推断调用在 AIR 里看不出实参，必须由检查器带出来。
func (c *Checker) noteCallTypeArgs(v *parse.Call, sig *FuncSig, m map[string]Type) {
	args := make([]Type, 0, len(sig.TypeParams))
	for _, p := range sig.TypeParams {
		args = append(args, m[p])
	}
	c.callTypeArgs[v] = args
}

// resolveTypeExpr 把调用点的类型实参表达式解析成语义类型（Ident / pkg.Name / 嵌套 Index）。
func (c *Checker) resolveTypeExpr(e parse.Expr) Type {
	if te := typeExprOf(e); te != nil {
		return c.resolveType(te)
	}
	return nil
}

// checkGenericCall 检查一次泛型函数调用：推断实参 → 代换签名 → 常规调用检查。
func (c *Checker) checkGenericCall(v *parse.Call, sig *FuncSig, expect Type) (Type, bound, untyped) {
	if len(v.Args) != len(sig.ParamTypes) {
		c.errorAt(v.Pos, "wrong number of arguments",
			sigName(sig)+" needs "+itoa(len(sig.ParamTypes))+" arguments, got "+itoa(len(v.Args)),
			"match the signature: add or delete arguments")
		return nil, nilBound, untyped{}
	}
	// 先按**形参类型**检查实参（此时 T 还是 TypeParam，字面量按 T 落型会被拒），
	// 故这里先用无期望类型检查一遍取实参类型，再解 T。
	argTypes := make([]Type, 0, len(v.Args))
	for _, a := range v.Args {
		at, _, ainfo := c.checkExprFull(a, nil)
		if at == nil {
			at = c.literalDefault(ainfo, parse.ExprPos(a))
		}
		argTypes = append(argTypes, at)
	}
	m := InferArgs(sig, argTypes)
	if m == nil {
		c.errorAt(v.Pos, "cannot infer the type arguments",
			sigName(sig)+" from "+typeListText(argTypes),
			"a type parameter must be decidable from the argument types (core design §10); pass an explicitly typed value")
		return nil, nilBound, untyped{}
	}
	// N3：约束校验的位置 = **调用点**（不是模板体内部）。
	c.checkCallConstraints(v.Pos, sigName(sig), sig.Constraints, m)
	inst := SubstSig(sig, m)
	c.noteFuncInst(sig, m)
	c.noteCallTypeArgs(v, sig, m)
	return c.checkInvoke(v, inst, nil, bound{off: 0}, expect)
}

// noteMethodInst 登记一次**泛型类方法**的实例（args 按类形参声明序）。
// 方法本身不声明类型形参（§四：方法继承所属类的形参），故不能走 noteFuncInst 的
// "按 sig.TypeParams 取实参"那条路 —— 那条会得到空实参表（实例键 = `Box.get[]`，
// mair 就无从展开方法体）。
func (c *Checker) noteMethodInst(sig *FuncSig, args []Type) {
	if sig == nil || len(args) == 0 {
		return
	}
	key := InstKey(sig, nil) + "["
	for i, a := range args {
		if i > 0 {
			key += ","
		}
		key += a.String()
	}
	key += "]"
	if c.funcInstSeen == nil {
		c.funcInstSeen = map[string]bool{}
	}
	if c.funcInstSeen[key] {
		return
	}
	c.funcInstSeen[key] = true
	c.funcInsts = append(c.funcInsts, FuncInst{Fn: sig, Args: append([]Type{}, args...)})
}

// noteFuncInst 登记一次泛型函数实例（实例缓存：同键只记一次，红线 11）。
func (c *Checker) noteFuncInst(sig *FuncSig, m map[string]Type) {
	key := InstKey(sig, m)
	if c.funcInstSeen == nil {
		c.funcInstSeen = map[string]bool{}
	}
	if c.funcInstSeen[key] {
		return
	}
	c.funcInstSeen[key] = true
	args := make([]Type, 0, len(sig.TypeParams))
	for _, p := range sig.TypeParams {
		args = append(args, m[p])
	}
	c.funcInsts = append(c.funcInsts, FuncInst{Fn: sig, Args: args})
}

// typeListText 把类型表渲染成诊断文本。
func typeListText(ts []Type) string {
	out := ""
	for i, t := range ts {
		if i > 0 {
			out += ", "
		}
		out += typeOrUn(t, untyped{})
	}
	return out
}
