package types

import (
	"sort"
	"strings"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// 类型模型。
//
// 值类型：Basic（数值/bool/str/Err）、Enum、struct、ArrayT、FuncT。
// 引用类型：普通 Class、Interface、Slice/MapT/SetT（容器句柄）。
// 零值：数值 0 / bool false / str "" / 其余引用与 Err = nil。
// ---------------------------------------------------------------------------

type Type interface {
	typeNode()
	String() string
}

// Basic is a primitive or the builtin Err.
type Basic struct{ Name string }

// Class is a user class; Struct flips it to value semantics (struct).
type Class struct {
	Name       string
	Pkg        string
	PkgPath    string
	Fields     []Field
	fieldIdx   map[string]int
	Methods    map[string]*FuncSig
	Struct     bool
	TypeParams []string
	// Constraints 是泛型形参的约束：实例化点校验，报错落在使用点。
	Constraints []parse.TypeParamConstraint
	// Derived 是 derive ... 的派生能力集（键 = ToString/Compare/Hash）。
	// 打印无 ToString 的 class = 编译错；Compare/Hash 生成同名方法。
	Derived map[string]bool
}

// HasDerive 报告类是否带某个 derive 能力（nil 安全）。
func (cl *Class) HasDerive(what string) bool {
	if cl == nil || cl.Derived == nil {
		return false
	}
	return cl.Derived[what]
}

type Field struct {
	Name string
	Type Type
	Line int
}

// Interface holds signature-only methods (结构化满足).
type Interface struct {
	Name    string
	Pkg     string
	PkgPath string
	Methods map[string]*FuncSig
	// Slots 是方法名的声明序（接口分发槽位号 = 接口声明里方法的声明序）。
	// 见证表按这个序排函数指针，调用点按同一序取槽位。
	Slots []string
}

// slotOrder 返回接口方法的声明序（无声明序记录时退化为名字排序，保确定性）。
func (ifc *Interface) slotOrder() []string {
	if ifc == nil {
		return nil
	}
	if len(ifc.Slots) > 0 {
		return ifc.Slots
	}
	names := make([]string, 0, len(ifc.Methods))
	for n := range ifc.Methods {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SlotOf 返回方法在接口里的槽位号（-1 = 无此方法）。
func (ifc *Interface) SlotOf(name string) int {
	for i, n := range ifc.slotOrder() {
		if n == name {
			return i
		}
	}
	return -1
}

// Enum: 封闭变体集；带数据变体的 Payload 非 nil。
type Enum struct {
	Name       string
	Pkg        string
	PkgPath    string
	Variants   []Variant
	variantI   map[string]int
	HasData    bool
	TypeParams []string
	// ErrCode = true 表示 `enum X: ErrCode { … }`：错误码约束枚举。
	// 码由编译器按（包身份哈希, 变体序号）分配，构建图内冲突 = 编译错误（C26）。
	ErrCode bool
	// Codes 是 ErrCode 枚举每个变体的错误码（下标 = 变体序号）；其余枚举为 nil。
	// 分配见 errcode.go —— 那是码的唯一实现，emit 与 is() 只读它。
	Codes []int32
	// DeclPos 是声明位置（诊断用：负载解析推迟到类型收齐后，报错仍要指回声明处）。
	DeclPos parse.Pos
}

type Variant struct {
	Name    string
	Payload Type
}

// Slice T[] / MapT map[K]V / SetT set[T] / ArrayT [T;N] — 容器全部引用对象,
// ArrayT 例外（内联值）。
type Slice struct{ Elem Type }
type MapT struct {
	Key   Type
	Value Type
}
type SetT struct{ Elem Type }
type ArrayT struct {
	Elem Type
	N    int64
}

// FuncT is the lambda type (P1, P2) -> R.
//
// 闭包：Captures 非空 = 该函数**值**是闭包（环境结构体里按值保存了捕获项）；
// 类型同一性**不看** Captures（捕获与否是值的事实，不是类型的事实），
// 但区域安全判定要看 —— containsReference 因此对闭包返回 true。
// 多返回：Results 非空 = 多个返回位（此时 Result 为 nil）；Result 是单返回的简写。
type FuncT struct {
	Params   []Type
	Result   Type // nil = 无返回
	Results  []Type
	Captures []Type
}

// HasRefCapture 报告闭包是否按值持有了引用（决定它能否存进更浅的区域）。
func (t *FuncT) HasRefCapture() bool {
	if t == nil {
		return false
	}
	for _, c := range t.Captures {
		if isReference(c) || containsReference(c) {
			return true
		}
	}
	return false
}

// TypeParam is a generic parameter during body checking (圆 1 单态化).
type TypeParam struct{ Name string }

func (*Basic) typeNode()     {}
func (*Class) typeNode()     {}
func (*Interface) typeNode() {}
func (*Enum) typeNode()      {}
func (*Slice) typeNode()     {}
func (*MapT) typeNode()      {}
func (*SetT) typeNode()      {}
func (*ArrayT) typeNode()    {}
func (*FuncT) typeNode()     {}
func (*TypeParam) typeNode() {}

// --- 构造辅助 ---------------------------------------------------------------

var (
	TI8    = &Basic{"i8"}
	TI16   = &Basic{"i16"}
	TI32   = &Basic{"i32"}
	TI64   = &Basic{"i64"}
	TU8    = &Basic{"u8"}
	TU16   = &Basic{"u16"}
	TU32   = &Basic{"u32"}
	TU64   = &Basic{"u64"}
	TUsize = &Basic{"usize"}
	TF32   = &Basic{"f32"}
	TF64   = &Basic{"f64"}
	TBool  = &Basic{"bool"}
	TStr   = &Basic{"str"}
	TRune  = &Basic{"rune"}
	TErr   = &Basic{"Err"}
)

var basicByName = map[string]*Basic{
	"i8": TI8, "i16": TI16, "i32": TI32, "i64": TI64,
	"u8": TU8, "u16": TU16, "u32": TU32, "u64": TU64, "usize": TUsize,
	"f32": TF32, "f64": TF64, "bool": TBool, "str": TStr, "rune": TRune, "Err": TErr,
}

func basicType(name string) (*Basic, bool) {
	b, ok := basicByName[name]
	return b, ok
}

// reservedTypeNames 是 C2 的保留类型名表（《AIC 语言规范》§12）。
//
// 基本类型直接取 basicByName（**同一张表**，禁第二份名单）；根类型是规范 §1
// 那批无需 import 即可用的名字 —— 它们一旦被用户声明成同名类型，
// `var x Option[i32]` 就同时指向两个东西，而"歧义即错误"。
var reservedTypeNames = func() map[string]bool {
	m := map[string]bool{}
	for name := range basicByName {
		m[name] = true
	}
	for _, name := range []string{
		"Option", "Result", "Value", "Duration", "Code", "Iterator", "LogLevel", "ctx",
		"map", "set", "chan", // 容器形参：类型位置的保留词
	} {
		m[name] = true
	}
	return m
}()

func (t *Basic) String() string     { return t.Name }
func (t *Class) String() string     { return qualName(t.Pkg, t.Name) }
func (t *Interface) String() string { return qualName(t.Pkg, t.Name) }
func (t *Enum) String() string      { return qualName(t.Pkg, t.Name) }
func (t *Slice) String() string     { return t.Elem.String() + "[]" }
func (t *MapT) String() string      { return "map[" + t.Key.String() + "]" + t.Value.String() }
func (t *SetT) String() string      { return "set[" + t.Elem.String() + "]" }
func (t *ArrayT) String() string    { return "[" + t.Elem.String() + ";" + itoa(int(t.N)) + "]" }
func (t *TypeParam) String() string { return t.Name }

func (t *FuncT) String() string {
	var parts []string
	for _, p := range t.Params {
		parts = append(parts, p.String())
	}
	s := "(" + strings.Join(parts, ", ") + ")"
	if len(t.Results) > 0 {
		var rs []string
		for _, r := range t.Results {
			rs = append(rs, r.String())
		}
		s += " -> (" + strings.Join(rs, ", ") + ")"
	} else if t.Result != nil {
		s += " -> " + t.Result.String()
	}
	return s
}

func qualName(pkg, name string) string {
	if pkg == "" {
		return name
	}
	return pkg + "." + name
}

// --- 分类谓词 ---------------------------------------------------------------

func isNumeric(t Type) bool {
	b, ok := t.(*Basic)
	if !ok {
		return false
	}
	switch b.Name {
	case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize", "f32", "f64", "rune":
		return true
	}
	return false
}

func isInt(t Type) bool {
	b, ok := t.(*Basic)
	if !ok {
		return false
	}
	switch b.Name {
	case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize", "rune":
		return true
	}
	return false
}

func isFloat(t Type) bool {
	b, ok := t.(*Basic)
	return ok && (b.Name == "f32" || b.Name == "f64")
}

func isStr(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.Name == "str"
}

func isErr(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.Name == "Err"
}

// ErrFieldIndex 是 `Err` 字段的**冻结下标**（§16 ④：`{code i32, msg str, cause}`）。
// **唯一实现**：AIR 降级的 `field` 下标与 L5 的 C 发端都走这里 —— AIR 侧对 Err
// 一律给下标 0 会让 `e.msg` 被降成 `field e, 0`（读的是 code），IR 路径**静默读错字段**。
func ErrFieldIndex(name string) (int, bool) {
	switch name {
	case "code":
		return 0, true
	case "msg":
		return 1, true
	case "cause":
		return 2, true
	}
	return 0, false
}

// isReference reports whether t's values are region-managed references
// (唯一引用形态 = class/interface 值与容器句柄).
func isReference(t Type) bool {
	switch v := t.(type) {
	case *Class:
		return !v.Struct
	case *Interface:
		return true
	case *Slice, *MapT, *SetT:
		return true
	}
	return false
}

// isValueType: 按值传递且无区域头的类型。
func isValueType(t Type) bool {
	switch v := t.(type) {
	case *Basic:
		return true
	case *Class:
		return v.Struct
	case *Enum, *ArrayT, *FuncT:
		return true
	}
	return false
}

// containsReference reports whether a value type embeds references that need
// per-reference guards when copied into shallower storage (⑥):
// 带数据 enum、[T;N] of class、**捕获引用的闭包**。
func containsReference(t Type) bool {
	switch v := t.(type) {
	case *Enum:
		if !v.HasData {
			return false
		}
		for _, va := range v.Variants {
			if va.Payload != nil && containsReference(va.Payload) {
				return true
			}
		}
		return false
	case *ArrayT:
		return containsReference(v.Elem)
	case *FuncT:
		return v.HasRefCapture()
	case *Class:
		if !v.Struct {
			return true
		}
		for _, f := range v.Fields {
			if containsReference(f.Type) {
				return true
			}
		}
		return false
	}
	return false
}

// captureSafe 报告类型能否被 `func` 字面量按值捕获（《AIC 语言规范》§5.3
// 捕获规则 + C11 扩展）。
//
// 判据 = §5.3 白名单的直译：**标量（含 str/rune）/ 无载荷 enum / [T;N] /
// 全字段值类型的 struct**。背后的两条理由（§5.3 判据 1、2）：
//  1. 值类型 ⇒ 闭包环境是纯值结构体，**无区域头**；
//  2. 不含引用 ⇒ 随闭包值拷贝后没有任何指针可能悬垂。
//
// 合起来才是规范说的"可证无悬垂"。class / 容器 / 接口 / chan / 带负载且含引用的
// enum 一律 false —— 捕获它们 = 编译错误，修复提示改用显式上下文参数。
func captureSafe(t Type, depth int) bool {
	if t == nil || depth > 8 {
		return false
	}
	switch v := t.(type) {
	case *Basic:
		return true // 标量 / str / rune / Err：按值、无区域头
	case *Enum:
		return !v.HasData // 无载荷 enum；带负载的不在白名单（§5.3）
	case *ArrayT:
		return captureSafe(v.Elem, depth+1) // [T;N]：元素必须也是白名单类型
	case *Class:
		if !v.Struct {
			return false // 引用 class：环境会含区域头
		}
		for i := range v.Fields {
			if !captureSafe(v.Fields[i].Type, depth+1) {
				return false // "全字段值类型的 struct"
			}
		}
		return true
	case *Instance:
		// 泛型实例（Option[i64] / Box[Node] …）：按实参代换基座后再判，
		// 否则 TypeParam 落不进任何分支，会把纯值实例一并拒掉。
		return captureInstanceSafe(v, depth)
	}
	return false
}

// captureInstanceSafe 是 captureSafe 对泛型实例的展开：把基座的字段/负载按实参
// 代换（Subst，单一实现）后逐个过 captureSafe。
func captureInstanceSafe(v *Instance, depth int) bool {
	if v == nil || len(v.Args) == 0 {
		return captureSafe(v.Base, depth+1)
	}
	switch b := v.Base.(type) {
	case *Class:
		if !b.Struct {
			return false
		}
		for i := range b.Fields {
			if !captureSafe(Subst(b.Fields[i].Type, b.TypeParams, v.Args), depth+1) {
				return false
			}
		}
		return true
	case *Enum:
		for i := range b.Variants {
			p := Subst(b.Variants[i].Payload, b.TypeParams, v.Args)
			if p != nil && !captureSafe(p, depth+1) {
				return false
			}
		}
		return true
	}
	return captureSafe(v.Base, depth+1)
}

// --- 同一性与赋值 -----------------------------------------------------------

// identical: 无隐式转换的世界里，类型相等只有一种（泛型实例与 TypeParam 同名即同）。
func identical(a, b Type) bool {
	switch av := a.(type) {
	case *Basic:
		bv, ok := b.(*Basic)
		return ok && av.Name == bv.Name
	case *Class:
		bv, ok := b.(*Class)
		return ok && av.Name == bv.Name && av.Pkg == bv.Pkg
	case *Interface:
		bv, ok := b.(*Interface)
		return ok && av.Name == bv.Name && av.Pkg == bv.Pkg
	case *Enum:
		bv, ok := b.(*Enum)
		return ok && av.Name == bv.Name && av.Pkg == bv.Pkg
	case *ChanT:
		bv, ok := b.(*ChanT)
		return ok && identical(av.Elem, bv.Elem)
	case *Slice:
		bv, ok := b.(*Slice)
		return ok && identical(av.Elem, bv.Elem)
	case *MapT:
		bv, ok := b.(*MapT)
		return ok && identical(av.Key, bv.Key) && identical(av.Value, bv.Value)
	case *SetT:
		bv, ok := b.(*SetT)
		return ok && identical(av.Elem, bv.Elem)
	case *ArrayT:
		bv, ok := b.(*ArrayT)
		return ok && av.N == bv.N && identical(av.Elem, bv.Elem)
	case *FuncT:
		bv, ok := b.(*FuncT)
		if !ok || len(av.Params) != len(bv.Params) {
			return false
		}
		for i := range av.Params {
			if !identical(av.Params[i], bv.Params[i]) {
				return false
			}
		}
		// 多返回位逐个比；单返回位退化到 Result。
		if len(av.Results) != len(bv.Results) {
			return false
		}
		for i := range av.Results {
			if !identical(av.Results[i], bv.Results[i]) {
				return false
			}
		}
		if (av.Result == nil) != (bv.Result == nil) {
			return false
		}
		return av.Result == nil || identical(av.Result, bv.Result)
	case *TypeParam:
		bv, ok := b.(*TypeParam)
		return ok && av.Name == bv.Name
	case *Instance:
		bv, ok := b.(*Instance)
		if !ok || len(av.Args) != len(bv.Args) {
			return false
		}
		if !identical(av.Base, bv.Base) {
			return false
		}
		for i := range av.Args {
			if !identical(av.Args[i], bv.Args[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// intRange: 整型可容纳区间 [min, max]。
func intRange(t Type) (lo, hi int64, ok bool) {
	b, isBasic := t.(*Basic)
	if !isBasic {
		return 0, 0, false
	}
	switch b.Name {
	case "i8":
		return -128, 127, true
	case "i16":
		return -32768, 32767, true
	case "i32":
		return -2147483648, 2147483647, true
	case "i64":
		return -9223372036854775808, 9223372036854775807, true
	case "u8":
		return 0, 255, true
	case "u16":
		return 0, 65535, true
	case "u32":
		return 0, 4294967295, true
	case "u64", "usize":
		return 0, 9223372036854775807, true // 上半区间由 uint64 位宽另行判定
	case "rune":
		// Unicode 标量值：0..=0x10FFFF，代理区 [0xD800, 0xDFFF] 非法（§2.1）。
		// 范围上界由构造处单独判定代理区（intRange 只给连续区间）。
		return 0, 0x10FFFF, true
	}
	return 0, 0, false
}

// uintMax: 无符号位宽（usize/u64 = 2^64-1 用 uint64 承载）。
func uintMax(t Type) (uint64, bool) {
	b, isBasic := t.(*Basic)
	if !isBasic {
		return 0, false
	}
	switch b.Name {
	case "u8":
		return 255, true
	case "u16":
		return 65535, true
	case "u32":
		return 4294967295, true
	case "u64", "usize":
		return 18446744073709551615, true
	}
	return 0, false
}
