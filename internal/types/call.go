package types

import (
	"strings"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// 调用 / 成员访问 / 下标（容器面、 str、 构造、
// FFI 句柄不涉区域）。
// ---------------------------------------------------------------------------

func (c *Checker) checkCall(v *parse.Call, expect Type) (Type, bound, untyped) {
	// 包函数：print.println(...) / str.utf8At(...) —— Fn = Field(Ident 包名)
	if field, ok := v.Fn.(*parse.Field); ok {
		if id, isId := field.X.(*parse.Ident); isId {
			if _, isPkg := c.imports[id.Name]; isPkg {
				if c.depInfo(id.Name) != nil {
					return c.checkDepCall(v, id.Name, field.Name, expect)
				}
				return c.checkStdCall(v, id.Name, expect)
			}
		}
	}
	// 包函数的裸前缀（parse 允许直接 IDENT 形式，防御）
	if id, ok := v.Fn.(*parse.Ident); ok {
		if _, isPkg := c.imports[id.Name]; isPkg {
			if c.depInfo(id.Name) != nil {
				return c.checkDepCall(v, id.Name, id.Name, expect)
			}
			return c.checkStdCall(v, id.Name, expect)
		}
	}
	// Err.wrap(code, msg, cause)：内建错误链构造（§16 ④）。Fn = Field{Err, wrap}，
	// 必须在"包成员"分派之前认出来（Err 不是包，否则会报"未声明的名字 Err"）。
	if f, isField := v.Fn.(*parse.Field); isField {
		if base, isID := f.X.(*parse.Ident); isID && base.Name == "Err" {
			if f.Name == "wrap" {
				return c.checkErrCtor(v, true)
			}
			c.errorAt(v.Pos, "Err has no such member", "Err."+f.Name,
				"Err carries code / msg / cause; build a chain with Err.wrap(code, msg, cause) (core design §16 N5 ④)")
			return nil, nilBound, untyped{}
		}
	}
	// 构造 C(args) 或转换 T(x)：Fn 是类型名
	if id, ok := v.Fn.(*parse.Ident); ok {
		if cl, isCl := c.classes[id.Name]; isCl {
			return c.checkConstructor(v, cl)
		}
		if b, isBasic := basicType(id.Name); isBasic {
			if b.Name == "Err" {
				return c.checkErrCtor(v, false)
			}
			return c.checkConversion(v, b)
		}
		if en, isEnum := c.enums[id.Name]; isEnum {
			c.errorAt(v.Pos, "an enum cannot be constructed by a call", en.Name+"(...)",
				"write the variant as "+en.Name+".Variant or Variant(payload)")
			return nil, nilBound, untyped{}
		}
	}
	// 变体构造 Some(x)：Some 是带数据变体名（本包枚举或 std Option）
	if id, ok := v.Fn.(*parse.Ident); ok {
		if en, variant, isVar := c.lookupVariantCtor(id.Name); isVar {
			return c.checkVariantCtor(v, en, variant, expect)
		}
		if id.Name == "Some" {
			if _, hasOpt := c.stdOptionEnum(); hasOpt {
				return c.checkOptionSome(v)
			}
		}
	}
	// channel 构造：chan[T].new()（类型由类型实参给出）
	if field, ok := v.Fn.(*parse.Field); ok && field.Name == "new" {
		if ix, isIx := field.X.(*parse.Index); isIx {
			if id, isID := ix.X.(*parse.Ident); isID && id.Name == "chan" {
				args := append([]parse.Expr{ix.Index}, ix.TypeArgs...)
				tes := make([]parse.TypeExpr, 0, len(args))
				for _, a := range args {
					if te := typeExprOf(a); te != nil {
						tes = append(tes, te)
					}
				}
				nt := &parse.NamedType{Name: "chan", Args: tes, Pos: ix.Pos}
				ct := c.resolveChanType(nt)
				if ct == nil {
					return nil, nilBound, untyped{}
				}
				// `chan[T].new()` = **无界**缓冲（【O2 决议】）；`new(cap)` cap>0 = 有界
				// （满即背压点，串行 L2 下 trap）；cap 0 与省略同义。无 rendezvous 形（规划）。
				if len(v.Args) > 1 {
					c.errorAt(v.Pos, "chan.new takes at most one capacity argument", "chan[T].new(cap)",
						"write chan[T].new() for an unbounded channel, or chan[T].new(cap) with cap > 0 for backpressure (core design §16 N11)")
					return nil, nilBound, untyped{}
				}
				if len(v.Args) == 1 {
					at, _, ainfo := c.checkExprFull(v.Args[0], TUsize)
					if at != nil {
						c.requireAssignableAt(v.Args[0], at, ainfo, TUsize, "channel capacity")
					}
					if isNegativeLiteral(v.Args[0]) {
						c.errorAt(v.Pos, "a channel capacity cannot be negative", "chan.new("+exprText(v.Args[0])+")",
							"a capacity is a count: 0 (or no argument) = unbounded; cap > 0 = bounded with backpressure (core design §16 N11)")
						return nil, nilBound, untyped{}
					}
				}
				return ct, bound{off: c.regionOff, exact: true}, untyped{}
			}
		}
	}
	// 容器构造：set.new() / map.new()（类型来自声明处）
	if field, ok := v.Fn.(*parse.Field); ok {
		if id, isId := field.X.(*parse.Ident); isId && (id.Name == "set" || id.Name == "map") && field.Name == "new" {
			if len(v.Args) > 0 {
				c.errorAt(v.Pos, id.Name+".new() takes no arguments", id.Name+".new(...)",
					"write the element type in the declaration; construction takes no arguments (core design §2.2)")
				return nil, nilBound, untyped{}
			}
			switch e := expect.(type) {
			case *SetT:
				return e, bound{off: c.regionOff, exact: true}, untyped{}
			case *MapT:
				return e, bound{off: c.regionOff, exact: true}, untyped{}
			}
			// 修复文案必须按**实际写的包名**生成：硬编码成 `set` 会让
			// `map.new()` 的诊断教用户写一个 set（照做也不编译）。
			shape := "var s set[i32] = set.new()"
			if id.Name == "map" {
				shape = "var m map[str]i32 = map.new()"
			}
			c.errorAt(v.Pos, id.Name+".new() has no target type",
				"the right-hand type cannot be inferred",
				"write the element type in the declaration: "+shape+" (core design §2.2)")
			return nil, nilBound, untyped{}
		}
	}

	// 限定变体构造 E.Variant(负载)：Fn = Field(Ident 枚举名, 变体名)。
	// 必须排在「方法调用」之前：否则会先去求值 field.X（枚举名不是值）而报
	// 「未声明的名字 Shape」——限定变体构造根本没有接收者。
	if field, ok := v.Fn.(*parse.Field); ok {
		if id, isID := field.X.(*parse.Ident); isID {
			if en, isEnum := c.enums[id.Name]; isEnum {
				if idx, isVar := en.variantI[field.Name]; isVar {
					return c.checkVariantCtor(v, en, en.Variants[idx], expect)
				}
			}
		}
		// 跨包限定变体构造 pkg.E.Variant(负载)：Fn = Field(Field(pkg, E), Variant)
		if inner, isField := field.X.(*parse.Field); isField {
			if pid, isID := inner.X.(*parse.Ident); isID {
				if en, va, found := c.depVariant(pid.Name, inner.Name, field.Name, v.Pos); found {
					return c.checkVariantCtor(v, en, va, expect)
				}
			}
		}
	}

	// 编译期内省原语：typeName(T) / sizeOf(T) / isValueType(T)。
	if name := comptimeIntrinsic(v); name != "" {
		return c.checkComptimeCall(v, name, expect)
	}
	// 类型级方法调用：`Class.method(args)` —— 方法带 NoRecv（如 derive Default
	// 生成的 default()），接收者是**类型名**而不是值，故必须在求值接收者之前处理。
	if field, ok := v.Fn.(*parse.Field); ok {
		if id, isID := field.X.(*parse.Ident); isID {
			if cl, isCl := c.classes[id.Name]; isCl {
				if sig, has := cl.Method(field.Name); has && sig.NoRecv {
					return c.checkInvoke(v, sig, nil, bound{}, expect)
				}
			}
		}
	}
	// map/set 构造：`map[K]V.new()` / `set[T].new()`（唯一构造 = .new()）
	// **必须在通用"方法调用"分支之前**：那里会把 map[K]V 当值求值，
	// 报出"未声明的名字 map"这种与真实错误无关的诊断（与泛型类同题）。
	if field, ok := v.Fn.(*parse.Field); ok && field.Name == "new" {
		// `map[K]V.new()` / `set[T].new()`：接收者是**表达式位类型构造子**
		//（parse.TypeExprNode —— 值类型 V 在 `]` 之外，普通 Index 表达不了）。
		if tn, isTN := field.X.(*parse.TypeExprNode); isTN {
			// set[T] 只有一个类型实参（Val 为 nil）；map[K]V 有两个。
			key := c.resolveType(tn.Key)
			if key == nil {
				return nil, nilBound, untyped{}
			}
			var ct Type
			if tn.Val != nil {
				val := c.resolveType(tn.Val)
				if val == nil {
					return nil, nilBound, untyped{}
				}
				if !hashable(key) {
					c.errorAt(tn.Pos, "the map key type is not hashable", typeText(key),
						"a key allows numeric / bool / str / payload-free enum / struct only (AIC language spec §4)")
					return nil, nilBound, untyped{}
				}
				if _, ok := RuntimeMapKeySuffix(key); !ok {
					c.errorAt(tn.Pos, "the map key type has no runtime instance", typeText(key),
						"keys available today: i32 / i64 / u32 / u64 / usize / bool / str (AIC language spec §4)")
					return nil, nilBound, untyped{}
				}
				ct = &MapT{Key: key, Value: val}
			} else {
				ct = &SetT{Elem: key}
			}
			if len(v.Args) > 0 {
				c.errorAt(v.Pos, tn.Name+".new takes no argument", tn.Name+"[..].new()",
					"a container starts empty: "+tn.Name+"[..].new() (AIC standard library spec §2.3)")
				return nil, nilBound, untyped{}
			}
			return ct, bound{off: c.regionOff, exact: true}, untyped{}
		}
	}
	// 泛型类的静态调用：`Box[i64].init(42)`。
	// Fn = Field(Index(Ident "Box", Index=i64), "init")。
	// **必须在 245 的通用"方法调用"分支之前**——那里会把 Box[i64] 当值求值，
	// 报出"未声明的名字 Box"这种与真实错误无关的诊断（诊断审计实测，err/280）。
	// 判据：泛型类按调用点实例化（§6.1），init 是类级函数（接收者是实例，非 this），
	// 与 `Box[T]{...}` 复合字面量互补。
	if field, ok := v.Fn.(*parse.Field); ok {
		if ix, isIx := field.X.(*parse.Index); isIx {
			if id, isID := ix.X.(*parse.Ident); isID {
				if cl, isCl := c.classes[id.Name]; isCl && len(cl.TypeParams) > 0 {
					return c.checkGenericStaticCall(v, ix, cl, field.Name, expect)
				}
			}
		}
	}
	// 方法调用 recv.method(args)
	if field, ok := v.Fn.(*parse.Field); ok {
		// **类名接收者**（`Box.init()` / `Box.something()`）：接收者是类型名而不是值。
		// 必须先按类型解析 —— 否则会先把它当值检查，报出"未声明的名字 Box"这种
		// 与真实错误（"该类没有 init 构造"）无关的诊断（诊断审计实测，err/280 冻的
		// 正是这条错）。
		if id, isID := field.X.(*parse.Ident); isID {
			if cl, isCl := c.classes[id.Name]; isCl {
				if field.Name == "init" {
					return c.checkConstructor(v, cl)
				}
				c.errorAt(v.Pos, "a class name is not a value", cl.Name+"."+field.Name,
					"construct with "+cl.Name+"{field: value} (no init) or "+cl.Name+"(args) (with init); methods need an instance (core design §4)")
				return nil, nilBound, untyped{}
			}
		}
		before := len(c.errors)
		recvTy, recvBound, recvInfo := c.checkExprFull(field.X, nil)
		if recvTy == nil {
			// **字面量接收者**：按字面量的默认类型定型（`"abc".isEmpty()` / `(1).…` 这类
			// 最常用形态）。默认类型由 literalDefault 给（字面量取最窄）。
			if dt := c.literalDefault(recvInfo, parse.ExprPos(field.X)); dt != nil {
				recvTy, recvBound = dt, bound{}
			}
		}
		if recvTy != nil {
			return c.checkMethodCall(v, field, recvTy, recvBound, expect)
		}
		if len(c.errors) > before {
			// 接收者**自身**已经报过错（未声明的名字等）：不再叠加第二条诊断
			// （一条错刷成两条是级联噪音，会把真正的首因埋掉）。
			return nil, nilBound, untyped{}
		}
		// 接收者定型失败**必须报错**：静默返回会让 `"abc".bogus()` 过 check 而在
		// build 阶段炸出无关诊断（"实参类型未知"），是 check↔build 分叉的根因。
		c.errorAt(v.Pos, "cannot resolve the type of the receiver", exprText(field.X),
			"the receiver of ."+field.Name+" must be a value with a known type: annotate it (var x T = …) or use a typed expression (core design §4)")
		return nil, nilBound, untyped{}
	}
	// lambda / 函数类型变量的调用
	if id, ok := v.Fn.(*parse.Ident); ok {
		if sym, found := c.scope.Lookup(id.Name); found && sym.Kind == SymVar {
			sym.Used = true // C22：调用闭包值 = 一次读（不走 checkIdent，这里补）
			if ft, isFnT := sym.Type.(*FuncT); isFnT {
				// 记录被调名自身的类型：AIR/发射侧靠它区分「直接调用自由函数」与
				// 「调用闭包值」（后者要带环境）。
				if c.types != nil {
					c.types[id] = ft
				}
				res := ft.Results
				if len(res) == 0 {
					res = resultsOrNil(ft.Result)
				}
				return c.checkInvoke(v, &FuncSig{Name: id.Name, Params: make([]string, len(ft.Params)),
					ParamTypes: ft.Params, Results: res}, nil, bound{}, expect)
			}
		}
	}
	// 显式泛型实参的调用：`id[i32](x)` / `id[Box[i32]](b)`（Fn 是 Index 形态）。
	if ix, isIx := v.Fn.(*parse.Index); isIx {
		if id, isID := ix.X.(*parse.Ident); isID {
			if sig, isFn := c.funcs[id.Name]; isFn && len(sig.TypeParams) > 0 {
				return c.checkGenericCallExplicit(v, ix, sig, expect)
			}
		}
	}
	// 直接调用自由函数 f(args)
	if id, ok := v.Fn.(*parse.Ident); ok {
		if sig, isFn := c.funcs[id.Name]; isFn {
			// 泛型函数：先用调用点实参解出类型形参，再按实例签名检查（T1 单态化）。
			if len(sig.TypeParams) > 0 {
				return c.checkGenericCall(v, sig, expect)
			}
			return c.checkInvoke(v, sig, nil, bound{off: 0}, expect)
		}
		if _, isPkg := c.imports[id.Name]; !isPkg {
			if isComptimeOnlyName(id.Name) {
				// 内省原语只在编译期可用 —— 报"没有运行期形态"，而不是"未声明的函数"。
				c.errorAt(v.Pos, "compile-time introspection has no runtime form", id.Name,
					id.Name+"(T) exists in constant positions only (const / comptime block / [T;N] size): the compiler emits no type metadata (core design §16 N4)")
				return nil, nilBound, untyped{}
			}
			c.errorAt(v.Pos, "undeclared function", id.Name,
				"check the spelling; a method is object.method(...), a constructor is Class{...} or Class(...)")
		}
		return nil, nilBound, untyped{}
	}
	c.errorAt(v.Pos, "this expression is not callable", "a call",
		"only functions, methods, constructors, and conversions take an argument list")
	return nil, nilBound, untyped{}
}

// checkInvoke 校验实参并返回调用结果类型与界（界 = 调用点偏移）。
//
// recv 非 nil 时为方法调用，但 **ParamTypes 不含接收者槽位**：语义侧的
// FuncSig.ParamTypes 一律只列真实形参（this 由接收者提供，「this 为第一
// 参数」是 C 侧调用约定，不是语义签名的一部分）。此处用 `params[1:]` 去砍
// 首位会让零参方法直接切片越界 panic、有参方法丢掉第一个实参位——两处各写
// 一份「this 占不占槽位」的必然结果（同款病）。
func (c *Checker) checkInvoke(v *parse.Call, sig *FuncSig, recv Type, recvBound bound, expect Type) (Type, bound, untyped) {
	params := sig.ParamTypes
	args := v.Args
	if len(args) != len(params) {
		c.errorAt(v.Pos, "wrong number of arguments",
			sigName(sig)+" needs "+itoa(len(params))+" arguments, got "+itoa(len(args)),
			"match the signature: add or delete arguments")
		return nil, nilBound, untyped{}
	}
	for i, a := range args {
		at, _, ainfo := c.checkExprFull(a, params[i])
		// 多返回值**不可传参**（它是调用点语义，不是值）。只在 var 声明处拦，
		// `print.println(f(3))`（f 带 Err 位）会过 check、到发射才炸出无关诊断
		// （"type (i32, Err) cannot be printed"）—— check 与 build 分叉。
		if mt, isMulti := at.(*MultiType); isMulti {
			c.errorAt(parse.ExprPos(a), "a multi-result call cannot be used as an argument",
				"the argument produces "+mt.String(),
				"bind it first: var v, err = f(); then pass v (or consume the Err with ? / or / catch / !) (core design §2.2)")
			return nil, nilBound, untyped{}
		}
		c.requireAssignableAt(a, at, ainfo, params[i], "arguments "+itoa(i+1))
	}
	// 结果：单一 / 多返回（元组以 []Type 表达）
	var res Type
	if len(sig.Results) == 1 {
		res = sig.Results[0]
	} else if len(sig.Results) > 1 {
		res = &MultiType{Elems: sig.Results}
	}
	off := c.regionOff
	if recv != nil {
		// 方法接收者不延长寿命；结果界仍 = 调用点偏移
		_ = recvBound
	}
	return res, bound{off: off}, untyped{}
}

// MultiType 是调用点语义的多返回值（不可存储/传参）。
type MultiType struct{ Elems []Type }

func (*MultiType) typeNode() {}
func (t *MultiType) String() string {
	s := "("
	for i, e := range t.Elems {
		if i > 0 {
			s += ", "
		}
		s += e.String()
	}
	return s + ")"
}

func (c *Checker) checkConstructor(v *parse.Call, cl *Class) (Type, bound, untyped) {
	var init *FuncSig
	for _, m := range cl.Methods {
		if m.IsInit {
			init = m
			break
		}
	}
	if init == nil {
		c.errorAt(v.Pos, "this class has no init constructor", cl.Name+"(...)",
			"a class without init uses a composite literal: "+cl.Name+"{field: value} (core design §4)")
		return nil, nilBound, untyped{}
	}
	fake := &parse.Call{Fn: v.Fn, Args: v.Args, Pos: v.Pos}
	res, b, _ := c.checkInvoke(fake, init, nil, bound{}, nil)
	_ = res
	// 构造结果 = 类本身，界 = 当前偏移（创建表达式，精确）
	_ = b
	if len(init.Results) > 0 && isErr(init.Results[len(init.Results)-1]) {
		return &MultiType{Elems: append([]Type{cl}, init.Results...)}, bound{off: c.regionOff, exact: true}, untyped{}
	}
	return cl, bound{off: c.regionOff, exact: true}, untyped{}
}

func (c *Checker) checkConversion(v *parse.Call, target *Basic) (Type, bound, untyped) {
	if len(v.Args) != 1 {
		c.errorAt(v.Pos, "a conversion takes exactly one argument", target.Name+"(x)",
			"write "+target.Name+"(expression); a narrowing conversion out of range traps at run time (core design §2.4)")
		return nil, nilBound, untyped{}
	}
	at, _, ainfo := c.checkExprFull(v.Args[0], target)
	if at == nil && (ainfo.kind == unInt || ainfo.kind == unUint || ainfo.kind == unFloat) {
		at = target // 字面量按目标类型做范围证明
	}
	if at == nil {
		return nil, nilBound, untyped{}
	}
	if isUntypedNil(ainfo) {
		c.errorAt(v.Pos, "nil cannot take part in a numeric conversion", target.Name+"(nil)",
			"write the zero value as nil, no conversion needed")
		return nil, nilBound, untyped{}
	}
	switch {
	case isNumeric(at) && isNumeric(target):
		if ainfo.kind == unInt || ainfo.kind == unUint {
			if !c.untypedFits(ainfo, target) {
				c.errorAt(v.Pos, "the literal does not fit the conversion target type",
					target.Name+"("+ainfo.text+")",
					"an out-of-range literal is a compile error (core design §2.4); only a run-time value traps")
				return nil, nilBound, untyped{}
			}
		}
		return target, bound{off: c.regionOff}, untyped{}
	case isStr(at) || isStr(target):
		c.errorAt(v.Pos, "there is no conversion operator between str and numbers", target.Name+"("+at.String()+")",
			"use the str package: fromI64/fromF64/toI64/toF64 (core design §14)")
		return nil, nilBound, untyped{}
	case isBool(at) || isBool(target):
		c.errorAt(v.Pos, "numeric and bool do not convert into each other", target.Name+"("+at.String()+")",
			"write the condition explicitly: x != 0 / if b { 1 } else { 0 }")
		return nil, nilBound, untyped{}
	}
	c.errorAt(v.Pos, "this conversion is not on the surface", target.Name+"("+at.String()+")",
		"explicit conversion is numeric-to-numeric only (a narrowing conversion out of range traps)")
	return nil, nilBound, untyped{}
}

func (c *Checker) lookupVariantCtor(name string) (*Enum, Variant, bool) {
	for _, en := range c.enums {
		if idx, ok := en.variantI[name]; ok {
			return en, en.Variants[idx], true
		}
	}
	return nil, Variant{}, false
}

func (c *Checker) checkVariantCtor(v *parse.Call, en *Enum, va Variant, expect Type) (Type, bound, untyped) {
	if va.Payload == nil {
		// ErrCode 枚举的**无载荷变体直构**（§7.2）：
		// `HttpErr.Timeout("msg")` ≡ `Err(HttpErr.Timeout, "msg", nil)`。
		// 判据：错误码枚举的每个变体就是一条错误，给它一个 str 参数直接造 Err
		// 消掉"Err(Enum.Variant, msg)" 的重复书写——错误路径是最热的路径。
		if en.ErrCode {
			if len(v.Args) != 1 {
				c.errorAt(v.Pos, "an error-code variant takes exactly one message", en.Name+"."+va.Name+"(msg)",
					"write "+en.Name+"."+va.Name+"(\"message\"); it builds Err("+en.Name+"."+va.Name+", \"message\", nil)")
				return nil, nilBound, untyped{}
			}
			mt, _, mi := c.checkExprFull(v.Args[0], TStr)
			if mt != nil {
				c.requireAssignableAt(v.Args[0], mt, mi, TStr, "error message")
			} else if mi.kind != unStr {
				c.errorAt(parse.ExprPos(v.Args[0]), "the error message must be a str", exprText(v.Args[0]),
					"write a message string: "+en.Name+"."+va.Name+"(\"message\")")
			}
			return TErr, bound{off: c.regionOff, exact: true}, untyped{}
		}
		c.errorAt(v.Pos, "this variant carries no payload", en.Name+"."+va.Name,
			"write "+en.Name+"."+va.Name)
		return nil, nilBound, untyped{}
	}
	if len(v.Args) != 1 {
		c.errorAt(v.Pos, "a variant payload is exactly one value", en.Name+"."+va.Name+"(payload)",
			"payload type "+va.Payload.String())
		return nil, nilBound, untyped{}
	}
	// 泛型枚举的变体构造（Option.Some / Result.Ok / Value.I64）：负载类型是
	// 类型形参 T，必须由**上下文期望类型**或实参解出（§6.1 单态化）。
	if len(en.TypeParams) > 0 {
		m := map[string]Type{}
		// 优先用 expect（`var o Option[i64] = Option.Some(i)` 的声明位）
		if inst, ok := expect.(*Instance); ok && inst.Base == en {
			for i, p := range en.TypeParams {
				if i < len(inst.Args) {
					m[p] = inst.Args[i]
				}
			}
		}
		payload := va.Payload
		if len(m) == 0 {
			// 无上下文：从实参解出 T
			at, _, ainfo := c.checkExprFull(v.Args[0], nil)
			if at != nil {
				unifyTypeParam(payload, at, m, 0)
			} else if ainfo.kind == unInt || ainfo.kind == unUint {
				m["T"] = TI64
			}
		}
		if len(m) == 0 {
			c.errorAt(v.Pos, "cannot infer the type argument of "+en.Name, en.Name+"."+va.Name+"(payload)",
				"give the target type in the declaration (var x Option[i64] = ...) or write "+en.Name+"[i64].Some(payload)")
			return nil, nilBound, untyped{}
		}
		// 用解出的实参检查负载
		pt := subst(payload, en.TypeParams, typeArgsOf(en.TypeParams, m))
		at, _, ainfo := c.checkExprFull(v.Args[0], pt)
		if at != nil {
			c.requireAssignableAt(v.Args[0], at, ainfo, pt, "variant payload")
		}
		inst := &Instance{Base: en, Args: typeArgsOf(en.TypeParams, m)}
		return inst, bound{off: c.regionOff, exact: true}, untyped{}
	}
	at, _, ainfo := c.checkExprFull(v.Args[0], va.Payload)
	if at != nil {
		c.requireAssignableAt(v.Args[0], at, ainfo, va.Payload, "variant payload")
	}
	return en, bound{off: c.regionOff, exact: true}, untyped{}
}

func (c *Checker) checkMethodCall(v *parse.Call, field *parse.Field, recvTy Type, recvBound bound, expect Type) (Type, bound, untyped) {
	// enum 限定变体：Color.Red / Option.Some
	if id, ok := field.X.(*parse.Ident); ok {
		if en, isEnum := c.enums[id.Name]; isEnum {
			if idx, isVar := en.variantI[field.Name]; isVar {
				va := en.Variants[idx]
				if va.Payload != nil {
					c.errorAt(v.Pos, "a variant with a payload cannot be referenced directly", en.Name+"."+field.Name,
						"construct with "+en.Name+"."+field.Name+"(payload); destructure with match")
					return nil, nilBound, untyped{}
				}
				c.errorAt(v.Pos, "a payload-free variant cannot be called", en.Name+"."+field.Name,
					"write "+en.Name+"."+field.Name+" directly (no parentheses)")
				return nil, nilBound, untyped{}
			}
			c.errorAt(v.Pos, "the enum has no such variant", en.Name+"."+field.Name,
				"see enum "+en.Name+" declaration")
			return nil, nilBound, untyped{}
		}
	}
	// str 方法
	if isStr(recvTy) {
		return c.checkStrMethod(v, field, recvBound, expect)
	}
	// channel 方法（send/recv/len）
	if sig, ok := chanMethod(recvTy, field.Name); ok {
		return c.checkContainerMethod(v, recvTy, recvBound, sig, recvTy, expect)
	}
	// 容器方法
	if sig, kind, ok := containerMethod(recvTy, field.Name); ok {
		return c.checkContainerMethod(v, recvTy, recvBound, sig, kind, expect)
	}
	// 类方法
	if cl, ok := recvTy.(*Class); ok {
		if m, has := cl.Methods[field.Name]; has {
			if !c.memberVisible(cl.Pkg, cl.Name, field.Name, v.Pos) {
				return nil, nilBound, untyped{}
			}
			c.checkDeref(field.X, "method call "+field.Name)
			return c.checkInvoke(v, m, recvTy, recvBound, expect)
		}
		c.errorAt(v.Pos, "the class has no such method", cl.String()+"."+field.Name,
			"see the class declaration for its methods; field access takes no parentheses")
		return nil, nilBound, untyped{}
	}
	// 泛型类实例的方法：基类方法签名按实例实参代换（Box[i32].get() -> i32）。
	if inst, ok := recvTy.(*Instance); ok {
		if bcl, isCl := inst.Base.(*Class); isCl {
			if m, has := bcl.Methods[field.Name]; has {
				if !c.memberVisible(bcl.Pkg, bcl.Name, field.Name, v.Pos) {
					return nil, nilBound, untyped{}
				}
				// 单态化（mair）要按实例各降一份方法体：把 (方法, 实例实参) 登记下来
				// —— 与泛型自由函数同一条纪律（实例键与符号名同源）。
				if len(bcl.TypeParams) > 0 {
					c.noteMethodInst(m, inst.Args)
				}
				c.checkDeref(field.X, "method call "+field.Name)
				return c.checkInvoke(v, substSigOf(m, bcl.TypeParams, inst.Args), recvTy, recvBound, expect)
			}
			c.errorAt(v.Pos, "the class has no such method", inst.String()+"."+field.Name,
				"see the class declaration for its methods; field access takes no parentheses")
			return nil, nilBound, untyped{}
		}
	}
	// 接口方法
	if ifc, ok := recvTy.(*Interface); ok {
		if m, has := ifc.Methods[field.Name]; has {
			return c.checkInvoke(v, m, recvTy, recvBound, expect)
		}
		c.errorAt(v.Pos, "the interface has no such method", ifc.String()+"."+field.Name,
			"see the interface declaration for its method set")
		return nil, nilBound, untyped{}
	}
	// 泛型形参上的方法调用由**约束**授权（接口约束按签名分发；Ord/Hash 授权
	// compare/hash 这两个 derive 生成的方法）。方法体只写一次，实例化时按约束
	// 直接落到具体方法上。
	if tp, ok := recvTy.(*TypeParam); ok {
		if sig, ok2 := c.constraintMethod(tp.Name, field.Name, tp, v.Pos); ok2 {
			return c.checkInvoke(v, sig, tp, recvBound, expect)
		}
		return nil, nilBound, untyped{}
	}
	// Err 内置方法（《AIC 语言规范》§7.1）：ok / code / is / msg。
	// 判据：错误处理三机制都围着 Err 转，这四个是它的完整观察面——
	// 不给方法就只能让用户解构三个字段，那会破坏"Err 布局冻结"的承诺。
	if _, ok := recvTy.(*Basic); ok && recvTy.String() == "Err" {
		if sig, has := errMethod(field.Name); has {
			return c.checkInvoke(v, sig, recvTy, recvBound, expect)
		}
		c.errorAt(v.Pos, "Err has no such method", "Err."+field.Name,
			"the Err method set is ok / code / is / msg (AIC language spec §7.1)")
		return nil, nilBound, untyped{}
	}
	c.errorAt(v.Pos, "this type has no methods", typeOrUn(recvTy, untyped{})+"."+field.Name,
		"methods exist on classes, interfaces, containers, str and Err only")
	return nil, nilBound, untyped{}
}

// errMethod 返回 Err 的内置方法签名（封闭集，禁第二份）。
func errMethod(name string) (*FuncSig, bool) {
	switch name {
	case "ok":
		return &FuncSig{Name: "ok", Results: []Type{TBool}}, true
	case "code":
		return &FuncSig{Name: "code", Results: []Type{TI32}}, true
	case "is":
		// is(c) 沿 cause 链任层命中（§7.1）；参数 = ErrCode 枚举值。
		// 签名的形参类型用 TErr 占位会导致"枚举写不进来"，故这里对
		// ErrCode 枚举放宽（checkInvoke 的 assignable 判定对 enum→Err 放行）。
		// **返回位 = bool**：漏写 Results 会让本调用的类型恒为 nil ——
		// 检查器因此连"能不能插值"都判不了，IR-1 的 ValueTys 更无从取（017 实测）。
		return &FuncSig{Name: "is", Params: []string{"c"}, ParamTypes: []Type{TErr},
			Results: []Type{TBool}}, true
	case "msg":
		return &FuncSig{Name: "msg", Results: []Type{TStr}}, true
	}
	return nil, false
}

// constraintMethod 在泛型形参的约束集里找方法。
// 找到 = 返回按形参代换过的签名；找不到 = 已报错（返回 false）。
func (c *Checker) constraintMethod(param, method string, tp *TypeParam, at parse.Pos) (*FuncSig, bool) {
	cons := c.constraintsOfParam(param)
	if len(cons) == 0 {
		c.errorAt(at, "this type parameter has no constraint that provides a method", param+"."+method,
			"add a constraint to the type parameter, e.g. [T: Ord] or [T: Shape] (core design §16 N3)")
		return nil, false
	}
	for _, name := range cons {
		switch name {
		case "Compare":
			if method == "compare" {
				return &FuncSig{Name: "compare", Params: []string{"other"}, ParamTypes: []Type{tp}, Results: []Type{TI32}}, true
			}
		case "Hash":
			if method == "hash" {
				return &FuncSig{Name: "hash", Results: []Type{TU64}}, true
			}
		case "ToString", "Clone", "Json", "value", "ref":
			// ToString = 插值/打印、Clone = clone()、Json = toJson/fromJson，
			// value/ref = 值性；不在此提供方法签名，落到下面的统一报错。
		default:
			ifc := c.lookupInterfaceRef(name)
			if ifc == nil {
				continue
			}
			if m, has := ifc.Methods[method]; has {
				return m, true
			}
		}
	}
	c.errorAt(at, "the constraints on "+param+" do not provide this method", param+"."+method+" with ["+param+": "+strings.Join(cons, " + ")+"]",
		"call a method declared by one of the constraints, or add the interface that declares it (core design §16 N3)")
	return nil, false
}

// ContainerMethodSig 导出容器内建方法签名（AIR 降级的实参装箱要用：
// `m.put(k, v)` 的 v 是具体类而值槽是接口时，IR 里必须显式 box —— 规则只有这一份，
// 不在 air 侧重列一张方法表）。
func ContainerMethodSig(t Type, name string) (*FuncSig, bool) {
	sig, _, ok := containerMethod(t, name)
	return sig, ok
}

// containerMethod 返回容器内建方法签名。
// scalarMethod 返回标量数值方法签名（《AIC 标准库规范》）。
// math 包已取消：`x.sqrt()` 而非 `math.sqrt(x)`（划界第 1 条：纯值变换 = 方法）。
func scalarMethod(ty string, name string, self *Basic) (*FuncSig, bool) {
	switch ty {
	case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize":
		switch name {
		case "abs":
			return &FuncSig{Name: name, Results: []Type{self}}, true
		case "min", "max":
			return &FuncSig{Name: name, Params: []string{"o"}, ParamTypes: []Type{self}, Results: []Type{self}}, true
		case "wrappingAdd", "wrappingSub", "wrappingMul":
			// 显式回绕（S-32）：普通算术溢出是确定性 trap
			return &FuncSig{Name: name, Params: []string{"o"}, ParamTypes: []Type{self}, Results: []Type{self}}, true
		}
	case "f32", "f64":
		switch name {
		case "abs", "floor", "ceil", "round", "trunc", "sqrt", "cbrt",
			"exp", "log", "log2", "log10", "sin", "cos", "tan",
			"asin", "acos", "atan":
			return &FuncSig{Name: name, Results: []Type{self}}, true
		case "pow", "atan2":
			return &FuncSig{Name: name, Params: []string{"o"}, ParamTypes: []Type{self}, Results: []Type{self}}, true
		case "isNaN", "isInf", "isFinite":
			return &FuncSig{Name: name, Results: []Type{TBool}}, true
		}
	case "rune":
		switch name {
		case "toI64":
			return &FuncSig{Name: name, Results: []Type{TI64}}, true
		}
	}
	return nil, false
}

func containerMethod(t Type, name string) (*FuncSig, Type, bool) {
	switch cv := t.(type) {
	case *Basic:
		if sig, ok := scalarMethod(cv.Name, name, cv); ok {
			return sig, t, true
		}
	case *Slice:
		switch name {
		case "len":
			return &FuncSig{Name: name, Results: []Type{TUsize}}, t, true
		case "isEmpty":
			return &FuncSig{Name: name, Results: []Type{TBool}}, t, true
		case "append":
			return &FuncSig{Name: name, Params: []string{"v"}, ParamTypes: []Type{cv.Elem}}, t, true
		case "pop":
			return &FuncSig{Name: name, Results: []Type{cv.Elem, TBool}}, t, true
		case "insert":
			return &FuncSig{Name: name, Params: []string{"i", "v"}, ParamTypes: []Type{TUsize, cv.Elem}}, t, true
		case "remove":
			// 只给 Params 不给 ParamTypes 会让检查器按 0 参放行、发射侧却取 arg0
			// （`xs.remove(i)` 报"needs 0 arguments"、`xs.remove()` 过 check 后 build 失败）。
			return &FuncSig{Name: name, Params: []string{"i"}, ParamTypes: []Type{TUsize}}, t, true
		case "clear":
			return &FuncSig{Name: name}, t, true
		case "slice":
			// 视图切片 `xs[a..b]`（S-21）：共享底层，越界 trap
			return &FuncSig{Name: name, Params: []string{"a", "n"}, ParamTypes: []Type{TI64, TI64},
				Results: []Type{&Slice{Elem: cv.Elem}}}, t, true
		case "first", "last":
			// 不存在 = Option（标准库规范 §2.2）
			return &FuncSig{Name: name, Results: []Type{optionInstance(cv.Elem)}}, t, true
		}
	case *MapT:
		switch name {
		case "len":
			return &FuncSig{Name: name, Results: []Type{TUsize}}, t, true
		case "isEmpty":
			return &FuncSig{Name: name, Results: []Type{TBool}}, t, true
		case "put":
			return &FuncSig{Name: name, Params: []string{"k", "v"}, ParamTypes: []Type{cv.Key, cv.Value}}, t, true
		case "get":
			// 不存在 = Option（§2.3），不是零值也不是 Err
			return &FuncSig{Name: name, Params: []string{"k"}, ParamTypes: []Type{cv.Key},
				Results: []Type{optionInstance(cv.Value)}}, t, true
		case "del":
			return &FuncSig{Name: name, Params: []string{"k"}, ParamTypes: []Type{cv.Key}}, t, true
		case "contains":
			return &FuncSig{Name: name, Params: []string{"k"}, ParamTypes: []Type{cv.Key}, Results: []Type{TBool}}, t, true
		case "has":
			return &FuncSig{Name: name, Params: []string{"k"}, ParamTypes: []Type{cv.Key}, Results: []Type{TBool}}, t, true
		case "clear":
			return &FuncSig{Name: name}, t, true
		case "keys":
			// 插入序全量（§2.3）
			return &FuncSig{Name: name, Results: []Type{&Slice{Elem: cv.Key}}}, t, true
		case "keyAt", "valAt":
			rt := cv.Key
			if name == "valAt" {
				rt = cv.Value
			}
			return &FuncSig{Name: name, Params: []string{"i"}, ParamTypes: []Type{TUsize}, Results: []Type{rt}}, t, true
		}
	case *SetT:
		switch name {
		case "len":
			return &FuncSig{Name: name, Results: []Type{TUsize}}, t, true
		case "isEmpty":
			return &FuncSig{Name: name, Results: []Type{TBool}}, t, true
		case "put", "add":
			return &FuncSig{Name: name, Params: []string{"v"}, ParamTypes: []Type{cv.Elem}}, t, true
		case "contains", "has":
			return &FuncSig{Name: name, Params: []string{"v"}, ParamTypes: []Type{cv.Elem}, Results: []Type{TBool}}, t, true
		case "del", "remove":
			return &FuncSig{Name: name, Params: []string{"v"}, ParamTypes: []Type{cv.Elem}}, t, true
		case "clear":
			return &FuncSig{Name: name}, t, true
		case "at":
			return &FuncSig{Name: name, Params: []string{"i"}, ParamTypes: []Type{TUsize}, Results: []Type{cv.Elem}}, t, true
		case "items":
			// 插入序全量（§2.3）
			return &FuncSig{Name: name, Results: []Type{&Slice{Elem: cv.Elem}}}, t, true
		}
	case *ArrayT:
		switch name {
		case "len":
			return &FuncSig{Name: name, Results: []Type{TUsize}}, t, true
		case "isEmpty":
			return &FuncSig{Name: name, Results: []Type{TBool}}, t, true
		}
	}
	return nil, nil, false
}

func indexOrNil(name string, _ Type) []string {
	if name == "remove" {
		return []string{"i"}
	}
	return nil
}

func boolOrNil(name string) []Type {
	if name == "has" {
		return []Type{TBool}
	}
	return nil
}

// checkContainerMethod 校验容器方法实参并打存储点守卫标记（③ append/put/add）。
func (c *Checker) checkContainerMethod(v *parse.Call, recvTy Type, recvBound bound, sig *FuncSig, _ Type, expect Type) (Type, bound, untyped) {
	// 内建方法签名同样只列真实形参（与 buildSig 的类方法一致）：接收者由调用点提供。
	res, b, _ := c.checkInvoke(v, sig, recvTy, recvBound, expect)
	// append/put/add 的实参是元素存储：目标深度 = 容器对象深度（≤ 接收者界）。
	switch sig.Name {
	case "append", "put", "add":
		if len(v.Args) > 0 {
			c.markStoreGuard(v.Args[0], recvBound, "container element")
		}
	case "insert":
		if len(v.Args) > 1 {
			c.markStoreGuard(v.Args[1], recvBound, "container element")
		}
	}
	return res, b, untyped{}
}

// markStoreGuard 对「把引用型值写入深度 ≤ 接收者界的存储」判定守卫/编译错
// （存储点全清单 ⓪–⑥）。
func (c *Checker) markStoreGuard(value parse.Expr, targetBound bound, what string) {
	vt, vb, _ := c.checkExprFull(value, nil)
	if vt == nil {
		return
	}
	if isValueType(vt) && !containsReference(vt) {
		return // 值类型无引用，免守卫
	}
	if vb.off <= targetBound.off {
		// 静态可证安全 —— **但两个参数之间除外**：参数定理只给出
		// "形参绝对深度 ≤ 入口"，因此参数派生值写本函数**局部变量**恒安全；
		// 而参数 A 的字段/容器（宿主也是参数）与参数 B 的值之间**没有深度顺序**。
		// 一律跳过会让 `func sink(h Holder, t Buf) { h.b = t }` 无守卫，
		// 调用点 `region { var t = Buf{}; sink(h, t) }` 之后 h.b 悬垂、区域复用内存时
		// 静默读到别的对象（**静默内存损坏**）。故双方都是参数派生时留守卫：
		// 该消的仍由逃逸分析 E1/E4 消（局部目标、返回值）。
		if !(vb.param && targetBound.param) {
			return // 静态可证安全
		}
	}
	if vb.exact {
		c.errorAt(parse.ExprPos(value), "a block-local object escapes: stored into a shallower region's "+what,
			"object created at depth "+itoa(vb.off)+", the target storage sits at depth "+itoa(targetBound.off),
			"move the creation into the target storage's region, or add live to raise its level (core design §5 R5)")
		return
	}
	c.guards[value] = GuardSpec{Kind: GuardStore, Dynamic: targetBound.param}
	if containsReference(vt) {
		c.guards[value] = GuardSpec{Kind: GuardComposite, Dynamic: targetBound.param}
	}
}

func sigName(sig *FuncSig) string {
	if sig.Recv != "" {
		return sig.Recv + "." + sig.Name
	}
	return sig.Name
}
