package types

import (
	"strings"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// std 最小类型面（逐条边界语义以语料冻结【O9】）。
// 这里只做类型化：print 任意单值 / str 包函数 / os / math / option / testing。
// sync/net/time 属圆 3；sort AIC 自举（圆 1）。
// ---------------------------------------------------------------------------

// stdType 返回 import 包内的具名类型（option.Option[T] 等）。
func stdType(pkg, name string, c *Checker) Type {
	if pkg == "time" {
		return nil // time 无类型，只有函数（见 stdFunc）
	}
	if pkg == "ctx" && name == "Ctx" {
		return ctxClass()
	}
	return nil
}

// ctxClass 是 `Ctx` 约定：
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

// resultEnum 构造 std 的 Result[T]（根类型，懒建一次）：
// enum Result[T] { Ok(T), Fail(Err) }（《AIC 标准库规范》§1 冻结）。
// 惯用：E-9 的失败通道标准形态 = chan[Result[T]]。
var resultOnce *Enum

func resultEnum() *Enum {
	if resultOnce != nil {
		return resultOnce
	}
	en := &Enum{
		Name:       "Result",
		Pkg:        "std", // 根类型：mangling 与 Option 口径一致（不用 option 这个旧包名）
		HasData:    true,
		TypeParams: []string{"T"},
		variantI:   map[string]int{"Ok": 0, "Fail": 1},
		Variants: []Variant{
			{Name: "Ok", Payload: &TypeParam{Name: "T"}},
			{Name: "Fail", Payload: TErr},
		},
	}
	resultOnce = en
	return en
}

// valueEnum 构造 std 的 Value 动态值枚举（《AIC 标准库规范》§1）：
// 替代运行期反射的"未知结构"场景。
var valueOnce *Enum

func valueEnum() *Enum {
	if valueOnce != nil {
		return valueOnce
	}
	en := &Enum{
		Name:     "Value",
		Pkg:      "std",
		HasData:  true,
		variantI: map[string]int{"Null": 0, "Bool": 1, "I64": 2, "F64": 3, "Str": 4, "List": 5, "Obj": 6},
	}
	// 递归类型：List(Value[]) / Obj(map[str]Value) 指向枚举自身
	// （自引用枚举合法——标签是 u32，负载是值，无大小递归）。
	self := func() Type { return en }
	en.Variants = []Variant{
		{Name: "Null"},
		{Name: "Bool", Payload: TBool},
		{Name: "I64", Payload: TI64},
		{Name: "F64", Payload: TF64},
		{Name: "Str", Payload: TStr},
		{Name: "List", Payload: &Slice{Elem: self()}},
		{Name: "Obj", Payload: &MapT{Key: TStr, Value: self()}},
	}
	valueOnce = en
	return en
}
// logLevelEnum 构造 std 的 LogLevel 枚举（标准库规范 §3.2）。
var logLevelOnce *Enum

func logLevelEnum() *Enum {
	if logLevelOnce != nil {
		return logLevelOnce
	}
	en := &Enum{
		Name:     "LogLevel",
		Pkg:      "std",
		variantI: map[string]int{"Debug": 0, "Info": 1, "Warn": 2, "Error": 3},
		Variants: []Variant{
			{Name: "Debug"}, {Name: "Info"}, {Name: "Warn"}, {Name: "Error"},
		},
	}
	logLevelOnce = en
	return en
}

// durationType 返回 std 的 Duration 结构体（《AIC 标准库规范》§1）：
// struct Duration derive Compare { var ms i64 }——真 struct，非别名（S-9）。
// 构造走 time.Sec/Ms/Us：单位进类型，禁裸毫秒参数。
var durationOnce *Class

func durationType() *Class {
	if durationOnce != nil {
		return durationOnce
	}
	cl := &Class{
		Name:     "Duration",
		Pkg:      "std",
		Fields:   []Field{{Name: "ms", Type: TI64}},
		fieldIdx: map[string]int{"ms": 0},
		Methods:  map[string]*FuncSig{},
		Derived:  map[string]bool{"Compare": true},
	}
	durationOnce = cl
	return cl
}

// optionInstance 构造 Option[T] 实例（stdlib 内部共用，禁第二份定义）。
func optionInstance(elem Type) Type {
	return &Instance{Base: optionEnum(), Args: []Type{elem}}
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
	// fmt：程序输出（数据，stdout）。《AIC 标准库规范》§3.1：
	//   print(s str)    — stdout，不追加换行
	//   println(s str)  — stdout，追加 '\n'
	//   eprint(s str)   — stderr，不追加换行
	//   eprintln(s str) — stderr，追加 '\n'
	// 签名即契约：**只收 str**（格式化唯一入口是字符串插值，不开第二条
	// "自动 ToString" 的路）。写失败记 sticky sink，main 退出码 4。
	if pkg == "fmt" {
		switch fnName {
		case "print", "println", "eprint", "eprintln":
		default:
			c.errorAt(v.Pos, "no such function in the fmt package", "fmt."+fnName,
				"the fmt surface is print / println / eprint / eprintln / flush; all output takes a single str")
			return nil, nilBound, untyped{}
		}
		if len(v.Args) != 1 {
			c.errorAt(v.Pos, "fmt."+fnName+" takes exactly one str", "fmt."+fnName+"(s)",
				"format with string interpolation first: fmt.println(\"x={v}\")")
			return nil, nilBound, untyped{}
		}
		// 参数必须是 str——这是契约。多返回值不能打印。
		at, _, ai := c.checkExprFull(v.Args[0], TStr)
		if mt, isMulti := at.(*MultiType); isMulti {
			c.errorAt(parse.ExprPos(v.Args[0]), "a multi-result call cannot be printed",
				"the argument produces "+mt.String(),
				"bind it first: var v, err = f(); then print a formatted str")
			c.types[v] = nil
			return nil, nilBound, untyped{}
		}
		if at != nil {
			c.requireAssignableAt(v.Args[0], at, ai, TStr, "fmt."+fnName+" argument")
		}
		c.types[v] = nil // fmt 无返回值
		return nil, nilBound, untyped{}
	}
	// 没有 `option` 包 —— Option/Some/None 是根类型（《AIC 标准库规范》§1）。
	// 旧助手面（option.ok/none/isSome/isNone/unwrap）随之删除：match 是穷尽的，
	// 不需要那套查询函数。命中旧拼写会给"no such function"（下面统一报错）。
	if pkg == "testing" {
		// 面 = eq/gt/lt，签名恒为 (实得, 期望, "note")：三参、前两值同型、
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
	// `log.*Json`（《AIC 标准库规范》§3.2：`log.infoJson[T: Json](v T)`）——
	// 结构化日志的唯一入口，不引入第二套字段 API。
	// stdFuncSig 里这条签名的形参只能占位成 TStr（表达不了 `[T: Json]` 的
	// 约束），**实参判定必须落在这里**：少了这个分支，`log.infoJson(Event{…})`
	// 会被判成 ok.Event 写进 str（034 实测）。约束走 satisfiesConstraints 的
	// 单一来源（constraint.go），不另立一份 Json 判据。
	if pkg == "log" && strings.HasSuffix(fnName, "Json") {
		if len(v.Args) != 1 {
			c.errorAt(v.Pos, "log."+fnName+" takes exactly one value", "log."+fnName+"(v)",
				"structured logging takes a single value (AIC standard library spec §3.2)")
			return nil, nilBound, untyped{}
		}
		at, _, _ := c.checkExprFull(v.Args[0], nil)
		if at != nil {
			c.satisfiesConstraints(parse.ExprPos(v.Args[0]), "log."+fnName, "T", at, []string{"Json"})
		}
		c.types[v] = nil // log.*Json 无返回值
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
	case "code":
		// `Code.nameOf(code)`：把错误码翻译回枚举名（G3 一切可枚举，§7.2）。
		// 编译器从构建图内的全部 ErrCode 枚举生成这张表。
		if name == "nameOf" {
			return stdSig{[]string{"code"}, []Type{TI32}, []Type{TStr}}, true
		}
	case "value":
		// 动态值助手（《AIC 标准库规范》§1）
		switch name {
		case "kind":
			return stdSig{[]string{"v"}, []Type{valueEnum()}, []Type{TStr}}, true
		case "get":
			return stdSig{[]string{"v", "k"}, []Type{valueEnum(), TStr}, []Type{optionInstance(valueEnum())}}, true
		case "getIdx":
			return stdSig{[]string{"v", "i"}, []Type{valueEnum(), TI64}, []Type{optionInstance(valueEnum())}}, true
		case "asStr":
			return stdSig{[]string{"v"}, []Type{valueEnum()}, []Type{optionInstance(TStr)}}, true
		case "asI64":
			return stdSig{[]string{"v"}, []Type{valueEnum()}, []Type{optionInstance(TI64)}}, true
		case "asF64":
			return stdSig{[]string{"v"}, []Type{valueEnum()}, []Type{optionInstance(TF64)}}, true
		case "asBool":
			return stdSig{[]string{"v"}, []Type{valueEnum()}, []Type{optionInstance(TBool)}}, true
		case "asList":
			return stdSig{[]string{"v"}, []Type{valueEnum()}, []Type{optionInstance(&Slice{Elem: valueEnum()})}}, true
		case "asObj":
			return stdSig{[]string{"v"}, []Type{valueEnum()}, []Type{optionInstance(&MapT{Key: TStr, Value: valueEnum()})}}, true
		}
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
			// 未命中 = Option.None（《AIC 标准库规范》§2.1：不存在 = Option，不用 -1）
			return stdSig{[]string{"s", "p"}, []Type{TStr, TStr}, []Type{optionInstance(TI64)}}, true
		case "fromI64":
			return stdSig{[]string{"v"}, []Type{TI64}, []Type{TStr}}, true
		case "fromI64Base":
			// base ∈ [2,36]；越界 = trap
			return stdSig{[]string{"v", "base"}, []Type{TI64, TI32}, []Type{TStr}}, true
		case "fromU64":
			return stdSig{[]string{"v"}, []Type{TU64}, []Type{TStr}}, true
		case "fromBool":
			return stdSig{[]string{"v"}, []Type{TBool}, []Type{TStr}}, true
		case "fromF64":
			return stdSig{[]string{"v"}, []Type{TF64}, []Type{TStr}}, true
		case "fromF64Fix":
			// digits ∈ [0,17]；越界 = trap
			return stdSig{[]string{"v", "digits"}, []Type{TF64, TI32}, []Type{TStr}}, true
		case "fromRune":
			return stdSig{[]string{"v"}, []Type{TRune}, []Type{TStr}}, true
		case "fromRunes":
			// 非法标量值 = trap（C24）
			return stdSig{[]string{"v"}, []Type{&Slice{Elem: TRune}}, []Type{TStr}}, true
		case "toI64":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{TI64, TErr}}, true
		case "toF64":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{TF64, TErr}}, true
		case "toBytes":
			// 拷贝（str 不可变）
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{&Slice{Elem: TU8}}}, true
		case "fromBytes":
			// UTF-8 校验显式（RFC 3629，拒代理区）
			return stdSig{[]string{"b"}, []Type{&Slice{Elem: TU8}}, []Type{TStr, TErr}}, true
		case "quote":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{TStr}}, true
		case "rawQuote":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{TStr}}, true
		case "hex":
			return stdSig{[]string{"b"}, []Type{&Slice{Elem: TU8}}, []Type{TStr}}, true
		case "fromHex":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{&Slice{Elem: TU8}, TErr}}, true
		case "utf8At":
			return stdSig{[]string{"s", "i"}, []Type{TStr, TUsize}, []Type{TRune, TUsize}}, true
		case "codepoints":
			return stdSig{[]string{"s"}, []Type{TStr}, []Type{&Slice{Elem: TRune}}}, true
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
			// sleep(d Duration)：任务让出（非线程阻塞）。单位进类型（P3）。
			return stdSig{[]string{"d"}, []Type{durationType()}, nil}, true
		case "after":
			// `time.after(ms)` 返回**单调毫秒截止点**（select 的时间分支就绪条件）。
			return stdSig{[]string{"ms"}, []Type{TI64}, []Type{TI64}}, true
		case "Sec", "Ms", "Us":
			// Duration 构造（《AIC 标准库规范》§5.3）：单位进类型，禁裸毫秒参数
			return stdSig{[]string{"n"}, []Type{TI64}, []Type{durationType()}}, true
		case "nowSec", "nowMs", "monotonicMs":
			return stdSig{nil, nil, []Type{TI64}}, true
		}
	case "log":
		// 运维遥测（标准库规范 §3.2）：4 级 + enabled 零成本守卫
		switch name {
		case "debug", "info", "warn", "error":
			return stdSig{[]string{"s"}, []Type{TStr}, nil}, true
		case "debugJson", "infoJson", "warnJson", "errorJson":
			// 结构化日志唯一入口 = derive Json（不引入第二套字段 API）。
			// 签名占位为 TStr：实际参数可以是任意类型，由 checkStdCall 按
			// Json 派生关系放行（见 checkStdCall 的 logJson 分支）。
			return stdSig{[]string{"v"}, []Type{TStr}, nil}, true
		case "setLevel", "enabled":
			// 级别参数是 LogLevel 枚举（不是 i32）。enabled **返回 bool**
			// （《AIC 标准库规范》§3.2）—— 漏了返回位会让 `if log.enabled(…)`
			// 拿不到条件类型（034 实测）。
			res := []Type(nil)
			if name == "enabled" {
				res = []Type{TBool}
			}
			return stdSig{[]string{"l"}, []Type{logLevelEnum()}, res}, true
		case "level":
			return stdSig{nil, nil, []Type{logLevelEnum()}}, true
		}
	case "ctx":
		switch name {
		case "new":
			return stdSig{nil, nil, []Type{ctxClass()}}, true
		}
	// math 包已取消（《AIC 标准库规范》）：标量数值函数进基本类型方法集。
	// 留这个分支会让人以为 `import math` 还有意义。
	}
	return stdSig{}, false
}

// checkErrCtor: Err(code, msg) 内建构造（E-8：首参必须 ErrCode 枚举值）。
// 错误码**不接受**裸整数——码由编译器按（模块路径哈希, 变体序号）分配，
// 手写数字必然撞码（《AIC 语言规范》§7.2）。
func (c *Checker) checkErrCtor(v *parse.Call, isWrap bool) (Type, bound, untyped) {
	if isWrap {
		if len(v.Args) != 3 {
			c.errorAt(v.Pos, "Err.wrap needs (code, msg, cause)",
				"got "+itoa(len(v.Args))+" arguments",
				"write Err.wrap(ErrCode.Variant, \"message\", inner); the inner error becomes the cause")
			return nil, nilBound, untyped{}
		}
		c.checkErrCodeArg(v.Args[0], "Err.wrap code")
		mt, _, mi := c.checkExprFull(v.Args[1], TStr)
		if mt != nil {
			c.requireAssignableAt(v.Args[1], mt, mi, TStr, "Err.wrap msg")
		} else if mi.kind != unStr {
			c.errorAt(parse.ExprPos(v.Args[1]), "the error message must be a str", exprText(v.Args[1]),
				"write a message string: Err.wrap(Code, \"message\", inner)")
		}
		kt, _, ki := c.checkExprFull(v.Args[2], TErr)
		if kt != nil {
			c.requireAssignableAt(v.Args[2], kt, ki, TErr, "Err.wrap cause")
		} else if ki.kind != unNil {
			c.errorAt(parse.ExprPos(v.Args[2]), "the cause must be an Err", exprText(v.Args[2]),
				"pass the inner error value (or nil): Err.wrap(Code, \"message\", inner) (AIC language spec §7.2)")
		}
		return TErr, bound{off: c.regionOff, exact: true}, untyped{}
	}
	if len(v.Args) != 2 {
		c.errorAt(v.Pos, "Err construction needs (code, msg)",
			"got "+itoa(len(v.Args))+" arguments",
			"write Err(ErrCode.Variant, \"message\"); code 0 is reserved for no error")
		return nil, nilBound, untyped{}
	}
	c.checkErrCodeArg(v.Args[0], "Err code")
	mt, _, mi := c.checkExprFull(v.Args[1], TStr)
	if mt != nil {
		c.requireAssignableAt(v.Args[1], mt, mi, TStr, "Err msg")
	} else if mi.kind != unStr {
		c.errorAt(parse.ExprPos(v.Args[1]), "the error message must be a str", exprText(v.Args[1]),
			"write a message string: Err(ErrCode.Variant, \"message\")")
	}
	return TErr, bound{off: c.regionOff, exact: true}, untyped{}
}

// checkErrCodeArg 校验 Err 构造首参：必须是 ErrCode 约束枚举（E-8）。
// 裸整数 = 编译错误（码由编译器分配，手写数字必然撞码）。
func (c *Checker) checkErrCodeArg(e parse.Expr, what string) {
	at, _, u := c.checkExprFull(e, nil)
	if at == nil {
		// 未定型整数字面量（`Err(10, "bad")`）： 把定型推迟到使用点，
		// 这里就是使用点 —— 只看 at 会**整条静默放行**（010 语料实测）。
		if u.kind == unInt || u.kind == unUint {
			c.errorAt(parse.ExprPos(e), "a bare integer is not an error code",
				what+": "+u.text+" literal",
				"write an ErrCode variant: Err(PkgErr.Variant, \"msg\") — codes are assigned by the compiler from the module path hash, so hand-written numbers collide (AIC language spec §7.2, E-8)")
		}
		return // 解析失败已在别处报错
	}
	if en, ok := at.(*Enum); ok && en.ErrCode {
		return
	}
	if b, ok := at.(*Basic); ok {
		c.errorAt(parse.ExprPos(e), "a bare integer is not an error code",
			what+": "+b.Name+" literal",
			"write an ErrCode variant: Err(PkgErr.Variant, \"msg\") — codes are assigned by the compiler from the module path hash, so hand-written numbers collide (AIC language spec §7.2, E-8)")
		return
	}
	c.errorAt(parse.ExprPos(e), "the error code must be an ErrCode enum",
		what+": "+at.String(),
		"declare `enum PkgErr: ErrCode { ... }` and write Err(PkgErr.Variant, \"msg\") (AIC language spec §7.2)")
}

// requirePrintable 校验一个值能否打印：
//   - class（含 struct）必须带 derive ToString——没有 ToString 的 class 是编译错；
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
	c.errorAt(pos, "printing a class needs derive ToString",
		"type "+bad.String()+" has no ToString",
		"annotate the class with derive ToString, or print its fields individually (core design §14)")
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

// stdOptionEnum: Option/Some/None 是根类型，无需 import（《AIC 标准库规范》§1）。
func (c *Checker) stdOptionEnum() (*Enum, bool) {
	return optionEnum(), true
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
		// 直接静默返回会让整条声明消失，只剩下游"undeclared name"的级联诊断。
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