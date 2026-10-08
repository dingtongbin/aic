package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 类型 → C 名 (核心设计 §十: 只有一处实现; 红线 10 的同款纪律)。
//
// 对齐 runtime/aic_l0.h 已定案的 ABI:
//   基本类型 = aic_i32 … aic_str;  Err = aic_Err (按值两字);
//   普通 class = 指针 (引用对象, 带头);  @packed class = 值 (C struct);
//   T[] = aic_list_<SUF> *;  map[K]V = aic_map_<KS>_<VS> *;
//   set[T] = aic_set_<SUF> *;  [T;N] = 内联数组 (值);
//   interface = aic_iface (两机器字);  enum = 无数据 C enum / 带数据 {tag, union};
//   lambda 类型 = 函数指针。
// ---------------------------------------------------------------------------

// BasicCName 把基本类型名映射为运行时的 C 名。
var BasicCName = map[string]string{
	"i8": "aic_i8", "i16": "aic_i16", "i32": "aic_i32", "i64": "aic_i64",
	"u8": "aic_u8", "u16": "aic_u16", "u32": "aic_u32", "u64": "aic_u64",
	"usize": "aic_usize",
	"f32":   "aic_f32", "f64": "aic_f64",
	"bool": "bool",
	"str":  "aic_str",
	"Err":  "aic_Err",
}

// cTypeName 返回一个语义类型的 C 类型名。
// 用户的具名类型统一带包前缀 (aic_<pkg>_<Name>), 与 mangling 同源。
func (c *Ctx) cTypeName(t types.Type) string {
	if t == nil {
		return "void"
	}
	// 泛型实例化上下文：T → 实参（实例化后的 C 里不允许残留 TypeParam）。
	t = c.substT(t)
	switch {
	case types.IsStrType(t):
		return "aic_str"
	case types.IsErrType(t):
		return "aic_Err"
	}
	if inst, ok := t.(*types.Instance); ok {
		// 泛型实例：typedef 名 = 实例键（§十 10.2 键与名同源）。
		// 引用类的实例仍是指针（Box[i32] *）；@packed 实例是值（C struct）。
		name := c.registerInstance(inst)
		if cl, isCl := types.IsClass(inst.Base); isCl && !cl.Packed {
			return name + " *"
		}
		return name
	}
	if cl, ok := types.IsClass(t); ok {
		// 普通 class = 引用对象 (指针); @packed = 值 (C struct)
		if cl.Packed {
			return c.namedTypeName(cl.Pkg, cl.Name)
		}
		return c.namedTypeName(cl.Pkg, cl.Name) + " *"
	}
	if en, ok := types.IsEnum(t); ok {
		return c.namedTypeName(types.EnumPkg(en), en.Name)
	}
	if ifc, ok := types.IsInterface(t); ok {
		_ = ifc
		return "aic_iface"
	}
	if _, ok := t.(*types.MutexT); ok {
		return "aic_mutex" // 值类型（零值 = 未加锁，§七）
	}
	if ch, ok := t.(*types.ChanT); ok {
		suf, has := c.containerSuffix(ch.Elem)
		if !has {
			return c.unsupported(t)
		}
		return "aic_chan_" + suf + " *"
	}
	if types.IsBytes(t) {
		// N7：bytes 与 u8[] 共用运行时实例（独立类型身份 + 独立方法集）。
		return "aic_list_u8 *"
	}
	if types.IsSlice(t) {
		suf, ok := c.containerSuffix(types.SliceElem(t))
		if !ok {
			return c.unsupported(t)
		}
		return "aic_list_" + suf + " *"
	}
	if types.IsArray(t) {
		elem, n, _ := types.ArrayElem(t)
		return fmt.Sprintf("%s[%d]", c.cTypeName(elem), n)
	}
	if types.IsMap(t) {
		k, v, _ := types.MapParts(t)
		ks, kok := c.mapKeySuffix(k)
		vs, vok := c.mapValueSuffix(v)
		if !kok || !vok {
			return c.unsupported(t)
		}
		return fmt.Sprintf("aic_map_%s_%s *", ks, vs)
	}
	if types.IsSet(t) {
		suf, ok := c.containerSuffix(types.SetElem(t))
		if !ok {
			return c.unsupported(t)
		}
		return "aic_set_" + suf + " *"
	}
	if params, res, ok := types.FuncParts(t); ok {
		parts := make([]string, 0, len(params)+1)
		for _, p := range params {
			parts = append(parts, c.cTypeName(p))
		}
		ret := "void"
		if res != nil {
			ret = c.cTypeName(res)
		}
		return fmt.Sprintf("%s (*)(%s)", ret, joinComma(parts))
	}
	if b, ok := t.(*types.Basic); ok {
		if name, has := BasicCName[b.Name]; has {
			return name
		}
	}
	c.warn("type %s has no C mapping (emitting void)", t.String())
	return "void"
}

// unsupported 记录一次容器形状越界 (见 containerSuffix 的说明), 并返回 void
// 让编译在 C 层失败而不是悄悄发错代码。
func (c *Ctx) unsupported(t types.Type) string {
	c.warn("container shape %s is not instantiated: the runtime L1 short-suffix table does not cover this (element/key/value) combination", t.String())
	return "void"
}

// containerSuffix: T[] / set[T] 的 short 后缀。
// 规则唯一所有者 = types.RuntimeSuffix（AIR 侧给构造调用命名也用同一张表）；
// 这里只做「未实例化 → void + 警告」的发射层包装。
func (c *Ctx) containerSuffix(elem types.Type) (string, bool) {
	return types.RuntimeSuffix(elem)
}

// mapKeySuffix: map[K]V 的键后缀（规则唯一所有者 = types.RuntimeMapKeySuffix）。
func (c *Ctx) mapKeySuffix(k types.Type) (string, bool) {
	return types.RuntimeMapKeySuffix(k)
}

// mapValueSuffix: map 的值槽位后缀（规则唯一所有者 = types.RuntimeMapValueSuffix）。
func (c *Ctx) mapValueSuffix(v types.Type) (string, bool) {
	return types.RuntimeMapValueSuffix(v)
}

// typedefName 是用户具名类型的 C 名: aic_<pkg>_<Name> (与 mangling 同一张表)。
func (c *Ctx) typedefName(pkg, name string) string {
	return mangleType(pkg, name)
}

// namedTypeName 与 typedefName 同源，但补齐缺失的包名：本包具名类型的 Pkg
// 可能未随语义类型带出（Class 的 Pkg 并非处处填），此时按「本包」解释 ——
// 缺了这一步就会发出 aic__Buf 这种空包名的符号，C 层直接报未声明类型。
func (c *Ctx) namedTypeName(pkg, name string) string {
	return c.typedefName(c.ownerPkg(pkg), name)
}

// ownerPkg 补齐符号所属包名：空 = 本包（跨包符号一律带 Pkg，见 types 侧唯一所有者）。
func (c *Ctx) ownerPkg(pkg string) string {
	if pkg == "" {
		return c.pkg()
	}
	return pkg
}

// cMethodName 是类方法的 C 符号名（所属包 = 类的包，不是调用点的包：
// 跨包调用 a.Point.sum() 时两者不同，用错就发出未定义符号）。
func (c *Ctx) cMethodName(cl *types.Class, name string) string {
	return MangleFunc(c.ownerPkg(types.ClassPkg(cl)), cl.Name, name)
}

// cTypeDecl 把一个语义类型写成「带声明子的声明」，处理 [T;N] 的 C 语法
// （数组类型名不能直接跟变量名：写 aic_i32 a[4] 而不是 aic_i32[4] a）。
func (c *Ctx) cTypeDecl(t types.Type, name string) string {
	if types.IsArray(t) {
		elem, n, _ := types.ArrayElem(t)
		return c.cTypeDecl(elem, fmt.Sprintf("%s[%d]", name, n))
	}
	if params, res, ok := types.FuncParts(t); ok {
		// 函数指针: 返回类型 (*name)(形参…)
		parts := make([]string, 0, len(params))
		for _, p := range params {
			parts = append(parts, c.cTypeName(p))
		}
		ret := "void"
		if res != nil {
			ret = c.cTypeName(res)
		}
		return fmt.Sprintf("%s (*%s)(%s)", ret, name, joinComma(parts))
	}
	return c.cTypeName(t) + " " + name
}

// zeroValue 返回某类型的 C 零值表达式 (核心设计 §二.3: 数值 0 / false / "" /
// 引用与 Err = NULL / 结构体 = {0})。
func (c *Ctx) zeroValue(t types.Type) string {
	if t == nil {
		return "0"
	}
	switch {
	case types.IsBoolType(t):
		return "false"
	case types.IsStrType(t):
		return "((aic_str){ 0, 0 })"
	case types.IsErrType(t):
		return "AIC_ERR_NONE"
	}
	// interface 是两机器字结构（不是指针）：零值必须写成结构字面量，
	// 发 NULL 会让 C 报 invalid initializer（§四 值 = {data, vt}）。
	if _, isIfc := types.IsInterface(t); isIfc {
		return "((aic_iface){ 0 })"
	}
	if types.IsReference(t) {
		return "NULL"
	}
	if types.IsValueType(t) && !types.IsNumeric(t) {
		return fmt.Sprintf("(%s){0}", c.cTypeName(t))
	}
	return "0"
}

// isNilLiteral 报告表达式是否 nil 字面量 (零值发射的分支点)。
func isNilLiteral(e parse.Expr) bool {
	_, ok := e.(*parse.NilLit)
	return ok
}

func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
