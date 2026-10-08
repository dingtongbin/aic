package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// std 最小类型面（核心设计 §十四；逐条边界语义 R4 以语料冻结【O9】）。
// 这里只做类型化：print 任意单值 / str 包函数 / os / math / option / testing。
// sync/net/time 属圆 3；sort AIC 自举（圆 1）。
// ---------------------------------------------------------------------------

// stdType 返回 import 包内的具名类型（option.Option[T] 等）。
func stdType(pkg, name string, c *Checker) Type {
	if pkg == "time" {
		return nil // time 无类型，只有函数（见 stdFunc）
	}
	if pkg == "sync" && name == "Mutex" {
		return TMutex
	}
	if pkg == "ctx" && name == "Ctx" {
		return ctxClass()
	}
	if pkg == "option" && name == "Option" {
		if en := optionEnum(); en != nil {
			return en
		}
	}
	return nil
}

// ctxClass 是 N12 的 `Ctx` 约定（核心设计 §16 N12）：
//
//	class Ctx { var vals map[str]str  func with(k str, v str) -> Ctx  func get(k str) -> (str, bool) }
//
// **库级**：语言不特殊对待（无隐藏传递、无隐式参数），handler/中间件的首参是
// Ctx 只是框架约定。类型面在检查器里（T-A），实现随标准库包在 T-C 落地。
var ctxOnce *Class

func ctxClass() *Class {
	if ctxOnce != nil {
		return ctxOnce
	}
	cl := &Class{
		Name:     "Ctx",
		Pkg:      "ctx",
		Fields:   []Field{{Name: "vals", Type: &MapT{Key: TStr, Value: TStr}}},
		fieldIdx: map[string]int{"vals": 0},
		Methods:  map[string]*FuncSig{},
	}
	cl.Methods["with"] = &FuncSig{
		Name: "with", Recv: "Ctx",
		Params: []string{"k", "v"}, ParamTypes: []Type{TStr, TStr},
		Results: []Type{cl},
	}
	cl.Methods["get"] = &FuncSig{
		Name: "get", Recv: "Ctx",
		Params: []string{"k"}, ParamTypes: []Type{TStr},
		Results: []Type{TStr, TBool},
	}
	ctxOnce = cl
	return cl
}

// IsOptionInstance 报告 t 是否为 std 的 Option[T] 实例（T1：裸 Some/None 与
// option.* 助手都靠它定型）。
// 注意：不能用 inst.Base.String()=="Option" 判定——Enum.String() 返回限定名
// "option.Option"，早期这样写导致 option.none()/裸 None 整条链失效。
func IsOptionInstance(t Type) (*Instance, bool) {
	inst, ok := t.(*Instance)
	if !ok || inst.Base == nil {
		return nil, false
	}
	en, isEnum := inst.Base.(*Enum)
	if !isEnum || en.Name != "Option" || en.Pkg != "option" || len(inst.Args) != 1 {
		return nil, false
	}
	return inst, true
}

// OptionBaseEnum 暴露 std Option 的基枚举（emit 侧从语法类型 option.Option[T]
// 还原实例时要用到，禁第二份定义）。
func OptionBaseEnum() *Enum { return optionEnum() }

// optionEnum 构造 std 的 Option[T]（懒建一次）。
var optionOnce *Enum

func optionEnum() *Enum {
	if optionOnce != nil {
		return optionOnce
	}
	en := &Enum{
		Name:       "Option",
		Pkg:        "option", // std 类型的包名参与 mangling（Option 的 typedef 名依赖它）
		HasData:    true,
		TypeParams: []string{"T"},
		variantI:   map[string]int{"Some": 0, "None": 1},
		Variants: []Variant{
			{Name: "Some", Payload: &TypeParam{Name: "T"}},
			{Name: "None"},
		},
	}
	optionOnce = en
	return en
}

// checkStdCall 类型化 import包.函数(...) 调用。
func (c *Checker) checkStdCall(v *parse.Call, pkg string, expect Type) (Type, bound, untyped) {
	fnName := ""
	switch f := v.Fn.(type) {
	case *parse.Ident:
		fnName = f.Name
	case *parse.Field:
		fnName = f.Name
	}
	// println 特例：接受任意单值（核心设计 §十四）
	if pkg == "print" {
		if fnName != "println" {
			c.errorAt(v.Pos, "no such function in the print package", "print."+fnName,
				"the print surface has println only (any single value)")
			return nil, nilBound, untyped{}
		}
		if len(v.Args) != 1 {
			c.errorAt(v.Pos, "println takes exactly one value", "print.println(v)",
				"one call prints one value; concatenate several with str + str")
			return nil, nilBound, untyped{}
		}
		// 必须走 checkExpr（而非 checkStmtOnly）：println 的实参类型决定 emit
		// 侧选哪条打印原语，类型表缺这一条就让任意实参形态（如 grade(95)）发不出去。
		at, _, _ := c.checkExprFull(v.Args[0], nil)
		// §二.2：多返回值不是值 ⇒ 不能打印。此前放行，到发射才炸出
		// "type (i32, Err) cannot be printed"（check 与 build 分叉）。
		if mt, isMulti := at.(*MultiType); isMulti {
			c.errorAt(parse.ExprPos(v.Args[0]), "a multi-result call cannot be printed",
				"the argument produces "+mt.String(),
				"bind it first: var v, err = f(); then print v (or consume the Err with ? / or / catch / !) (core design §2.2)")
			c.types[v] = nil
			return nil, nilBound, untyped{}
		}
		c.requirePrintable(v.Args[0], at, v.Pos)
		c.types[v] = nil // println 无返回值（省略时该节点会落到「未定型字面量」路径）
		return nil, nilBound, untyped{}
	}
	if pkg == "option" {
		if res, b, u, handled := c.stdOptionCall(v, fnName, expect); handled {
			return res, b, u
		}
	}
	if pkg == "testing" {
		// 面 = eq/gt/lt（§十四），签名恒为 (实得, 期望, "note")：三参、前两值同型、
		// 说明是 str（失败消息里原样打印）。
		if fnName != "eq" && fnName != "gt" && fnName != "lt" {
			c.errorAt(v.Pos, "no such function in the package", "testing."+fnName,
				"the testing surface has eq / gt / lt only (core design §14)")
			return nil, nilBound, untyped{}
		}
		if len(v.Args) != 3 {
			c.errorAt(v.Pos, "a testing comparison needs three arguments", "testing."+fnName+"(actual, expected, \"note\")",
				"the first two values share a type, the third is a str note; failure traps and prints both values (core design §14)")
			return nil, nilBound, untyped{}
		}
		at, _, _ := c.checkExprFull(v.Args[0], nil)
		bt, _, _ := c.checkExprFull(v.Args[1], at)
		if at != nil && bt != nil && !identical(at, bt) {
			c.errorAt(v.Pos, "the two testing values have different types", at.String()+" and "+bt.String(),
				"compare values of one type; convert explicitly first (red line 1)")
		}
		mt, _, mi := c.checkExprFull(v.Args[2], TStr)
		if mt != nil {
			c.requireAssignableAt(v.Args[2], mt, mi, TStr, "testing note")
		}
		return nil, nilBound, untyped{}
	}
	sig, ok2 := stdFuncSig(pkg, fnName)
	if !ok2 {
		// 修复文案不得指向内部产物（"pitfall list 3" 这种）也不得举不存在的例子。
		// 注：走到这里说明调用方已判定该包是 std 面（用户包由 depInfo 路径处理）；
		// 用户包**检查失败**时也会落到这里，故文案不假设"面由编译器固定"。
		c.errorAt(v.Pos, "no such function in the package", pkg+"."+fnName,
			"check the spelling; if "+pkg+" is a user package, its public functions are declared in its own files, otherwise the std surface is core design §14")
		return nil, nilBound, untyped{}
	}
	fake := &FuncSig{Name: fnName, Params: sig.params, ParamTypes: sig.types, Results: sig.results}
	return c.checkInvoke(v, fake, nil, bound{}, expect)
}

// StdSig 暴露 std 面签名（emit 侧推调用结果类型用；唯一实现仍是 stdFuncSig）。
func StdSig(pkg, name string) (params []Type, results []Type, ok bool) {
	sig, found := stdFuncSig(pkg, name)
	if !found {
		return nil, nil, false
	}
	return sig.types, sig.results, true
}

type stdSig struct {
	params  []string
	types   []Type
	results []Type
}

func stdFuncSig(pkg, name string) (stdSig, bool) {
	switch pkg {
	case "str":
		switch name {
		case "concat":
			return stdSig{[]string{"a", "b"}, []Type{TStr, TStr}, []Type{TStr}}, true
		case "sub":
			return stdSig{[]string{"s", "i", "j"}, []Type{TStr, TUsize, TUsize}, []Type{TStr}}, true
		case "trim":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{TStr}}, true
		case "split":
			return stdSig{[]string{"s", "sep"}, []Type{TStr, TStr}, []Type{&Slice{Elem: TStr}}}, true
		case "startsWith", "endsWith":
			return stdSig{[]string{"s", "p"}, []Type{TStr, TStr}, []Type{TBool}}, true
		case "indexOf":
			// 未命中 = -1（语料 682 冻结），故返回有符号 i64 而非 usize
			return stdSig{[]string{"s", "p"}, []Type{TStr, TStr}, []Type{TI64}}, true
		case "fromI64":
			return stdSig{[]string{"v"}, []Type{TI64}, []Type{TStr}}, true
		case "fromU64":
			return stdSig{[]string{"v"}, []Type{TU64}, []Type{TStr}}, true
		case "fromBool":
			return stdSig{[]string{"v"}, []Type{TBool}, []Type{TStr}}, true
		case "fromF64":
			return stdSig{[]string{"v"}, []Type{TF64}, []Type{TStr}}, true
		case "toI64":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{TI64, TErr}}, true
		case "toF64":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{TF64, TErr}}, true
		case "utf8At":
			return stdSig{[]string{"s", "i"}, []Type{TStr, TUsize}, []Type{TU32, TUsize}}, true
		case "codepoints":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{&Slice{Elem: TU32}}}, true
		}
	case "os":
		switch name {
		case "args":
			return stdSig{nil, nil, []Type{&Slice{Elem: TStr}}}, true
		case "readFile":
			return stdSig{[]string{"path"}, []Type{TStr}, []Type{TStr, TErr}}, true
		case "readStdin":
			return stdSig{nil, nil, []Type{TStr, TErr}}, true
		case "exit":
			return stdSig{[]string{"code"}, []Type{TI32}, nil}, true
		}
	case "time":
		switch name {
		case "now", "monotonic":
			return stdSig{nil, nil, []Type{TI64}}, true
		case "sleep":
			return stdSig{[]string{"ms"}, []Type{TI64}, nil}, true
		case "after":
			// N6：`time.after(ms)` 返回**单调毫秒截止点**（select 的时间分支就绪条件）。
			return stdSig{[]string{"ms"}, []Type{TI64}, []Type{TI64}}, true
		}
	case "ctx":
		switch name {
		case "new":
			return stdSig{nil, nil, []Type{ctxClass()}}, true
		}
	case "math":
		switch name {
		case "abs", "floor", "ceil", "sqrt":
			return stdSig{[]string{"x"}, []Type{TF64}, []Type{TF64}}, true
		case "min", "max":
			return stdSig{[]string{"a", "b"}, []Type{TF64, TF64}, []Type{TF64}}, true
		case "pow":
			return stdSig{[]string{"a", "b"}, []Type{TF64, TF64}, []Type{TF64}}, true
		}
	}
	return stdSig{}, false
}

// checkErrCtor: Err(code i32, msg str) 内建构造（核心设计 §六）。
func (c *Checker) checkErrCtor(v *parse.Call, isWrap bool) (Type, bound, untyped) {
	// §16 N5 ④：`Err.wrap(code, msg, cause)` 建链；`Err(code, msg)` 建无链错误。
	if isWrap {
		if len(v.Args) != 3 {
			c.errorAt(v.Pos, "Err.wrap needs (code i32, msg str, cause Err)",
				"got "+itoa(len(v.Args))+" arguments",
				"write Err.wrap(integer code, \"message\", inner); the inner error becomes the cause")
			return nil, nilBound, untyped{}
		}
		ct, _, ci := c.checkExprFull(v.Args[0], TI32)
		if ct != nil {
			c.requireAssignableAt(v.Args[0], ct, ci, TI32, "Err.wrap code")
		} else if ci.kind != unInt && ci.kind != unUint {
			c.errorAt(parse.ExprPos(v.Args[0]), "the error code must be an integer", exprText(v.Args[0]),
				"write an integer code: Err.wrap(10, \"message\", inner); code 0 is reserved for no error")
		}
		mt, _, mi := c.checkExprFull(v.Args[1], TStr)
		if mt != nil {
			c.requireAssignableAt(v.Args[1], mt, mi, TStr, "Err.wrap msg")
		} else if mi.kind != unStr {
			c.errorAt(parse.ExprPos(v.Args[1]), "the error message must be a str", exprText(v.Args[1]),
				"write a message string: Err.wrap(10, \"message\", inner)")
		}
		kt, _, ki := c.checkExprFull(v.Args[2], TErr)
		if kt != nil {
			c.requireAssignableAt(v.Args[2], kt, ki, TErr, "Err.wrap cause")
		} else if ki.kind != unNil {
			// 只有 nil（= 无 cause）与真正的 Err 值可以进 cause 位；字面量不行（红线 1）。
			c.errorAt(parse.ExprPos(v.Args[2]), "the cause must be an Err", exprText(v.Args[2]),
				"pass the inner error value (or nil): Err.wrap(10, \"message\", inner) (core design §16 N5 ④)")
		}
		return TErr, bound{off: c.regionOff, exact: true}, untyped{}
	}
	if len(v.Args) != 2 {
		c.errorAt(v.Pos, "Err construction needs (code i32, msg str)",
			"got "+itoa(len(v.Args))+" arguments",
			"write Err(integer code, \"message\"); code 0 is reserved for no error")
		return nil, nilBound, untyped{}
	}
	ct, _, ci := c.checkExprFull(v.Args[0], TI32)
	if ct != nil {
		c.requireAssignableAt(v.Args[0], ct, ci, TI32, "Err code")
	}
	mt, _, mi := c.checkExprFull(v.Args[1], TStr)
	if mt != nil {
		c.requireAssignableAt(v.Args[1], mt, mi, TStr, "Err msg")
	}
	return TErr, bound{off: c.regionOff, exact: true}, untyped{}
}

// requirePrintable 校验一个值能否打印（§十四）：
//   - class（含 @packed）必须带 @derive(ToString)——没有 ToString 的 class 是编译错；
//   - 容器/数组/enum 负载递归检查（打印会穿透到元素，规则同源）；
//   - 其余（数值/bool/str/Err/nil/无数据 enum）恒可打印。
//
// 只报一次（at = println 的位置），不给每个元素重复报错。
func (c *Checker) requirePrintable(at parse.Expr, t Type, pos parse.Pos) {
	if t == nil {
		return
	}
	bad := unprintableClass(t, 0)
	if bad == nil {
		return
	}
	c.errorAt(pos, "printing a class needs @derive(ToString)",
		"type "+bad.String()+" has no ToString",
		"annotate the class with @derive(ToString), or print its fields individually (core design §14)")
}

// unprintableClass 返回类型里第一个不可打印的 class（可打印返回 nil）。
func unprintableClass(t Type, depth int) *Class {
	if t == nil || depth > 8 {
		return nil
	}
	if cl, ok := t.(*Class); ok {
		if !cl.HasDerive("ToString") {
			return cl
		}
		for i := range cl.Fields {
			if bad := unprintableClass(cl.Fields[i].Type, depth+1); bad != nil {
				return bad
			}
		}
		return nil
	}
	if types := []Type{SliceElem(t), SetElem(t)}; types[0] != nil || types[1] != nil {
		for _, e := range types {
			if bad := unprintableClass(e, depth+1); bad != nil {
				return bad
			}
		}
		return nil
	}
	if k, v, ok := MapParts(t); ok {
		if bad := unprintableClass(k, depth+1); bad != nil {
			return bad
		}
		return unprintableClass(v, depth+1)
	}
	if elem, _, ok := ArrayElem(t); ok {
		return unprintableClass(elem, depth+1)
	}
	if en, ok := t.(*Enum); ok {
		for _, va := range en.Variants {
			if bad := unprintableClass(va.Payload, depth+1); bad != nil {
				return bad
			}
		}
	}
	return nil
}

// stdOptionEnum: import option 后 Some/None 可用。
func (c *Checker) stdOptionEnum() (*Enum, bool) {
	if c.imports["option"] {
		return optionEnum(), true
	}
	return nil, false
}

// checkOptionSome: Some(x) → Option[实参类型]（负载 TypeParam 由实参定型）。
func (c *Checker) checkOptionSome(v *parse.Call) (Type, bound, untyped) {
	if len(v.Args) != 1 {
		c.errorAt(v.Pos, "Some takes exactly one payload", "Some(v)", "the payload type decides T of Option[T]")
		return nil, nilBound, untyped{}
	}
	at, ab, ainfo := c.checkExprFull(v.Args[0], nil)
	if at == nil {
		// 未定型字面量（`Some(9)`）按**默认类型**定型 —— 与 option.ok 同一口径。
		// 此前直接静默返回 ⇒ 整条声明消失，只剩下游"undeclared name"的级联诊断。
		at = c.literalDefault(ainfo, parse.ExprPos(v.Args[0]))
	}
	if at == nil {
		c.errorAt(v.Pos, "Some cannot infer the payload type", "Some(v)",
			"give the payload a type: Some(x) where x is typed, or write the target type in the declaration (core design §14)")
		return nil, nilBound, untyped{}
	}
	inst := &Instance{Base: optionEnum(), Args: []Type{at}}
	// Some(x) 是创建表达式（装箱在当前区域）
	_ = ab
	return inst, bound{off: c.regionOff, exact: true}, untyped{}
}

// optionUnwrapSuite: option.ok / none / isSome / isWrap 面_type化。
func (c *Checker) stdOptionCall(v *parse.Call, fnName string, expect Type) (Type, bound, untyped, bool) {
	switch fnName {
	case "ok":
		if len(v.Args) != 1 {
			c.errorAt(v.Pos, "option.ok takes exactly one value", "option.ok(v)", "T is decided by the argument")
			return nil, nilBound, untyped{}, true
		}
		at, _, ainfo := c.checkExprFull(v.Args[0], nil)
		if at == nil {
			at = c.literalDefault(ainfo, parse.ExprPos(v.Args[0]))
			if at == nil {
				return nil, nilBound, untyped{}, true
			}
		}
		return &Instance{Base: optionEnum(), Args: []Type{at}}, bound{off: c.regionOff}, untyped{}, true
	case "none":
		if inst, ok := IsOptionInstance(expect); ok {
			return inst, bound{off: c.regionOff}, untyped{}, true
		}
		c.errorAt(v.Pos, "option.none() has no target type", "the right-hand type cannot be inferred",
			"write the element type in the declaration: var o Option[i32] = option.none()")
		return nil, nilBound, untyped{}, true
	case "isSome", "isNone":
		if len(v.Args) != 1 {
			c.errorAt(v.Pos, fnName+" takes exactly one argument", "option."+fnName+"(o)", "the argument is an Option value")
			return nil, nilBound, untyped{}, true
		}
		c.checkStmtOnly(v.Args[0])
		return TBool, nilBound, untyped{}, true
	case "unwrap":
		if len(v.Args) != 1 {
			c.errorAt(v.Pos, "unwrap takes exactly one argument", "option.unwrap(o)", "the argument is an Option value")
			return nil, nilBound, untyped{}, true
		}
		at, _, _ := c.checkExprFull(v.Args[0], nil)
		if inst, ok := IsOptionInstance(at); ok {
			return inst.Args[0], nilBound, untyped{}, true
		}
		c.errorAt(v.Pos, "the argument of unwrap must be an Option value", typeOrUn(at, untyped{}),
			"test isSome first, or destructure with match")
		return nil, nilBound, untyped{}, true
	}
	return nil, nilBound, untyped{}, false
}
