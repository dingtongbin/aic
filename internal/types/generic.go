package types

import "strings"

// ---------------------------------------------------------------------------
// 泛型实例（R2 最小面：类型代换用于成员/变体访问；单态化与实例缓存 = 圆 1，
// 核心设计 §四——红线 11 在圆 1 落地）。
// ---------------------------------------------------------------------------

type Instance struct {
	Base Type // *Class 或 *Enum
	Args []Type
}

func (*Instance) typeNode() {}

func (t *Instance) String() string {
	var parts []string
	for _, a := range t.Args {
		parts = append(parts, a.String())
	}
	return t.Base.String() + "[" + strings.Join(parts, ", ") + "]"
}

// identical 对实例展开比较（同基类型同实参，type.go 的 identical 分派到此）。

// Subst 是 subst 的导出入口：emit 侧做实例负载/字段的类型代换（同一张实现，禁第二份）。
func Subst(t Type, params []string, args []Type) Type { return subst(t, params, args) }

// subst 把类型里的 TypeParam 按位置代换为实参。
func subst(t Type, params []string, args []Type) Type {
	if len(params) == 0 || t == nil {
		return t
	}
	switch v := t.(type) {
	case *TypeParam:
		for i, p := range params {
			if p == v.Name && i < len(args) {
				return args[i]
			}
		}
		return t
	case *Slice:
		return &Slice{Elem: subst(v.Elem, params, args)}
	case *MapT:
		return &MapT{Key: subst(v.Key, params, args), Value: subst(v.Value, params, args)}
	case *SetT:
		return &SetT{Elem: subst(v.Elem, params, args)}
	case *ArrayT:
		return &ArrayT{Elem: subst(v.Elem, params, args), N: v.N}
	case *FuncT:
		out := &FuncT{}
		for _, p := range v.Params {
			out.Params = append(out.Params, subst(p, params, args))
		}
		if v.Result != nil {
			out.Result = subst(v.Result, params, args)
		}
		return out
	case *Instance:
		out := &Instance{Base: v.Base}
		for _, a := range v.Args {
			out.Args = append(out.Args, subst(a, params, args))
		}
		return out
	}
	return t
}

// instClass 解包类实例：返回基类与类型形参/实参表。
func unwrapInstance(t Type) (Type, []string, []Type) {
	if inst, ok := t.(*Instance); ok {
		switch b := inst.Base.(type) {
		case *Class:
			return b, b.TypeParams, inst.Args
		case *Enum:
			return b, b.TypeParams, inst.Args
		}
	}
	return t, nil, nil
}

// ---------------------------------------------------------------------------
// 泛型函数单态化（T1）：调用点推断 + 实例登记（红线 11 实例缓存）。
//
// §十 的终止性前提：类型形参不得只出现在返回口 → 每个 T 必然能从**调用点实参**
// 的类型解出；解不出 = 编译错，**禁止发明隐式推断**。
// ---------------------------------------------------------------------------

// FuncInst 是一次泛型函数实例化（单态化的工作单元；键 = 函数名 + 实参类型）。
type FuncInst struct {
	Fn   *FuncSig
	Args []Type
}

// InferArgs 用实参类型解出类型形参（返回 nil = 有 T 解不出，调用方报错）。
func InferArgs(sig *FuncSig, argTypes []Type) map[string]Type {
	if sig == nil || len(sig.TypeParams) == 0 {
		return nil
	}
	m := map[string]Type{}
	for i, pt := range sig.ParamTypes {
		if i >= len(argTypes) {
			break
		}
		unifyTypeParam(pt, argTypes[i], m, 0)
	}
	for _, tp := range sig.TypeParams {
		if m[tp] == nil {
			return nil
		}
	}
	return m
}

// SubstSig 按类型参数映射代换签名（形参与返回位；recv 不参与）。
func SubstSig(sig *FuncSig, m map[string]Type) *FuncSig {
	if sig == nil || len(m) == 0 {
		return sig
	}
	params := sig.TypeParams
	args := make([]Type, 0, len(params))
	for _, p := range params {
		args = append(args, m[p])
	}
	out := &FuncSig{
		Name: sig.Name, Recv: sig.Recv, Export: sig.Export, Extern: sig.Extern,
		IsInit: sig.IsInit, TypeParams: sig.TypeParams, Derived: sig.Derived,
		Params: append([]string{}, sig.Params...),
	}
	for _, pt := range sig.ParamTypes {
		out.ParamTypes = append(out.ParamTypes, subst(pt, params, args))
	}
	for _, rt := range sig.Results {
		out.Results = append(out.Results, subst(rt, params, args))
	}
	return out
}

// unifyTypeParam 结构化地把 param 里的 TypeParam 绑定到 arg 的对应部分。
// 只做「解 T」这一件事：类型不匹配留给常规可赋值性检查报错（错误信息更好）。
func unifyTypeParam(param, arg Type, m map[string]Type, depth int) {
	if param == nil || arg == nil || depth > 8 {
		return
	}
	if tp, ok := param.(*TypeParam); ok {
		if m[tp.Name] == nil {
			m[tp.Name] = arg
		}
		return
	}
	if ps, ok := param.(*Slice); ok {
		if as, ok2 := arg.(*Slice); ok2 {
			unifyTypeParam(ps.Elem, as.Elem, m, depth+1)
		}
		return
	}
	if ps, ok := param.(*SetT); ok {
		if as, ok2 := arg.(*SetT); ok2 {
			unifyTypeParam(ps.Elem, as.Elem, m, depth+1)
		}
		return
	}
	if pm, ok := param.(*MapT); ok {
		if am, ok2 := arg.(*MapT); ok2 {
			unifyTypeParam(pm.Key, am.Key, m, depth+1)
			unifyTypeParam(pm.Value, am.Value, m, depth+1)
		}
		return
	}
	if pa, ok := param.(*ArrayT); ok {
		if aa, ok2 := arg.(*ArrayT); ok2 {
			unifyTypeParam(pa.Elem, aa.Elem, m, depth+1)
		}
		return
	}
	if pf, ok := param.(*FuncT); ok {
		if af, ok2 := arg.(*FuncT); ok2 {
			for i := range pf.Params {
				if i < len(af.Params) {
					unifyTypeParam(pf.Params[i], af.Params[i], m, depth+1)
				}
			}
			unifyTypeParam(pf.Result, af.Result, m, depth+1)
		}
		return
	}
	if pi, ok := param.(*Instance); ok {
		if ai, ok2 := arg.(*Instance); ok2 && identical(pi.Base, ai.Base) {
			for i := range pi.Args {
				if i < len(ai.Args) {
					unifyTypeParam(pi.Args[i], ai.Args[i], m, depth+1)
				}
			}
		}
	}
}

// substSigOf 按 (形参名, 实参) 代换一个方法签名（泛型类实例的方法调用）。
func substSigOf(sig *FuncSig, params []string, args []Type) *FuncSig {
	if sig == nil || len(params) == 0 {
		return sig
	}
	out := &FuncSig{
		Name: sig.Name, Recv: sig.Recv, Export: sig.Export, Extern: sig.Extern,
		IsInit: sig.IsInit, TypeParams: sig.TypeParams, Derived: sig.Derived,
		Params: append([]string{}, sig.Params...),
	}
	for _, pt := range sig.ParamTypes {
		out.ParamTypes = append(out.ParamTypes, subst(pt, params, args))
	}
	for _, rt := range sig.Results {
		out.Results = append(out.Results, subst(rt, params, args))
	}
	return out
}

// InstKey 是实例缓存的键（§十 10.2：键与符号名同源）。
func InstKey(sig *FuncSig, m map[string]Type) string {
	if sig == nil {
		return ""
	}
	out := sig.Name + "["
	for i, p := range sig.TypeParams {
		if i > 0 {
			out += ","
		}
		out += p + "=" + typeText(m[p])
	}
	return out + "]"
}
