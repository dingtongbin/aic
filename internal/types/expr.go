package types

import (
	"aic/internal/parse"
	"aic/internal/pkg"
)

// ---------------------------------------------------------------------------
// 表达式检查 + 静态深度界传播（核心设计 §五 R3，R0c ② 修订版）。
//
// bound：off = 对象深度上界（相对函数入口偏移）；exact = 创建表达式（必然是
// off 深度的那个对象）。规则：创建 = 当前偏移/精确；变量读 = 声明偏移（@live
// 已减一）；字段/元素读 = 宿主界；调用 = 调用点偏移；参数 = 0；nil = -∞。
// ---------------------------------------------------------------------------

type bound struct {
	off   int
	exact bool
	// param = 该界来自**形参（含 this）派生链**。参数定理（§五 R3）只保证
	// "参数派生值写本函数**局部变量**恒安全"；两个参数之间的深度顺序不可证，
	// 故 param 与 param 之间的存储必须留守卫（否则静默内存损坏，见 markStoreGuard）。
	param bool
}

var nilBound = bound{off: -1 << 29}

type untypedKind int

const (
	unNone untypedKind = iota
	unInt
	unUint
	unFloat
	unStr
	unBool
	unNil
)

// checkExpr 检查表达式并返回其类型与界。expect 是上下文期望类型（字面量/
// nil/容器字面量/lambda 用它完成推断），可为 nil。
func (c *Checker) checkExpr(e parse.Expr, expect Type) (Type, bound) {
	ty, b, info := c.checkExprFull(e, expect)
	_ = info
	c.types[e] = ty
	return ty, b
}

// checkExprFull 额外返回未类型化字面量信息（赋值点做范围证明用）。
//
// **每个表达式节点的检查类型都在这里落表**（c.types）：emit 侧靠这张表定宽与
// 选原语，而 return / 实参 / 字段初始化等位置走的是本函数（不是 checkExpr），
// 若只在 checkExpr 里记，那些位置上的表达式在 emit 侧就没有类型——
// 「return option.none()」因此发不出去（None 的目标类型要靠这张表）。
func (c *Checker) checkExprFull(e parse.Expr, expect Type) (Type, bound, untyped) {
	ty, b, info := c.checkExprFullRec(e, expect)
	if e != nil {
		c.types[e] = ty
	}
	return ty, b, info
}

// checkExprFullRec 是 checkExprFull 的实现体（不落表，由外层统一落）。
func (c *Checker) checkExprFullRec(e parse.Expr, expect Type) (Type, bound, untyped) {
	switch v := e.(type) {
	case *parse.IntLit:
		cv, _ := foldIntLit(v.Text)
		if cv.Kind == ConstUint {
			return nil, nilBound, untyped{kind: unUint, uval: cv.Uint, text: v.Text}
		}
		return nil, nilBound, untyped{kind: unInt, ival: cv.Int, text: v.Text}
	case *parse.FloatLit:
		f, err := parseFloat(v.Text)
		if err != nil {
			c.errorAt(v.Pos, "the float literal cannot be parsed", v.Text, "write it as 1.0 / 1.5e-3")
			return TF64, nilBound, untyped{}
		}
		return nil, nilBound, untyped{kind: unFloat, fval: f, text: v.Text}
	case *parse.StrLit:
		return nil, nilBound, untyped{kind: unStr, sval: unquote(v.Text)}
	case *parse.InterpLit:
		return c.checkInterp(v)
	case *parse.BoolLit:
		return nil, nilBound, untyped{kind: unBool, bval: v.Value}
	case *parse.NilLit:
		return nil, nilBound, untyped{kind: unNil}
	case *parse.ThisExpr:
		if c.thisClass == nil {
			c.errorAt(v.Pos, "this may only appear inside a method body", "this",
				"a method is a func inside a class body; a free function has no this (core design §4)")
			return nil, nilBound, untyped{}
		}
		return c.thisClass, bound{off: 0}, untyped{}
	case *parse.Ident:
		return c.checkIdent(v, expect)
	case *parse.Unary:
		return c.checkUnary(v, expect)
	case *parse.Binary:
		return c.checkBinary(v, expect)
	case *parse.Call:
		return c.checkCall(v, expect)
	case *parse.Field:
		return c.checkField(v, expect)
	case *parse.Index:
		return c.checkIndex(v, expect)
	case *parse.ArrayLit:
		return c.checkArrayLit(v, expect)
	case *parse.CompositeLit:
		return c.checkComposite(v, expect)
	case *parse.LambdaExpr:
		return c.checkLambda(v, expect)
	case *parse.MatchExpr:
		return c.checkMatchExpr(v, expect)
	case *parse.CheckExpr:
		return c.checkCheckExpr(v, expect)
	case *parse.OrExpr:
		return c.checkOr(v, expect)
	case *parse.CatchExpr:
		return c.checkCatch(v, expect)
	case *parse.AssertExpr:
		return c.checkAssert(v, expect)
	}
	c.errorAt(parse.ExprPos(e), "this expression form is not allowed here", "got "+exprName(e),
		"assignment is not an expression (core design §3); bind a variable first")
	return nil, nilBound, untyped{}
}

type untyped struct {
	kind untypedKind
	ival int64
	uval uint64
	fval float64
	sval string
	bval bool
	text string
}

func (c *Checker) checkIdent(v *parse.Ident, expect Type) (Type, bound, untyped) {
	if sym, ok := c.scope.Lookup(v.Name); ok {
		// N1 闭包捕获：命中点在当前闭包自己的作用域**之外** ⇒ 这是一次按值捕获。
		if c.caps != nil && !c.lookupInsideLambda(v.Name) {
			c.noteCapture(v, v.Name, sym)
		}
		switch sym.Kind {
		case SymVar:
			if st := c.nilState[v.Name]; st != nil && st.mustNil {
				return sym.Type, nilBound, untyped{}
			}
			// 变量读界 = 声明偏移（§五 R3）；param 标记 = 形参（含 this）派生，
			// 用于存储点判定：参数定理只覆盖"写本函数局部变量"，两个参数之间没有深度顺序。
			return sym.Type, bound{off: sym.DeclOffset, param: sym.Param}, untyped{}
		case SymConst:
			if sym.Type == nil {
				// 未定型 const = 字面量别名：范围证明推迟到使用点
				return nil, nilBound, c.untypedFromConst(sym.Const, v.Pos)
			}
			return sym.Type, nilBound, untyped{}
		case SymFunc:
			c.errorAt(v.Pos, "a function name is not a value (functions are not first-class)", v.Name,
				"call it as "+v.Name+"(...); function pointers are not part of the surface (FFI: §8)")
			return nil, nilBound, untyped{}
		case SymTypeParam:
			c.errorAt(v.Pos, "a type parameter is not a value", v.Name, "a type parameter appears in type positions only")
			return nil, nilBound, untyped{}
		}
	}
	// std option 的 None（import 后可用；T 由上下文期望定型）
	if v.Name == "None" {
		if _, hasOpt := c.stdOptionEnum(); hasOpt {
			if inst, ok := IsOptionInstance(expect); ok {
				return inst, nilBound, untyped{}
			}
		}
	}
	// 裸名撞字段（红线 3）：本类有此字段但没写 this.
	if c.thisClass != nil {
		if _, isField := c.thisClass.fieldIdx[v.Name]; isField {
			c.errorAt(v.Pos, "a bare name collides with a field: access this class's fields as this.", v.Name,
				"write this."+v.Name+" (core design §4, red line 3)")
			return nil, nilBound, untyped{}
		}
	}
	if en := c.lookupEnumByVariant(v.Name); en != nil {
		// 无数据变体的裸名引用：Some/None 式用法限定在负载枚举上不允许——
		// 变体名必须限定（E.A）或经 match 模式绑定。
		c.errorAt(v.Pos, "an enum variant must be qualified by its enum name", v.Name,
			"write "+en.Name+"."+v.Name)
		return nil, nilBound, untyped{}
	}
	if isComptimeOnlyName(v.Name) {
		// N4：内省原语只在编译期可用 —— 报"没有运行期形态"，而不是"未声明的名字"。
		c.errorAt(v.Pos, "compile-time introspection has no runtime form", v.Name,
			v.Name+"(T) exists in constant positions only (const / comptime block / [T;N] size): the compiler emits no type metadata (core design §16 N4)")
		return nil, nilBound, untyped{}
	}
	if c.failed[v.Name] {
		// 这个名字的声明**已经报过错**（const 折叠失败等）：不再刷"未声明的名字"
		// （级联噪音会把真正的首因埋掉）。
		return nil, nilBound, untyped{}
	}
	if pkg.IsStd(v.Name) && !c.imports[v.Name] {
		// 缺 import 是最常见的首因之一；"未声明的名字 + 去声明一个变量"对它是**误导**
		// （照做也不能用）。已有的"包未导入"诊断此前只覆盖类型位，这里补名字位。
		c.errorAt(v.Pos, "package not imported", v.Name,
			"write `import "+v.Name+"` first, then use its members (a directory is a package; core design §11)")
		return nil, nilBound, untyped{}
	}
	c.errorAt(v.Pos, "undeclared name", v.Name,
		"declare it first (var name Type = value) or check the spelling; type and function names are not values")
	return nil, nilBound, untyped{}
}

func (c *Checker) untypedFromConst(cv ConstVal, at parse.Pos) untyped {
	switch cv.Kind {
	case ConstInt:
		return untyped{kind: unInt, ival: cv.Int}
	case ConstUint:
		return untyped{kind: unUint, uval: cv.Uint}
	case ConstFloat:
		return untyped{kind: unFloat, fval: cv.Float}
	case ConstStr:
		return untyped{kind: unStr, sval: cv.Str}
	case ConstBool:
		return untyped{kind: unBool, bval: cv.Bool}
	}
	return untyped{}
}

// lookupEnumByVariant 反查变体所属枚举（裸变体名报错用）。
func (c *Checker) lookupEnumByVariant(variant string) *Enum {
	for _, en := range c.enums {
		if _, ok := en.variantI[variant]; ok {
			return en
		}
	}
	return nil
}

func (c *Checker) checkUnary(v *parse.Unary, expect Type) (Type, bound, untyped) {
	ty, _, info := c.checkExprFull(v.X, expect)
	// 操作数是**未定型字面量**（`!true` / `-1` / `~0`）：类型推迟到使用点（§一），
	// 但一元算符的**结果类型**由操作数的字面量种类定死。这里不能因为 ty == nil 就早退：
	// 早退会让 `!true` 完全没有类型，AIR 侧只好兜底成 `i32`，于是
	// `print.println(!true)` 打印 `0` 而不是 `false`（720 实测），
	// `var b bool = !false` 连声明都建立不起来（"undeclared name b"）。
	if ty == nil {
		switch {
		case info.kind == unNone:
			return nil, nilBound, untyped{}
		case v.Op == "!" && info.kind == unBool:
			return TBool, nilBound, untyped{}
		case v.Op == "-" && (info.kind == unInt || info.kind == unUint || info.kind == unFloat):
			return nil, nilBound, negateInfo(info)
		case v.Op == "~" && (info.kind == unInt || info.kind == unUint):
			return nil, nilBound, info
		}
		c.errorAt(v.Pos, "unary "+v.Op+" operand types do not match", typeOrUn(ty, info),
			"! applies to bool; - and ~ apply to numbers; there is no implicit conversion (red line 1)")
		return nil, nilBound, untyped{}
	}
	switch v.Op {
	case "-":
		if isNumeric(ty) || info.kind == unInt || info.kind == unUint || info.kind == unFloat {
			// -9223372036854775808 与字面量作为整体参与判定（§一）
			if info.kind == unInt && info.ival == -9223372036854775808 {
				c.errorAt(v.Pos, "negating the minimum i64 overflows", "-("+info.text+")",
					"write the literal -9223372036854775808 (judged as a whole), or i64(-9223372036854775807 - 1)")
				return nil, nilBound, untyped{}
			}
			return ty, nilBound, negateInfo(info)
		}
	case "!":
		if isBool(ty) || info.kind == unBool {
			return TBool, nilBound, untyped{}
		}
	case "~":
		if isInt(ty) || info.kind == unInt || info.kind == unUint {
			return ty, nilBound, untyped{}
		}
	}
	c.errorAt(v.Pos, "unary "+v.Op+" operand types do not match", typeOrUn(ty, info),
		"! applies to bool; - and ~ apply to numbers; there is no implicit conversion (red line 1)")
	return nil, nilBound, untyped{}
}

func negateInfo(info untyped) untyped {
	switch info.kind {
	case unInt:
		return untyped{kind: unInt, ival: -info.ival}
	case unUint:
		return untyped{kind: unUint, uval: ^info.uval + 1}
	case unFloat:
		return untyped{kind: unFloat, fval: -info.fval}
	}
	return info
}

func typeOrUn(ty Type, info untyped) string {
	if ty != nil {
		return ty.String()
	}
	switch info.kind {
	case unInt:
		return "integer literal " + info.text
	case unUint:
		return "unsigned literal " + info.text
	case unFloat:
		return "float literal " + info.text
	case unStr:
		return "string literal"
	case unBool:
		return "bool literal"
	}
	return "?"
}

func isBool(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.Name == "bool"
}

func exprName(e parse.Expr) string {
	switch e.(type) {
	case *parse.Assign:
		return "assignment expression"
	case *parse.MultiAssign:
		return "multi-target assignment"
	}
	return "expression"
}
