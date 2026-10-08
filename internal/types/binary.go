package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// 二元运算（核心设计 §一 运算符全表 + §三 比较语义）。禁混算 f32/f64；无隐式
// 转换；未类型化字面量与定型操作数混合时按可表示性落入对方类型。
// ---------------------------------------------------------------------------

func (c *Checker) checkBinary(v *parse.Binary, expect Type) (Type, bound, untyped) {
	// && || 短路：两侧必须 bool（无条件 coercion）。
	if v.Op == "&&" || v.Op == "||" {
		c.requireBoolOperand(v.Left, v.Op)
		c.requireBoolOperand(v.Right, v.Op)
		return TBool, nilBound, untyped{}
	}

	lt, lb, linfo := c.checkExprFull(v.Left, expect)
	rt, rb, rinfo := c.checkExprFull(v.Right, expect)
	// 上游真错误 = 类型与字面量信息双缺；未类型化字面量的类型位是 nil 但
	// 信息在，必须继续走（字面量在运算位按对侧定型）。
	if (lt == nil && linfo.kind == unNone) || (rt == nil && rinfo.kind == unNone) {
		return nil, nilBound, untyped{}
	}

	// N3：泛型形参上的比较由**约束**授权 —— `[T: Ord]` 允许 </<=/>/>=，
	// `[T: Eq]`（或 Ord）允许 ==/!=。约束是唯一的授权来源：没有约束的 T
	// 一律不能比较（否则模板体会在实例化时才炸）。
	if tp, ok := lt.(*TypeParam); ok {
		if opAllowedByConstraint(c.constraintsOfParam(tp.Name), v.Op) {
			if _, rtIsSame := rt.(*TypeParam); rtIsSame || rt == nil {
				return TBool, nilBound, untyped{}
			}
		}
	}
	if tp, ok := rt.(*TypeParam); ok {
		if opAllowedByConstraint(c.constraintsOfParam(tp.Name), v.Op) && lt == nil {
			return TBool, nilBound, untyped{}
		}
	}

	// str 连接/比较：未类型化 str 字面量按对侧（或彼此）定型为 str。
	// 少了这一步，"x" + a、a + ""、"a" + "b" 全被判成「运算符两侧类型不符」——
	// 字面量在运算位按对侧定型是 §一 的通用规则，数值位有 unifyOperands 承担，
	// str 位必须同样处理（否则 str + str 这条运算符只在两侧都已是变量时可用）。
	if linfo.kind == unStr || rinfo.kind == unStr {
		if isStr(lt) {
			rt = TStr
		}
		if isStr(rt) {
			lt = TStr
		}
		if linfo.kind == unStr && rinfo.kind == unStr {
			lt, rt = TStr, TStr
		}
	}

	// 字符串连接与比较（§一/§三）
	if isStr(lt) && isStr(rt) {
		switch v.Op {
		case "+":
			return TStr, bound{off: maxOff(lb, rb)}, untyped{}
		case "==", "!=":
			return TBool, nilBound, untyped{}
		case "<", "<=", ">", ">=":
			return TBool, nilBound, untyped{} // 字节字典序
		}
		c.opMismatch(v, lt, rt)
		return nil, nilBound, untyped{}
	}

	// 移位：左任意整型，右任意整型（count ≥ 位宽 = 运行期 trap）。
	if v.Op == "<<" || v.Op == ">>" {
		if !intOperand(lt, linfo) || !intOperand(rt, rinfo) {
			c.opMismatch(v, lt, rt)
			return nil, nilBound, untyped{}
		}
		if !c.operandsReady(v, lt, rt, linfo, rinfo, isInt) {
			return nil, nilBound, untyped{}
		}
		c.unifyOperands(v, &lt, &rt, linfo, rinfo, isInt)
		return lt, nilBound, untyped{}
	}

	// 算术
	switch v.Op {
	case "+", "-", "*", "/", "%":
		if !numOperand(lt, linfo) || !numOperand(rt, rinfo) {
			c.opMismatch(v, lt, rt)
			return nil, nilBound, untyped{}
		}
		if v.Op == "%" && (isFloatType(lt) || isFloatType(rt) || linfo.kind == unFloat || rinfo.kind == unFloat) {
			// C 的 % 只对整数成立（发出去就是非法 C）；浮点取余要显式调用。
			// 注意**未定型字面量**也要判（`5.5 % 2.0` 的两侧此刻都还是 nil 类型）。
			c.errorAt(v.Pos, "the remainder operator % is integer-only", typeOrUn(lt, linfo)+" % "+typeOrUn(rt, rinfo),
				"for floats use math.fmod(a, b); % keeps truncation semantics for integers (core design §1)")
			return nil, nilBound, untyped{}
		}
		if !c.operandsReady(v, lt, rt, linfo, rinfo, isNumeric) {
			return nil, nilBound, untyped{}
		}
		c.unifyOperands(v, &lt, &rt, linfo, rinfo, isNumeric)
		return lt, nilBound, untyped{}
	}

	// 位运算（§一 7/8/9 级）与移位（4 级）：整数专属。
	// 移位量的范围（count ≥ 左操作数位宽）在**发射侧**带 trap（§一「移位语义」）。
	switch v.Op {
	case "&", "|", "^":
		if !intOperand(lt, linfo) || !intOperand(rt, rinfo) {
			c.opMismatch(v, lt, rt)
			return nil, nilBound, untyped{}
		}
		if !c.operandsReady(v, lt, rt, linfo, rinfo, isInt) {
			return nil, nilBound, untyped{}
		}
		c.unifyOperands(v, &lt, &rt, linfo, rinfo, isInt)
		return lt, nilBound, untyped{}
	case "<<", ">>":
		if !intOperand(lt, linfo) {
			c.errorAt(v.Pos, "the left operand of "+v.Op+" must be an integer", typeOrUn(lt, linfo),
				"shifts are integer-only (core design §1)")
			return nil, nilBound, untyped{}
		}
		if !intOperand(rt, rinfo) {
			c.errorAt(v.Pos, "the shift count must be an integer", typeOrUn(rt, rinfo),
				"the count may be any integer type; count >= the left operand's width traps at run time (core design §1)")
			return nil, nilBound, untyped{}
		}
		if lt == nil {
			lt = c.literalDefault(linfo, parse.ExprPos(v.Left))
		}
		if lt == nil {
			return nil, nilBound, untyped{}
		}
		// 结果类型 = 左操作数类型；计数不参与统一（`x << u8(3)` 合法）。
		return lt, nilBound, untyped{}
	}

	// 比较（§三 比较语义）
	switch v.Op {
	case "<", "<=", ">", ">=":
		if !numOperand(lt, linfo) || !numOperand(rt, rinfo) {
			// §三：**无负载 enum 可按 tag 排序**（带负载的必须用 match）；`@packed` 需 @derive(Compare)。
			if _, ok := orderedEnumOperand(lt, rt); ok {
				return TBool, nilBound, untyped{}
			}
			if cl, ok := packedCompareOperand(lt, rt); ok {
				if !cl.Derived["Compare"] {
					c.errorAt(v.Pos, "ordering a @packed value needs @derive(Compare)", cl.Name+" "+v.Op+" "+cl.Name,
						"add @derive(Compare) to "+cl.Name+", or compare fields explicitly (core design §3)")
					return nil, nilBound, untyped{}
				}
				return TBool, nilBound, untyped{}
			}
			c.opMismatch(v, lt, rt)
			return nil, nilBound, untyped{}
		}
		if !c.operandsReady(v, lt, rt, linfo, rinfo, isNumeric) {
			return nil, nilBound, untyped{}
		}
		c.unifyOperands(v, &lt, &rt, linfo, rinfo, isNumeric)
		return TBool, nilBound, untyped{}
	case "==", "!=":
		return c.checkEquality(v, lt, lb, rt, rb, linfo, rinfo)
	}

	c.errorAt(v.Pos, "unsupported operator", v.Op, "the full operator table is core design §1")
	return nil, nilBound, untyped{}
}

// orderedEnumOperand 报告两个操作数是否是**同一个无负载 enum**（可按 tag 排序，§三：
// 带负载的 enum 禁 `==`/`<`，必须用 match；无负载的 tag 就是它的值）。
func orderedEnumOperand(lt, rt Type) (*Enum, bool) {
	l, ok1 := lt.(*Enum)
	r, ok2 := rt.(*Enum)
	if !ok1 || !ok2 || l.HasData || r.HasData || l.Name != r.Name || l.Pkg != r.Pkg {
		return nil, false
	}
	return l, true
}

// packedCompareOperand 报告两个操作数是否是**同一个** @packed 类（`<` 等需要 Compare）。
func packedCompareOperand(lt, rt Type) (*Class, bool) {
	l, ok1 := lt.(*Class)
	r, ok2 := rt.(*Class)
	if !ok1 || !ok2 || !l.Packed || !r.Packed || l.Name != r.Name || l.Pkg != r.Pkg {
		return nil, false
	}
	return l, true
}

// isFloatType 报告类型是否为 f32/f64。
func isFloatType(t Type) bool {
	b, ok := t.(*Basic)
	return ok && (b.Name == "f32" || b.Name == "f64")
}

// checkEquality 实现 §三 比较语义：无数据 enum、@packed 逐字段、class 身份、
// err==nil、容器与普通 class 无 <。
func (c *Checker) checkEquality(v *parse.Binary, lt Type, lb bound, rt Type, rb bound, linfo, rinfo untyped) (Type, bound, untyped) {
	if !c.operandsReady(v, lt, rt, linfo, rinfo, nil) {
		return nil, nilBound, untyped{}
	}
	// nil 比较：任意引用/容器/Err 与 nil
	// 未类型化数值字面量按对侧定型（== 也无隐式转换，只做范围证明）
	if lt == nil && rt != nil && hasLitType(linfo) {
		if !c.untypedFits(linfo, rt) {
			c.errorAt(v.Pos, "the literal does not fit the other operand's type", typeOrUn(rt, linfo),
				"adjust the literal or use a wider type")
			return nil, nilBound, untyped{}
		}
		lt = rt
	} else if rt == nil && lt != nil && hasLitType(rinfo) {
		if !c.untypedFits(rinfo, lt) {
			c.errorAt(v.Pos, "the literal does not fit the other operand's type", typeOrUn(lt, rinfo),
				"adjust the literal or use a wider type")
			return nil, nilBound, untyped{}
		}
		rt = lt
	} else if lt == nil && rt == nil && hasLitType(linfo) && hasLitType(rinfo) {
		lt = c.widenLiterals(linfo, rinfo)
		rt = lt
	}
	if isUntypedNil(linfo) || isUntypedNil(rinfo) {
		other := lt
		if isUntypedNil(linfo) {
			other = rt
		}
		if !nilAssignable(other) {
			c.errorAt(v.Pos, "nil can only be compared with references, containers, or Err", typeOrUn(other, pickNilInfo(linfo, rinfo)),
				"comparing numeric/bool/str with nil is meaningless: test the zero value instead")
			return nil, nilBound, untyped{}
		}
		return TBool, nilBound, untyped{}
	}
	if !identical(lt, rt) {
		c.errorAt(v.Pos, "comparison across types is forbidden", "both sides are "+typeText(lt)+" and "+typeText(rt),
			"only equal types compare; convert explicitly first: "+typeText(lt)+"("+exprText(v.Right)+")")
		return nil, nilBound, untyped{}
	}
	switch t := lt.(type) {
	case *Basic:
		if t.Name == "Err" {
			// §三 只定义了 `err == nil` / `err != nil`（= code == 0 的内建比较）；
			// 两个 Err 之间没有定义的相等语义（发出去就是非法的 C 结构体比较）。
			c.errorAt(v.Pos, "two Err values cannot be compared", "Err "+v.Op+" Err",
				"test err != nil, or compare the fields: err.code == 0 / err.msg == \"\" (core design §3)")
			return nil, nilBound, untyped{}
		}
		return TBool, nilBound, untyped{} // 数值/bool/str（str 已在上面处理 <；== 合法）
	case *Enum:
		if t.HasData {
			c.errorAt(v.Pos, "an enum with payloads forbids ==", t.String(),
				"destructure variant by variant with match (core design §3)")
			return nil, nilBound, untyped{}
		}
		return TBool, nilBound, untyped{}
	case *Class:
		if !t.Packed {
			return TBool, nilBound, untyped{} // 身份比较（引用相等）
		}
		return TBool, nilBound, untyped{} // @packed 逐字段
	case *Interface:
		// 接口值没有定义好的相等语义（{data, vt} 逐字比较既非身份也非结构相等）
		// ⇒ 编译期拒，指向可写的替代（§三 只定义了 class 身份、@packed 逐字段、err==nil）。
		c.errorAt(v.Pos, "an interface value does not support ==", t.String(),
			"compare the concrete values after a type switch, or compare a field you control (core design §3)")
		return nil, nilBound, untyped{}
	case *ArrayT:
		c.errorAt(v.Pos, "a fixed-size array does not support ==", t.String(), "compare element by element, or use a class")
		return nil, nilBound, untyped{}
	case *Slice, *MapT, *SetT:
		c.errorAt(v.Pos, "a container does not support == (except against nil)", t.String(),
			"test emptiness with len()==0 and compare contents element by element")
		return nil, nilBound, untyped{}
	}
	return TBool, nilBound, untyped{}
}

func isUntypedNil(info untyped) bool { return info.kind == unNil }

func numOperand(ty Type, info untyped) bool {
	if ty != nil {
		return isNumeric(ty)
	}
	return info.kind == unInt || info.kind == unUint || info.kind == unFloat
}

func intOperand(ty Type, info untyped) bool {
	if ty != nil {
		return isInt(ty)
	}
	return info.kind == unInt || info.kind == unUint
}

func numLit(info untyped) bool {
	return info.kind == unInt || info.kind == unUint || info.kind == unFloat
}

// hasLitType: 一切值字面量（数值/浮点/str/bool）——等值位按对侧定型。
func hasLitType(info untyped) bool {
	return info.kind != unNone && info.kind != unNil
}

func pickNilInfo(l, r untyped) untyped {
	if l.kind == unNil {
		return r
	}
	return l
}

// operandsReady 校验两侧可参与运算（处理未类型化字面量与定型类型的混合）。
// pred = nil 表示不限制种类（比较位）。
func (c *Checker) operandsReady(v *parse.Binary, lt, rt Type, linfo, rinfo untyped, pred func(Type) bool) (ok bool) {
	ok = true
	ensure := func(side parse.Expr, ty Type, info untyped, at parse.Pos) Type {
		if ty != nil {
			if pred != nil && !pred(ty) {
				c.errorAt(at, "operand types are not valid for "+opNameOf(v), ty.String(),
					"this operator requires specific kinds of types; there is no implicit conversion (red line 1)")
				ok = false
				return nil
			}
			return ty
		}
		// 未类型化字面量在二元位先定型：与另一侧定型类型一致否则取最窄
		return nil
	}
	ensure(v.Left, lt, linfo, parse.ExprPos(v.Left))
	ensure(v.Right, rt, rinfo, parse.ExprPos(v.Right))
	// 混算禁令：f32/f64 与整型混用且都是定型类型
	if lt != nil && rt != nil && isNumeric(lt) && isNumeric(rt) && !identical(lt, rt) {
		if linfo.kind == unNone && rinfo.kind == unNone {
			c.errorAt(v.Pos, "the two sides have different types and there is no implicit conversion", lt.String()+" and "+rt.String(),
				"convert explicitly first: "+lt.String()+"("+exprText(v.Right)+") (red line 1)")
			return false
		}
	}
	return ok
}

// unifyOperands 在可运算前提下定型未类型化字面量（值域证明推迟到赋值点，
// 这里只决定结果的定型类型）。返回两侧统一后的偏移（取较大上界）。
func (c *Checker) unifyOperands(v *parse.Binary, lt, rt *Type, linfo, rinfo untyped, pred func(Type) bool) int {
	off := 0
	_ = off
	// 未类型化 + 定型：字面量落向定型侧（可表示性在赋值/实参位再证）
	if *lt == nil && *rt != nil {
		if !c.untypedFits(linfo, *rt) {
			c.errorAt(v.Pos, "the literal does not fit the other operand's type", typeOrUn(*rt, linfo),
				"adjust the literal or use a wider type")
		}
		*lt = *rt
	} else if *rt == nil && *lt != nil {
		if !c.untypedFits(rinfo, *lt) {
			c.errorAt(v.Pos, "the literal does not fit the other operand's type", typeOrUn(*lt, rinfo),
				"adjust the literal or use a wider type")
		}
		*rt = *lt
	} else if *lt == nil && *rt == nil {
		// 双字面量：取最窄可容纳（§一）
		*lt = c.widenLiterals(linfo, rinfo)
		*rt = *lt
	}
	return off
}

func (c *Checker) widenLiterals(l, r untyped) Type {
	if l.kind == unFloat || r.kind == unFloat {
		return TF64
	}
	if l.kind == unUint || r.kind == unUint {
		return TU64
	}
	return TI32
}

// untypedFits: 字面量对目标类型的可表示性（§二.4）。
func (c *Checker) untypedFits(info untyped, ty Type) bool {
	if info.kind == unNone || info.kind == unNil {
		return true
	}
	switch info.kind {
	case unStr:
		return isStr(ty)
	case unBool:
		return isBool(ty)
	case unInt:
		if isInt(ty) {
			lo, hi, ok := intRange(ty)
			return ok && info.ival >= lo && info.ival <= hi
		}
		if isFloat(ty) {
			return true
		}
		return false
	case unUint:
		if isInt(ty) {
			um, ok := uintMax(ty)
			return ok && info.uval <= um
		}
		if isFloat(ty) {
			return true
		}
		return false
	case unFloat:
		if isFloat(ty) {
			if ty == TF32 {
				return float32Fits(info.fval)
			}
			return true
		}
		return false
	}
	return true
}

func float32Fits(f float64) bool {
	f32 := float32(f)
	if f > 0 && float64(f32) > 3.4028234663852886e+38 {
		return false
	}
	return true
}

func (c *Checker) requireBoolOperand(e parse.Expr, op string) {
	ty, _, info := c.checkExprFull(e, TBool)
	if ty == nil {
		return
	}
	if !isBool(ty) && info.kind != unBool {
		c.errorAt(parse.ExprPos(e), op+" operands must be bool", typeOrUn(ty, info),
			"the condition must be bool; there is no truthiness coercion (core design §3)")
	}
}

func (c *Checker) opMismatch(v *parse.Binary, lt, rt Type) {
	c.errorAt(v.Pos, "the operand types do not match", opNameOf(v)+" applied to "+typeOrUn(lt, untyped{})+" and "+typeOrUn(rt, untyped{}),
		"check both operand types; there is no implicit conversion, convert explicitly (red line 1)")
}

func opNameOf(v *parse.Binary) string { return v.Op }

func maxOff(a, b bound) int {
	if a.off > b.off {
		return a.off
	}
	return b.off
}

// exprText 表达式回显（诊断上下文用；单行为主）。
//
// 覆盖**全部** 23 种表达式节点：早年只覆盖常用的十来个，其余的（`-1`、`f(x)!`、
// 字面量、λ、match…）会退化成 "expression" —— 诊断里写 "scope.timeout(expression)"
// 等于没写。诊断是这个语言的第一界面（AI 友好 = 报错必须能直接改），故补齐。
func exprText(e parse.Expr) string {
	switch v := e.(type) {
	case *parse.Ident:
		return v.Name
	case *parse.IntLit:
		return v.Text
	case *parse.FloatLit:
		return v.Text
	case *parse.StrLit:
		return v.Text
	case *parse.InterpLit:
		out := "\""
		for i, t := range v.Texts {
			out += t
			if i < len(v.Values) {
				out += "{" + exprText(v.Values[i]) + "}"
			}
		}
		return out + "\""
	case *parse.BoolLit:
		if v.Value {
			return "true"
		}
		return "false"
	case *parse.NilLit:
		return "nil"
	case *parse.ThisExpr:
		return "this"
	case *parse.Unary:
		return v.Op + exprText(v.X)
	case *parse.Binary:
		return exprText(v.Left) + " " + v.Op + " " + exprText(v.Right)
	case *parse.Assign:
		return exprText(v.LHS) + " " + v.Op + " " + exprText(v.Right)
	case *parse.MultiAssign:
		parts := make([]string, 0, len(v.LHS))
		for _, l := range v.LHS {
			parts = append(parts, exprText(l))
		}
		return joinComma(parts) + " " + v.Op + " " + exprText(v.Right)
	case *parse.ArrayLit:
		if len(v.Elems) == 0 {
			return "[]"
		}
		parts := make([]string, 0, len(v.Elems))
		for _, el := range v.Elems {
			parts = append(parts, exprText(el))
		}
		return "[" + joinComma(parts) + "]"
	case *parse.CompositeLit:
		parts := make([]string, 0, len(v.Fields))
		for _, f := range v.Fields {
			parts = append(parts, f.Name+": "+exprText(f.Value))
		}
		return exprText(v.TypeName) + "{" + joinComma(parts) + "}"
	case *parse.LambdaExpr:
		if v.Func {
			params := make([]string, 0, len(v.Params))
			for i, name := range v.Params {
				if i < len(v.Ptypes) && v.Ptypes[i] != nil {
					params = append(params, name+" "+typeExprText(v.Ptypes[i]))
					continue
				}
				params = append(params, name)
			}
			out := "func(" + joinComma(params) + ")"
			if len(v.Results) > 0 {
				res := make([]string, 0, len(v.Results))
				for _, r := range v.Results {
					if r != nil {
						res = append(res, typeExprText(r))
					}
				}
				out += " -> " + joinComma(res)
			}
			return out + " { … }"
		}
		if v.Block != nil {
			return "(" + joinComma(v.Params) + ") => { … }"
		}
		return "(" + joinComma(v.Params) + ") => " + exprText(v.Body)
	case *parse.MatchExpr:
		return "match " + exprText(v.Subject) + " { … }"
	case *parse.Call:
		args := make([]string, 0, len(v.Args))
		for _, a := range v.Args {
			args = append(args, exprText(a))
		}
		return exprText(v.Fn) + "(" + joinComma(args) + ")"
	case *parse.Index:
		if v.End != nil {
			return exprText(v.X) + "[" + exprText(v.Index) + ".." + exprText(v.End) + "]"
		}
		return exprText(v.X) + "[" + exprText(v.Index) + "]"
	case *parse.Field:
		return exprText(v.X) + "." + v.Name
	case *parse.CheckExpr:
		if v.Postfix {
			return exprText(v.X) + "?"
		}
		return "check " + exprText(v.X)
	case *parse.AssertExpr:
		return exprText(v.X) + "!"
	case *parse.OrExpr:
		return exprText(v.X) + " or " + exprText(v.Default)
	case *parse.CatchExpr:
		return exprText(v.X) + " catch " + v.Name + " { … }"
	}
	return "expression"
}

// typeExprText 类型表达式回显（诊断上下文用）：`[]i32` / `map[str]Point` /
// `[u8;4]` / `(i32) -> i32` / `Box[T]` / `sync.Mutex`。
func typeExprText(t parse.TypeExpr) string {
	switch v := t.(type) {
	case *parse.BasicType:
		return v.Name
	case *parse.SliceType:
		return typeExprText(v.Elem) + "[]"
	case *parse.SetType:
		return "set[" + typeExprText(v.Elem) + "]"
	case *parse.MapType:
		return "map[" + typeExprText(v.Key) + "]" + typeExprText(v.Value)
	case *parse.ArrayType:
		return "[" + typeExprText(v.Elem) + ";" + exprText(v.Size) + "]"
	case *parse.NamedType:
		name := v.Name
		if v.Pkg != "" {
			name = v.Pkg + "." + name
		}
		if len(v.Args) > 0 {
			args := make([]string, 0, len(v.Args))
			for _, a := range v.Args {
				args = append(args, typeExprText(a))
			}
			name += "[" + joinComma(args) + "]"
		}
		return name
	case *parse.FuncType:
		params := make([]string, 0, len(v.Params))
		for _, p := range v.Params {
			params = append(params, typeExprText(p))
		}
		out := "(" + joinComma(params) + ")"
		if v.Result != nil {
			out += " -> " + typeExprText(v.Result)
		}
		return out
	}
	return "type"
}
