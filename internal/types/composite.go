package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// 复合字面量 / 列表字面量 / lambda / match 表达式 / check（核心设计 §三/§四/§六）。
// ---------------------------------------------------------------------------

// checkComposite: C{f: v}——无 init 类的构造（核心设计 §四）。
// 类型头可以是裸类名 C、跨包限定名 pkg.C，或泛型实例 Box[i32]（§四 可见性 + 泛型）。
func (c *Checker) checkComposite(v *parse.CompositeLit, expect Type) (Type, bound, untyped) {
	cl, inst := c.compositeClass(v)
	if cl == nil {
		return nil, nilBound, untyped{}
	}
	if init := c.classInit(cl); init != nil {
		c.errorAt(v.Pos, "this class has an init constructor, so composite literals are disabled", cl.Name+"{…}",
			"rewrite it as a constructor call: "+cl.Name+"(args) (core design §4)")
		return nil, nilBound, untyped{}
	}
	seen := map[string]bool{}
	for _, f := range v.Fields {
		idx, exists := cl.fieldIdx[f.Name]
		if !exists {
			c.errorAt(f.Pos, "the class has no such field", cl.Name+"."+f.Name,
				"see the class declaration for its fields; unlisted fields take their zero value")
			continue
		}
		if seen[f.Name] {
			c.errorAt(f.Pos, "duplicate field initialisation", cl.Name+"."+f.Name, "write each field once")
			continue
		}
		seen[f.Name] = true
		ft := cl.Fields[idx].Type
		if inst != nil {
			// 泛型实例的字段类型按实例实参代换（T → i32）。
			ft = subst(ft, cl.TypeParams, inst.Args)
		}
		fv, _, finfo := c.checkExprFull(f.Value, ft)
		c.requireAssignableAt(f.Value, fv, finfo, ft, "field "+f.Name)
	}
	if inst != nil {
		return inst, bound{off: c.regionOff, exact: true}, untyped{}
	}
	return cl, bound{off: c.regionOff, exact: true}, untyped{}
}

// TypeExprOfExpr 是 typeExprOf 的导出形态：emit 侧的复合字面量/泛型实例解析
// 必须用**同一套**规则（曾经 emit 自己抄了一份、漏了嵌套 Index ⇒
// `Box[Box[i32]]{…}` 发出未定义的 typedef 名）。
func TypeExprOfExpr(e parse.Expr) parse.TypeExpr { return typeExprOf(e) }

// typeExprOf 把表达式位的类型写法还原成类型表达式（`Box[i32]` 里的 `i32` 在
// 表达式位是 Ident，类型位是 NamedType；两者指同一个名字）。
// **嵌套泛型**（`Box[Box[i32]]`）在表达式位是 Index，必须递归还原 —— 此前不认
// Index ⇒ `id[Box[i32]](b)` 报"无法解析类型实参"、复合字面量静默失败。
func typeExprOf(e parse.Expr) parse.TypeExpr {
	switch v := e.(type) {
	case *parse.Ident:
		return &parse.NamedType{Name: v.Name, Pos: v.Pos}
	case *parse.Field:
		if id, ok := v.X.(*parse.Ident); ok {
			return &parse.NamedType{Pkg: id.Name, Name: v.Name, Pos: v.Pos}
		}
	case *parse.Index:
		// `Name[T, U]`：Index 是第一个类型实参，TypeArgs 是其余。
		base := typeExprOf(v.X)
		nt, isNT := base.(*parse.NamedType)
		if !isNT || v.End != nil {
			return nil
		}
		args := append([]parse.Expr{v.Index}, v.TypeArgs...)
		for _, a := range args {
			at := typeExprOf(a)
			if at == nil {
				return nil
			}
			nt.Args = append(nt.Args, at)
		}
		return nt
	}
	return nil
}

// compositeClass 解析复合字面量的类型头 → (类, 泛型实例或 nil)。
// 唯一实现：本包裸名 / 跨包限定名 / 泛型实例 Box[i32]（三处各写一份 = 红线 10 的病）。
func (c *Checker) compositeClass(v *parse.CompositeLit) (*Class, *Instance) {
	switch head := v.TypeName.(type) {
	case *parse.Ident:
		cl, isCl := c.classes[head.Name]
		if !isCl {
			c.errorAt(v.Pos, "unknown class name", head.Name+"{…}",
				"a composite literal applies to classes only; for containers write i32[]{} or declare the type first")
			return nil, nil
		}
		return cl, nil
	case *parse.Field:
		id, isID := head.X.(*parse.Ident)
		if !isID {
			break
		}
		if !c.imports[id.Name] {
			c.errorAt(v.Pos, "package not imported", id.Name+"."+head.Name,
				"write import "+id.Name+", then construct its type")
			return nil, nil
		}
		if t := c.depType(id.Name, head.Name, nil, v.Pos); t != nil {
			if cl, isCl := t.(*Class); isCl {
				return cl, nil
			}
			c.errorAt(v.Pos, "a composite literal applies to classes only", id.Name+"."+head.Name+"{…}",
				"write the enum variant as "+id.Name+"."+head.Name+".Variant; declare a type first for containers")
			return nil, nil
		}
		return nil, nil
	case *parse.Index:
		// 泛型实例 Box[i32]{…}：X 是裸类名或跨包限定名，Args 是类型实参。
		base, _ := c.compositeClass(&parse.CompositeLit{TypeName: head.X, Pos: v.Pos})
		if base == nil {
			return nil, nil
		}
		if len(base.TypeParams) == 0 {
			c.errorAt(v.Pos, "a non-generic type takes no type arguments", base.Name+"[…]",
				"drop the [..]; only a class declared with [T] takes arguments")
			return nil, nil
		}
		inst := &Instance{Base: base}
		argExprs := append([]parse.Expr{head.Index}, head.TypeArgs...)
		for _, a := range argExprs {
			at := c.resolveType(typeExprOf(a))
			if at == nil {
				return nil, nil
			}
			inst.Args = append(inst.Args, at)
		}
		if len(inst.Args) != len(base.TypeParams) {
			c.errorAt(v.Pos, "wrong number of type arguments",
				base.Name+" has "+itoa(len(inst.Args))+" arguments, the declaration has "+itoa(len(base.TypeParams)),
				"type arguments map one-to-one onto the declaration [T, ...]")
			return nil, nil
		}
		return base, inst
	}
	c.errorAt(v.Pos, "the composite literal has no type name", "the expression before { ... }",
		"write Class{field: value} (across packages: pkg.Class{field: value})")
	return nil, nil
}

func (c *Checker) classInit(cl *Class) *FuncSig {
	for _, m := range cl.Methods {
		if m.IsInit {
			return m
		}
	}
	return nil
}

// checkArrayLit: [] 或 [a, b, c]——不参与类型推断（§二.2），元素类型来自上下文。
func (c *Checker) checkArrayLit(v *parse.ArrayLit, expect Type) (Type, bound, untyped) {
	var elem Type
	switch e := expect.(type) {
	case *Slice:
		elem = e.Elem
	case *ArrayT:
		if len(v.Elems) != int(e.N) {
			c.errorAt(v.Pos, "wrong number of elements for the fixed-size array",
				"declare [;"+itoa(int(e.N))+"], the literal has "+itoa(len(v.Elems)),
				"the number of elements must equal N")
		}
		elem = e.Elem
	case nil:
		c.errorAt(v.Pos, "a list literal takes no part in type inference", "[] / [a, b]",
			"write the element type in the declaration: var xs i32[] = [] (core design §2.2)")
		return nil, nilBound, untyped{}
	default:
		c.errorAt(v.Pos, "the target type is not a list or a fixed-size array", typeText(expect),
			"a list literal can initialise T[] or [T;N] only")
		return nil, nilBound, untyped{}
	}
	for _, el := range v.Elems {
		et, _, einfo := c.checkExprFull(el, elem)
		c.requireAssignableAt(el, et, einfo, elem, "list element")
	}
	if expect != nil {
		return expect, bound{off: c.regionOff, exact: true}, untyped{}
	}
	return &Slice{Elem: elem}, bound{off: c.regionOff, exact: true}, untyped{}
}

// checkLambda: lambda 需要上下文函数类型（核心设计 §三）；N1 的 `func` 字面量
// （参数带类型）自带签名，可以没有上下文类型，并且**按值捕获**外层局部。
func (c *Checker) checkLambda(v *parse.LambdaExpr, expect Type) (Type, bound, untyped) {
	target, ok := expect.(*FuncT)
	if v.Func {
		// `func(x i32) -> R { … }`：签名自带。有期望类型时必须逐项一致。
		own, ok2 := c.funcLitType(v)
		if !ok2 {
			return nil, nilBound, untyped{}
		}
		if ok && !identical(own, target) {
			c.errorAt(v.Pos, "the func literal signature does not match its target type",
				own.String()+" vs "+target.String(),
				"make the parameter and result types identical, or drop the annotation and let the literal decide")
			return nil, nilBound, untyped{}
		}
		target = own
	} else if !ok {
		c.errorAt(v.Pos, "the lambda lacks an expected function type", "the parameter types of a lambda cannot be inferred from nothing",
			"give the target type: var name (P) -> R = (x) => ..., pass it as an argument of function type, or write the typed form func(x P) -> R { ... }")
		return nil, nilBound, untyped{}
	}
	if len(v.Params) != len(target.Params) {
		c.errorAt(v.Pos, "lambda parameter count does not match the expected type",
			"the lambda has "+itoa(len(v.Params))+" parameters, expected "+itoa(len(target.Params)),
			"the parameter list maps one-to-one onto (P1, P2) -> R")
		return nil, nilBound, untyped{}
	}
	// 体在独立作用域检查；块体 return 对照自身签名。
	inner := NewScope(c.scope, c.regionOff)
	for i, p := range v.Params {
		inner.Declare(&Symbol{Kind: SymVar, Name: p, Type: target.Params[i], DeclOffset: c.regionOff, Param: true})
	}
	savedScope, savedNil := c.scope, c.nilState
	savedFn := c.fn
	savedBase, savedCaps := c.lambdaBase, c.caps
	var caps []capture
	c.fn = &FuncSig{Results: resultsOf(target)} // lambda 块体 return 对照自身签名
	c.scope, c.nilState = inner, map[string]*nilState{}
	c.lambdaBase, c.caps = inner, &caps
	defer func() {
		c.scope, c.nilState, c.fn = savedScope, savedNil, savedFn
		c.lambdaBase, c.caps = savedBase, savedCaps
	}()

	if v.Body != nil {
		bt, _, binfo := c.checkExprFull(v.Body, target.Result)
		if bt != nil && target.Result != nil {
			c.requireAssignableAt(v.Body, bt, binfo, target.Result, "lambda result")
		}
	}
	if v.Block != nil {
		c.checkBlock(v.Block)
	}
	if len(caps) > 0 && c.closures != nil {
		names := make([]string, 0, len(caps))
		for _, cp := range caps {
			names = append(names, cp.Name)
		}
		c.closures[v] = names
	}
	return withCaptures(target, caps), closureBound(caps, c.regionOff), untyped{}
}

// paramNameAt 取第 i 个形参名（诊断上下文用；越界给空串）。
func paramNameAt(v *parse.LambdaExpr, i int) string {
	if i < len(v.Params) {
		return v.Params[i]
	}
	return ""
}

// funcLitType 由 `func(...) -> R` 字面量自身的拼写建立函数类型。
func (c *Checker) funcLitType(v *parse.LambdaExpr) (*FuncT, bool) {
	ft := &FuncT{}
	for i, pt := range v.Ptypes {
		t := c.resolveType(pt)
		if t == nil {
			c.errorAt(parse.TypeExprPos(pt), "the parameter type of the func literal cannot be resolved",
				"parameter " + itoa(i+1) + " (" + paramNameAt(v, i) + ")",

				"write a basic type or a type declared in this package")
			return nil, false
		}
		ft.Params = append(ft.Params, t)
	}
	for _, rt := range v.Results {
		t := c.resolveType(rt)
		if t == nil {
			c.errorAt(parse.TypeExprPos(rt), "the result type of the func literal cannot be resolved", "result type",
				"write a basic type or a type declared in this package")
			return nil, false
		}
		ft.Results = append(ft.Results, t)
	}
	if len(ft.Results) == 1 {
		ft.Result = ft.Results[0]
		ft.Results = nil
	}
	return ft, true
}

// resultsOf 把一个函数类型的返回位摊成列表（单返回位退化为 1 项）。
func resultsOf(ft *FuncT) []Type {
	if ft == nil {
		return nil
	}
	if len(ft.Results) > 0 {
		return ft.Results
	}
	if ft.Result != nil {
		return []Type{ft.Result}
	}
	return nil
}

// withCaptures 返回带捕获表的函数类型副本（**不改**期望类型对象：形参上的函数
// 类型是共享的，就地改写会污染其它调用点）。
func withCaptures(ft *FuncT, caps []capture) *FuncT {
	if len(caps) == 0 {
		return ft
	}
	out := &FuncT{Params: ft.Params, Result: ft.Result, Results: ft.Results}
	for _, cp := range caps {
		out.Captures = append(out.Captures, cp.Type)
	}
	return out
}

// checkCheckExpr: check 表达式前缀（§六）。
func (c *Checker) checkCheckExpr(v *parse.CheckExpr, expect Type) (Type, bound, untyped) {
	call, ok := v.X.(*parse.Call)
	if !ok {
		c.errorAt(v.Pos, "the operand of check must be a call", "check …",
			"write check f(...); the operand is a call whose last result is Err (core design §6)")
		return nil, nilBound, untyped{}
	}
	ct, _, _ := c.checkExprFull(call, expect)
	if ct == nil {
		return nil, nilBound, untyped{}
	}
	mt, isMulti := ct.(*MultiType)
	spelling := "check"
	if v.Postfix {
		spelling = "?"
	}
	// **只返回 Err 的调用**：类型表里是 Err 本身（不是多返回）⇒ 同样没有值可用，
	// 但必须与"末位不是 Err"区分开（否则报出**反了**的诊断，见 errpath.go 同款修正）。
	if !isMulti && isErr(ct) {
		c.errorAt(v.Pos, "the operand of "+spelling+" returns Err only: there is no value to use",
			spelling+" "+exprText(call),
			"bind it explicitly: var err = f(); or declare the result it should carry: -> (T, Err)")
		return nil, nilBound, untyped{}
	}
	if !isMulti || len(mt.Elems) == 0 || !isErr(mt.Elems[len(mt.Elems)-1]) {
		c.errorAt(v.Pos, "the last result of the "+spelling+" operand must be Err", spelling+" "+exprText(call),
			"the callee signature ends with Err; an ordinary function needs no "+spelling)
		return nil, nilBound, untyped{}
	}
	// 所在函数必须有 Err 位
	if !c.fnHasErr() {
		spelling := "check"
		if v.Postfix {
			spelling = "?"
		}
		c.errorAt(v.Pos, "the enclosing function has no Err result slot", "the current function signature has no Err",
			spelling+" propagates to the caller: add -> Err to the function, or handle the error here (or / catch / !)")
		return nil, nilBound, untyped{}
	}
	// check 的值 = 去掉末位 Err 的结果
	var head Type
	switch len(mt.Elems) {
	case 1:
		head = nil // 仅 Err 的调用：check 后无值
	case 2:
		head = mt.Elems[0]
	default:
		head = &MultiType{Elems: mt.Elems[:len(mt.Elems)-1]}
	}
	return head, bound{off: c.regionOff}, untyped{}
}

func (c *Checker) fnHasErr() bool {
	if c.fn == nil {
		return false
	}
	n := len(c.fn.Results)
	return n > 0 && isErr(c.fn.Results[n-1])
}
