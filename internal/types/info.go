package types

import (
	"strings"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// Info —— 检查结果的只读视图，供 internal/emit 消费（核心设计 §十：emit 是
// AST → C 的纯函数；它需要的语义事实全部来自这里，不在 emit 里重做一遍）。
//
// 为什么要有这一层：类型模型与作用域事实（哪个名字是局部/参数、哪个名字是
// 用户函数、类型实参）只有在检查器内才完整可见；emit 若自带一份作用域重建，
// 就成了「两处各写一份」（红线 10 的同款病）。本层只读、不产生新语义。
// ---------------------------------------------------------------------------

// Info 是不可变的检查产物视图。
type Info struct {
	// Pkg 是包名（目录即包；CLI 单文件编译时 = main）。
	Pkg string
	// Locals 记录函数体/方法体里出现过的局部名与参数名。
	// 键 = 函数标识（自由函数 = 名字；方法 = "Class.method"）。
	Locals map[string]map[string]bool
	// Types 是每个表达式节点的检查类型（未定型字面量为 nil，见 LookupType）。
	Types map[parse.Expr]Type
	// Guards 是存储点标记（emit 的守卫发射依据）。
	Guards map[parse.Node]GuardSpec
	// Consts 是包级常量（含折叠值）。
	Consts map[string]*Symbol
	// Funcs 是包级函数签名（不含方法）。
	Funcs map[string]*FuncSig
	// Classes / Interfaces / Enums 是包级具名类型。
	Classes    map[string]*Class
	Interfaces map[string]*Interface
	Enums      map[string]*Enum
	// Imported 是 import 过的包名集合（std 面见 §十四）。
	Imported map[string]bool
	// mainHasErr 记录入口是否有 Err 位。
	MainHasErr bool
	// FuncInsts 是泛型函数实例表（单态化的工作单元；键 = 函数 + 实参类型）。
	FuncInsts []FuncInst
	// Closures 是闭包字面量的捕获表（N1：键 = LambdaExpr 节点，值 = 捕获项名，
	// 按首次出现序）。AIR 降级按它建环境结构体 —— 禁第二份自由变量分析。
	Closures map[parse.Expr][]string
	// CapRefs 记录闭包体内每一次捕获引用（标识符节点 → 环境槽号）。
	CapRefs map[parse.Expr]int
	// Comptime 是编译期内省原语的折叠结果（N4）。
	Comptime map[parse.Expr]ComptimeVal
	// ArrayLens 是 [T;N] 的 N 折出来的长度（键 = N 表达式节点）。emit 必须按**同一份**
	// 结果发 C 数组维度：自己再折一遍只认字面量与 const 名，`[i32; sizeOf(i32)]` 会发成 `[0]`。
	ArrayLens map[parse.Expr]int64
	// TypeExprTypes 是「类型写法节点 → 语义类型」（检查器的唯一解析结果）。
	TypeExprTypes map[parse.TypeExpr]Type
	// CallTypeArgs 是泛型调用点的实例实参（键 = 调用节点）。
	CallTypeArgs map[parse.Expr][]Type
}

// ComptimeVal 是一次编译期内省（N4）的折叠结果。
//   Kind = "typeName" | "sizeOf" | "isValueType"
//   Ty   = 实参类型（sizeOf 在 C 侧用 sizeof(Ty) 现算真尺寸）
//   Const= 折叠值；sizeOf 对 @packed/[T;N] 不可折（ConstNone），由 C 的 sizeof 承担
type ComptimeVal struct {
	Kind  string
	Ty    Type
	Const ConstVal
}

// ComptimeOf 返回一次内省调用的折叠结果（非内省 = false）。
func (i *Info) ComptimeOf(e parse.Expr) (ComptimeVal, bool) {
	if i == nil || e == nil {
		return ComptimeVal{}, false
	}
	v, ok := i.Comptime[e]
	return v, ok
}

// CapRef 返回一次标识符引用是否落在当前闭包的环境槽上。
func (i *Info) CapRef(e parse.Expr) (int, bool) {
	if i == nil || e == nil {
		return 0, false
	}
	slot, ok := i.CapRefs[e]
	return slot, ok
}

// Captures 返回闭包字面量的捕获项名（无捕获或非闭包 = nil）。
func (i *Info) Captures(e parse.Expr) []string {
	if i == nil || e == nil {
		return nil
	}
	return i.Closures[e]
}

// LookupType 返回表达式节点的检查类型；未定型字面量返回 nil（调用方用上下文
// 期望类型定夺——字面量的取值本来由上下文决定，§一 最窄可容纳）。
func (i *Info) LookupType(e parse.Expr) Type {
	if i == nil || e == nil {
		return nil
	}
	return i.Types[e]
}

// HasGuard 报告该节点是否被标了存储点守卫。
func (i *Info) HasGuard(n parse.Node) (GuardSpec, bool) {
	if i == nil || n == nil {
		return GuardSpec{}, false
	}
	g, ok := i.Guards[n]
	return g, ok
}

// IsLocal 报告 name 在函数 fnKey 内是局部名或参数名（→ 直接发射裸标识符）。
func (i *Info) IsLocal(fnKey, name string) bool {
	if i == nil {
		return false
	}
	m, ok := i.Locals[fnKey]
	if !ok {
		return false
	}
	return m[name]
}

// FuncKey 是 Locals 的键：自由函数 = 名字；方法 = "Class.method"。
func FuncKey(recv, name string) string {
	if recv == "" {
		return name
	}
	return recv + "." + name
}

// HasErrResult 报告末位是否为 Err（§六：Err 只允许出现在返回类型最末位）。
// emit 用它决定是否需要在每个出口构造 Err 值、以及 check 的传播形态。
func (sig *FuncSig) HasErrResult() bool {
	if sig == nil || len(sig.Results) == 0 {
		return false
	}
	return isErr(sig.Results[len(sig.Results)-1])
}

// Results 暴露返回位（只读副本由调用方自行约束）。
func (sig *FuncSig) ResultsList() []Type {
	if sig == nil {
		return nil
	}
	return sig.Results
}

// --- 类型模型只读访问（emit 的类型→C 映射需要，避免 emit 自带一份分类逻辑） ---

// IsReference 报告引用型（区域托管，零值 = NULL，带对象头）。
func IsReference(t Type) bool { return isReference(t) }

// IsValueType 报告按值传递且无区域头的类型。
func IsValueType(t Type) bool { return isValueType(t) }

// IsErrType 报告内建 Err。
func IsErrType(t Type) bool { return isErr(t) }

// IsStrType 报告内建 str。
func IsStrType(t Type) bool { return isStr(t) }

// IsBoolType 报告 bool。
func IsBoolType(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.Name == "bool"
}

// IsInteger 报告整型（不含 f32/f64）。
func IsInteger(t Type) bool { return isInt(t) }

// IsNumeric 报告数值类型。
func IsNumeric(t Type) bool { return isNumeric(t) }

// ContainsReference 报告值类型内部是否含引用（⑥ 逐内部引用守卫的依据）。
func ContainsReference(t Type) bool { return containsReference(t) }

// IsSlice / IsMap / IsSet / IsArray 报告容器种类。
func IsSlice(t Type) bool { _, ok := t.(*Slice); return ok }
func IsMap(t Type) bool   { _, ok := t.(*MapT); return ok }
func IsSet(t Type) bool   { _, ok := t.(*SetT); return ok }
func IsArray(t Type) bool { _, ok := t.(*ArrayT); return ok }

// SliceElem 返回 T[] 的元素类型（非切片返回 nil）。
func SliceElem(t Type) Type {
	if s, ok := t.(*Slice); ok {
		return s.Elem
	}
	return nil
}

// ArrayElem 返回 [T;N] 的元素与长度。
func ArrayElem(t Type) (Type, int64, bool) {
	if a, ok := t.(*ArrayT); ok {
		return a.Elem, a.N, true
	}
	return nil, 0, false
}

// MapParts 返回 map[K]V 的键值类型。
func MapParts(t Type) (Type, Type, bool) {
	if m, ok := t.(*MapT); ok {
		return m.Key, m.Value, true
	}
	return nil, nil, false
}

// SetElem 返回 set[T] 的元素类型。
func SetElem(t Type) Type {
	if s, ok := t.(*SetT); ok {
		return s.Elem
	}
	return nil
}

// FuncParts 返回 lambda 类型 (P…) -> R。
func FuncParts(t Type) ([]Type, Type, bool) {
	if f, ok := t.(*FuncT); ok {
		return f.Params, f.Result, true
	}
	return nil, nil, false
}

// MultiElems 返回多返回值形状（非多返回返回 nil）。
func MultiElems(t Type) []Type {
	if m, ok := t.(*MultiType); ok {
		return m.Elems
	}
	return nil
}

// InstanceParts 返回泛型实例（Option[i32]）的基类型与实参。
func InstanceParts(t Type) (Type, []Type, bool) {
	if in, ok := t.(*Instance); ok {
		return in.Base, in.Args, true
	}
	return nil, nil, false
}

// IsPacked 报告类是否 @packed（值语义 = C struct）。
func IsPacked(t Type) bool {
	if cl, ok := t.(*Class); ok {
		return cl.Packed
	}
	return false
}

// IsClass / IsEnum / IsInterface: 具名类型判定（nil 安全）。
func IsClass(t Type) (*Class, bool) {
	cl, ok := t.(*Class)
	return cl, ok
}

func IsEnum(t Type) (*Enum, bool) {
	en, ok := t.(*Enum)
	return en, ok
}

func IsInterface(t Type) (*Interface, bool) {
	ifc, ok := t.(*Interface)
	return ifc, ok
}

// ClassPkg 返回类的包名（mangling 用）。
func ClassPkg(cl *Class) string {
	if cl == nil {
		return ""
	}
	return cl.Pkg
}

// EnumPkg 返回枚举的包名（mangling 用）。
func EnumPkg(en *Enum) string {
	if en == nil {
		return ""
	}
	return en.Pkg
}

// --- 成员索引只读访问（fieldIdx/variantI 不导出，emit 需要声明序索引） ---

// FieldIndex 返回字段在声明序中的下标。
func (cl *Class) FieldIndex(name string) (int, bool) {
	if cl == nil {
		return 0, false
	}
	i, ok := cl.fieldIdx[name]
	return i, ok
}

// VariantIndex 返回变体下标（tag 值）。
func (en *Enum) VariantIndex(name string) (int, bool) {
	if en == nil {
		return 0, false
	}
	i, ok := en.variantI[name]
	return i, ok
}

// Method 返回类/接口的方法签名。
func (cl *Class) Method(name string) (*FuncSig, bool) {
	if cl == nil {
		return nil, false
	}
	m, ok := cl.Methods[name]
	return m, ok
}

func (ifc *Interface) Method(name string) (*FuncSig, bool) {
	if ifc == nil {
		return nil, false
	}
	m, ok := ifc.Methods[name]
	return m, ok
}

// VariantPayload 返回带数据变体的负载类型（无数据返回 nil）。
func (en *Enum) VariantPayload(name string) (Type, bool) {
	if en == nil {
		return nil, false
	}
	i, ok := en.variantI[name]
	if !ok {
		return nil, false
	}
	return en.Variants[i].Payload, true
}

// --- 检查器侧：装配 Info -------------------------------------------------------

// NoteLocal 在声明点登记局部名（由 declareVar 与参数装配处调用）。
// 依据是检查器自己的作用域（唯一事实来源），不是 emit 的二次推导。
func (c *Checker) NoteLocal(key, name string) {
	if key == "" || name == "" || name == "_" {
		return
	}
	m := c.locals[key]
	if m == nil {
		m = map[string]bool{}
		c.locals[key] = m
	}
	m[name] = true
}

func (c *Checker) buildInfo() *Info {
	info := &Info{
		// 包名 = 源文件所在目录名（目录即包，§四）。唯一所有者在这里，
		// emit 只读它：两处各算一次包名 = 红线 10 的同款病（mangling 会分叉）。
		Pkg:        c.pkg,
		Locals:     c.locals,
		Types:      c.types,
		Guards:     c.guards,
		Consts:     c.consts,
		Funcs:      c.funcs,
		Classes:    c.classes,
		Interfaces: c.ifaces,
		Enums:      c.enums,
		Imported:   c.imports,
		FuncInsts:  c.funcInsts,
		Closures:   c.closures,
		CapRefs:    c.capRefs,
		Comptime:   c.comptime,
		ArrayLens:  c.arrayLens,
		TypeExprTypes: c.typeExprTypes,
		CallTypeArgs:  c.callTypeArgs,
	}
	if sig, ok := c.funcs["main"]; ok && len(sig.Results) > 0 {
		info.MainHasErr = isErr(sig.Results[len(sig.Results)-1])
	}
	return info
}

// pkgNameFromPath 取目录名作包名（目录即包，核心设计 §四 可见性）。
func pkgNameFromPath(path string) string {
	p := strings.ReplaceAll(path, "\\", "/")
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "main"
	}
	dir := p[:i]
	if j := strings.LastIndex(dir, "/"); j >= 0 {
		dir = dir[j+1:]
	}
	if dir == "" {
		return "main"
	}
	return dir
}
