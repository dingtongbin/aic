package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// 泛型函数的调用点检查（T1 单态化：推断 → 实例化 → 登记实例）。
//
// 终止性前提：类型形参不得只出现在返回口，故每个 T 必须能从**实参类型**
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
	// 约束校验的位置 = 调用点（与推断版同一判据）。
	c.checkCallConstraints(v.Pos, sigName(sig), sig.Constraints, m)
	inst := SubstSig(sig, m)
	c.noteFuncInst(sig, m)
	c.noteCallTypeArgs(v, sig, m)
	return c.checkInvoke(v, inst, nil, bound{off: 0}, expect)
}

// checkGenericStaticCall 处理 `Box[i64].init(42)`：泛型类的实例化 + 静态方法调用。
// 实例化点 = 约束校验点（《AIC 语言规范》§6.1：约束不满足报在调用点/实例化点）。
func (c *Checker) checkGenericStaticCall(v *parse.Call, ix *parse.Index, cl *Class, method string, expect Type) (Type, bound, untyped) {
	// 1. 解析类型实参（Index 是第一个，TypeArgs 是其余）
	argExprs := append([]parse.Expr{ix.Index}, ix.TypeArgs...)
	if len(argExprs) != len(cl.TypeParams) {
		c.errorAt(v.Pos, "wrong number of type arguments",
			cl.Name+" takes "+itoa(len(cl.TypeParams))+" type arguments, got "+itoa(len(argExprs)),
			"write "+cl.Name+"["+joinComma(cl.TypeParams)+"].init(...)")
		return nil, nilBound, untyped{}
	}
	m := map[string]Type{}
	for i, te := range argExprs {
		at := c.resolveTypeExpr(te)
		if at == nil {
			c.errorAt(v.Pos, "cannot resolve the type argument", exprText(te),
				"a type argument must be a type name")
			return nil, nilBound, untyped{}
		}
		m[cl.TypeParams[i]] = at
	}
	// 2. 约束检查在**实例化点**（不是声明处）
	c.checkCallConstraints(v.Pos, cl.Name, cl.Constraints, m)
	// 3. 找静态方法（init / NoRecv 方法），按实参代换
	sig, has := cl.Method(method)
	if !has {
		c.errorAt(v.Pos, "the generic class has no such method", cl.Name+"."+method,
			"type-level calls on a generic class are init(...) or a no-receiver method (AIC language spec §6.1)")
		return nil, nilBound, untyped{}
	}
	inst := SubstSig(sig, m)
	c.noteClassInst(cl, m)
	c.noteCallTypeArgs(v, sig, m)
	// 方法不新增类型形参（§6.1），它的 T 来自**类**的 TypeParams ——
	// SubstSig 只看 sig.TypeParams（方法上为空）会原样返回，故这里用类的
	// 形参表再代换一次，让 `func init(v T) -> Box[T]` 的返回变成 Box[i64]。
	if len(cl.TypeParams) > 0 {
		cargs := make([]Type, 0, len(cl.TypeParams))
		for _, p := range cl.TypeParams {
			cargs = append(cargs, m[p])
		}
		inst.Results = substResults(inst.Results, cl.TypeParams, cargs)
		inst.ParamTypes = substTypes(inst.ParamTypes, cl.TypeParams, cargs)
	}
	// 结果类型 = 实例化后的类（Instance 而非 Class），
	// 这样 `var bi Box[i64] = Box[i64].init(42)` 类型能对上
	res := inst.Results
	if len(res) > 0 {
		if st, ok := res[0].(*Class); ok {
			res = []Type{&Instance{Base: st, Args: typeArgsOf(st.TypeParams, m)}}
		}
	}
	inst.Results = res
	return c.checkInvoke(v, inst, nil, bound{off: 0}, expect)
}

// substTypes / substResults 按类的类型形参表代换（方法体的 T 归属类）。
func substTypes(ts []Type, params []string, args []Type) []Type {
	out := make([]Type, 0, len(ts))
	for _, t := range ts {
		out = append(out, subst(t, params, args))
	}
	return out
}

func substResults(ts []Type, params []string, args []Type) []Type {
	return substTypes(ts, params, args)
}

// typeArgsOf 按形参声明序取实参。
func typeArgsOf(params []string, m map[string]Type) []Type {
	out := make([]Type, 0, len(params))
	for _, p := range params {
		out = append(out, m[p])
	}
	return out
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
	// 约束校验的位置 = **调用点**（不是模板体内部）。
	c.checkCallConstraints(v.Pos, sigName(sig), sig.Constraints, m)
	inst := SubstSig(sig, m)
	c.noteFuncInst(sig, m)
	c.noteCallTypeArgs(v, sig, m)
	return c.checkInvoke(v, inst, nil, bound{off: 0}, expect)
}

// noteMethodInst 登记一次**泛型类方法**的实例（args 按类形参声明序）。
// 方法本身不声明类型形参（方法继承所属类的形参），故不能走 noteFuncInst 的
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

// noteFuncInst 登记一次泛型函数实例（实例缓存：同键只记一次）。
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

// noteClassInst 登记泛型类的实例（供单态化与 C 发端决定发哪个实例符号）。
// 与 noteFuncInst 同族：键 = 类名 + 类型实参，去重后进 funcInsts 同一条队列
// （实例化的是类的成员函数，最终都由函数级实例承载）。
func (c *Checker) noteClassInst(cl *Class, m map[string]Type) {
	// Methods 是 map[string]*FuncSig；逐个登记录（泛型类的每个成员函数都要实例化）。
	for _, mm := range cl.Methods {
		c.noteFuncInst(mm, m)
	}
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
