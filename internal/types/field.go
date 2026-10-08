package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// 成员访问 / 下标 / str 方法（核心设计 §二.2 §二.3）。
// ---------------------------------------------------------------------------

func (c *Checker) checkField(v *parse.Field, expect Type) (Type, bound, untyped) {
	// 跨包**枚举变体**取值：pkg.Enum.Variant（三段名）。
	// 此前只有本包的 `Enum.Variant` 能取值，跨包变体值不可达（`lib.Color.green` 报
	// "a package member is not a value"）—— 跨包变体在模式位与构造位都可用，只有取值位漏了。
	if inner, isField := v.X.(*parse.Field); isField {
		if id, isID := inner.X.(*parse.Ident); isID {
			if _, isPkg := c.imports[id.Name]; isPkg {
				if en, va, found := c.depVariant(id.Name, inner.Name, v.Name, v.Pos); found {
					if va.Payload != nil {
						c.errorAt(v.Pos, "a variant with a payload cannot be referenced directly", en.Name+"."+v.Name,
							"construct with "+id.Name+"."+en.Name+"."+v.Name+"(payload); destructure with match")
						return nil, nilBound, untyped{}
					}
					return en, bound{off: c.regionOff, exact: true}, untyped{}
				}
				c.errorAt(v.Pos, "the enum has no such variant", id.Name+"."+inner.Name+"."+v.Name,
					"see the enum declaration in package "+id.Name+"; a payload-free variant is a value (core design §3)")
				return nil, nilBound, untyped{}
			}
		}
	}
	// 包限定：str.utf8At / util.add —— 包名.成员
	if id, ok := v.X.(*parse.Ident); ok {
		if _, isPkg := c.imports[id.Name]; isPkg {
			// 跨包常量 util.Limit：常量是值，可直接读（函数/类型只能调用或构造）
			if sym, found := c.depConst(id.Name, v.Name, v.Pos); found {
				if sym == nil {
					return nil, nilBound, untyped{}
				}
				if sym.Type == nil {
					return nil, nilBound, c.untypedFromConst(sym.Const, v.Pos)
				}
				return sym.Type, nilBound, untyped{}
			}
			if c.depInfo(id.Name) != nil {
				c.errorAt(v.Pos, "a package member is not a value", id.Name+"."+v.Name,
					"across packages only functions (call them), types (construct them), and constants are visible; functions are not first-class (core design §4)")
				return nil, nilBound, untyped{}
			}
			c.errorAt(v.Pos, "a package member can only be called", id.Name+"."+v.Name,
				"write it as a call: "+id.Name+"."+v.Name+"(...)")
			return nil, nilBound, untyped{}
		}
		if en, isEnum := c.enums[id.Name]; isEnum {
			if idx, isVar := en.variantI[v.Name]; isVar {
				va := en.Variants[idx]
				if va.Payload != nil {
					c.errorAt(v.Pos, "a variant with a payload cannot be referenced directly", en.Name+"."+v.Name,
						"construct with "+en.Name+"."+v.Name+"(payload); destructure with match")
					return nil, nilBound, untyped{}
				}
				return en, bound{off: c.regionOff, exact: true}, untyped{}
			}
			c.errorAt(v.Pos, "the enum has no such variant", en.Name+"."+v.Name,
				"see enum "+en.Name+" declaration")
			return nil, nilBound, untyped{}
		}
	}
	recvTy, recvBound, _ := c.checkExprFull(v.X, nil)
	if recvTy == nil {
		return nil, nilBound, untyped{}
	}
	if b, isBasic := recvTy.(*Basic); isBasic && b.Name == "Err" {
		switch v.Name {
		case "code":
			return TI32, bound{off: 0}, untyped{}
		case "msg":
			return TStr, bound{off: 0}, untyped{}
		case "cause":
			// §16 N5 ④：`Err = {code, msg, cause}`；cause 为空指针时读它 = 解引用 nil
			//（§九 nil 教条：疑 nil = 运行时 trap，故这里只判"可证必 nil"）。
			c.checkDeref(v.X, "field access cause")
			return TErr, bound{off: 0}, untyped{}
		}
		c.errorAt(v.Pos, "Err has only the fields code, msg and cause", "Err."+v.Name,
			"write e.code / e.msg / e.cause (core design §6 / §16 N5 ④)")
		return nil, nilBound, untyped{}
	}
	switch cl := recvTy.(type) {
	case *Class:
		if idx, ok := cl.fieldIdx[v.Name]; ok {
			if !c.memberVisible(cl.Pkg, cl.Name, v.Name, v.Pos) {
				return nil, nilBound, untyped{}
			}
			// 解引用位（§九）：可证必然为 nil 的字段访问 = 编译错
			c.checkDeref(v.X, "field access "+v.Name)
			// 字段读界 = 宿主界（§五 R3）；字段不做 nil 流跟踪（§九）
			return cl.Fields[idx].Type, bound{off: recvBound.off}, untyped{}
		}
		c.errorAt(v.Pos, "the class has no such field", cl.String()+"."+v.Name,
			"see the class declaration for its fields; call methods with parentheses")
		return nil, nilBound, untyped{}
	case *Instance:
		// 泛型实例的字段访问：基类字段 + 按实例实参代换（Box[i32].v : i32）。
		base, params, args := unwrapInstance(cl)
		if bcl, isCl := base.(*Class); isCl {
			if idx, ok := bcl.fieldIdx[v.Name]; ok {
				if !c.memberVisible(bcl.Pkg, bcl.Name, v.Name, v.Pos) {
					return nil, nilBound, untyped{}
				}
				c.checkDeref(v.X, "field access "+v.Name)
				return subst(bcl.Fields[idx].Type, params, args), bound{off: recvBound.off}, untyped{}
			}
			c.errorAt(v.Pos, "the class has no such field", cl.String()+"."+v.Name,
				"see the class declaration for its fields; call methods with parentheses")
			return nil, nilBound, untyped{}
		}
	case *Interface:
		if m, ok := cl.Methods[v.Name]; ok {
			c.errorAt(v.Pos, "an interface method must be called", cl.String()+"."+v.Name,
				"write "+cl.String()+"."+v.Name+"(args); signature -> "+resultsText(m.Results))
			return nil, nilBound, untyped{}
		}
		c.errorAt(v.Pos, "the interface has no such method", cl.String()+"."+v.Name,
			"see the interface declaration for its method set")
		return nil, nilBound, untyped{}
	case *Enum:
		c.errorAt(v.Pos, "an enum has no members", cl.String()+"."+v.Name,
			"a variant is accessed as "+cl.String()+".Variant")
		return nil, nilBound, untyped{}
	}
	c.errorAt(v.Pos, "this type has no members", typeOrUn(recvTy, untyped{})+"."+v.Name,
		"member access applies to class fields and enum variants only")
	return nil, nilBound, untyped{}
}

func resultsText(results []Type) string {
	s := ""
	for i, r := range results {
		if i > 0 {
			s += ", "
		}
		s += r.String()
	}
	return s
}

func (c *Checker) checkIndex(v *parse.Index, expect Type) (Type, bound, untyped) {
	xt, xb, _ := c.checkExprFull(v.X, nil)
	if xt == nil {
		return nil, nilBound, untyped{}
	}
	// 下标位接受任意整型（H6 唯一例外，核心设计 §二.4）
	c.checkIndexInt(v.Index, "index")
	if v.End != nil {
		c.checkIndexInt(v.End, "view end")
	}

	switch t := xt.(type) {
	case *Slice:
		if v.End != nil {
			c.errorAt(v.Pos, "a list view is not on the surface (reserved: O6)", typeText(t)+"[i..j]",
				"only str has a slice view (str[i..j]); for lists write a loop")
			return nil, nilBound, untyped{}
		}
		return t.Elem, bound{off: xb.off}, untyped{}
	case *ArrayT:
		if v.End != nil {
			c.errorAt(v.Pos, "a fixed-size array has no view", t.String(), "copy or iterate element by element")
			return nil, nilBound, untyped{}
		}
		return t.Elem, bound{off: xb.off}, untyped{}
	case *Basic:
		if t.Name == "str" {
			if v.End != nil {
				return TStr, bound{off: 0}, untyped{} // 视图字节恒任务区域
			}
			return TU8, bound{off: 0}, untyped{}
		}
	case *MapT:
		c.errorAt(v.Pos, "map does not support indexing", typeText(t)+"[k]",
			"read a map with get(key) and write it with put(key, value) (miss semantics: core design §2.2)")
		return nil, nilBound, untyped{}
	}
	c.errorAt(v.Pos, "this type does not support indexing", typeOrUn(xt, untyped{}),
		"indexing applies to lists, fixed-size arrays, and str")
	return nil, nilBound, untyped{}
}

// checkIndexInt: 下标位与视图端点接受任意整型（有符号按数值判负——运行期）。
func (c *Checker) checkIndexInt(e parse.Expr, what string) {
	ty, _, info := c.checkExprFull(e, TUsize)
	if ty == nil {
		return
	}
	if isInt(ty) {
		return
	}
	if info.kind == unInt || info.kind == unUint {
		return
	}
	c.errorAt(parse.ExprPos(e), what+"must be an integer type", typeOrUn(ty, info),
		"an index accepts any integer type (the H6 exception); floats and other types do not")
}

// checkStrMethod: str 包内建方法面（核心设计 §二.3/§十四）。
// 内建方法表必须与发射表**同一套**（emit/builtin.go 的 str 方法）：曾经只有 emit 认
// `isEmpty`，于是 `"abc".isEmpty()` 靠"字面量接收者静默通过"侥幸可用，而变量接收者被拒
// —— 两张手工表分叉是系统性风险，这里补齐并加锚点。
func (c *Checker) checkStrMethod(v *parse.Call, field *parse.Field, recvBound bound, expect Type) (Type, bound, untyped) {
	switch field.Name {
	case "len":
		if len(v.Args) != 0 {
			c.errorAt(v.Pos, "len() takes no arguments", "s.len()", "remove the parenthesised content")
			return nil, nilBound, untyped{}
		}
		return TUsize, nilBound, untyped{}
	case "isEmpty":
		if len(v.Args) != 0 {
			c.errorAt(v.Pos, "isEmpty() takes no arguments", "s.isEmpty()", "remove the parenthesised content")
			return nil, nilBound, untyped{}
		}
		return TBool, nilBound, untyped{}
	}
	c.errorAt(v.Pos, "str has no such method", "str."+field.Name,
		"str has the methods len() and isEmpty() only; everything else lives in the str package (concat/sub/trim/..., core design §14)")
	return nil, nilBound, untyped{}
}
