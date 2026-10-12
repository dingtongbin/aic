package emit

import (
	"fmt"
	"sort"

	"aic/internal/air"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 插值助手：值 → str（规范 §1.4 的可插值集合里那两个"非标量"成员）
//
//	str_enumName      无载荷 enum 的**值** → 变体名（`Level.High` → "High"）
//	str_ofStruct      derive ToString 的 struct/class → `Name{f: v, …}`
//
// 为什么是生成的 C 函数而不是内联表达式：两者都要按**类型**展开（变体表 / 字段表），
// 而调用点上只有类型文本。内联会把整张表复制进每一处插值点 —— 20 个变体的枚举
// 插三次就是三份 switch。函数只发一次，调用点只留一个符号。
//
// 发哪些：只发**被引用过**的（irReprEnums / irReprStructs 在函数体段按发现填充，
// 第一遍收集、第二遍发）。全量发 = 一堆没人调的 static 函数 —— 覆盖率报告会直接
// 盯上，而阶段 3 的死代码清零不是嘴上说的。
//
// 位置：原型在函数体段之前（调用点在体内，C 要求先声明），定义在函数体段之后
// （体内要读被打印类型的字段，与 structcmp / spawn thunk 同一机制）。
// ---------------------------------------------------------------------------

// emitReprProtos 发插值助手的原型（原型段：函数体之前）。
func (c *Ctx) emitReprProtos() {
	for _, ty := range sortedKeys(c.irReprEnums) {
		c.line("static aic_str %s(aic_i32 tag);", c.reprEnumName(ty))
	}
	for _, ty := range sortedKeys(c.irReprStructs) {
		ct, err := c.irDeclText(ty, "v")
		if err != nil {
			continue
		}
		c.line("static aic_str %s(%s, aic_u32 line);", c.reprStructName(ty), ct)
	}
	if len(c.irReprEnums)+len(c.irReprStructs) > 0 {
		c.line("")
	}
}

// emitReprBodies 发插值助手的定义（函数体段之后）。
func (c *Ctx) emitReprBodies() error {
	for _, ty := range sortedKeys(c.irReprEnums) {
		if err := c.emitEnumReprBody(ty); err != nil {
			return err
		}
	}
	for _, ty := range sortedKeys(c.irReprStructs) {
		if err := c.emitStructReprBody(ty); err != nil {
			return err
		}
	}
	return nil
}

// emitEnumReprBody 发「tag → 变体名」：`Level.High` 插出来就是 "High"。
// tag 序号 = 变体声明序（AIR 的 Variant.Tag 与 C 枚举常量同一张表）。
func (c *Ctx) emitEnumReprBody(ty string) error {
	t, ok := c.irTab[ty]
	if !ok {
		return fmt.Errorf("emit(IR): no C mapping for the interpolated enum type %q", ty)
	}
	en, isEn := types.IsEnum(t)
	if !isEn {
		return fmt.Errorf("emit(IR): %q is not an enum type declaration", ty)
	}
	c.line("static aic_str %s(aic_i32 tag) {", c.reprEnumName(ty))
	c.line("    switch (tag) {")
	for i, v := range en.Variants {
		c.line("    case %d: return %s;", i, c.strLit(v.Name))
	}
	c.line("    }")
	// 不可达：tag 只由本类型的变体构造产生。留一条显式返回而不是 default 空串 ——
	// 空串会让"打不出名字"与"名字就是空"无法区分。
	c.line("    return %s;", c.strLit(""))
	c.line("}")
	c.line("")
	return nil
}

// emitStructReprBody 发一个 struct 的 ToString：`Name{f1: v1, f2: v2}`。
// 字段值按**字段类型**机械选格式化器（与插值逐分量格式化同一条规则，禁第二份），
// 故嵌套 payload-free enum / derive ToString struct 字段自己也能打。
func (c *Ctx) emitStructReprBody(ty string) error {
	t, ok := c.irTab[ty]
	if !ok {
		return fmt.Errorf("emit(IR): no C mapping for the interpolated struct type %q", ty)
	}
	cl, isCl := types.IsClass(t)
	if !isCl {
		return fmt.Errorf("emit(IR): %q is not a struct type declaration", ty)
	}
	ct, err := c.irDeclText(ty, "v")
	if err != nil {
		return err
	}
	c.line("static aic_str %s(%s, aic_u32 line) {", c.reprStructName(ty), ct)
	c.line("    aic_str out = %s;", c.strLit(cl.Name+"{"))
	for i := range cl.Fields {
		f := cl.Fields[i]
		expr, err := c.reprFieldText(f.Type, "v."+f.Name)
		if err != nil {
			return err
		}
		if i > 0 {
			c.line("    out = aic_str_concat(out, %s, line);", c.strLit(", "))
		}
		c.line("    out = aic_str_concat(aic_str_concat(out, %s, line), %s, line);",
			c.strLit(f.Name+": "), expr)
	}
	c.line("    out = aic_str_concat(out, %s, line);", c.strLit("}"))
	c.line("    return out;")
	c.line("}")
	c.line("")
	return nil
}

// reprFieldText 给一个字段值选 ToString 用的格式化表达式（字段类型 → 格式化器）。
// 规则与插值逐分量格式化同源：标量走 str_fromXxx，嵌套 enum/struct 走插值助手。
// line 是生成函数的行号形参（str_fromXxx 的分配要它做陷阱定位）。
func (c *Ctx) reprFieldText(fty types.Type, valExpr string) (string, error) {
	switch n := basicNameOf(fty); n {
	case "str":
		return valExpr, nil
	case "bool":
		return fmt.Sprintf("aic_std_str_from_bool(%s)", valExpr), nil
	case "f64":
		return fmt.Sprintf("aic_std_str_from_f64(%s, line)", valExpr), nil
	case "f32":
		return fmt.Sprintf("aic_std_str_from_f64((aic_f64)(%s), line)", valExpr), nil
	case "i8", "i16", "i32", "i64":
		return fmt.Sprintf("aic_std_str_from_i64((aic_i64)(%s), line)", valExpr), nil
	case "u8", "u16", "u32", "u64", "usize":
		return fmt.Sprintf("aic_std_str_from_u64((aic_u64)(%s), line)", valExpr), nil
	}
	if en, isEn := types.IsEnum(fty); isEn && !en.HasData {
		return fmt.Sprintf("%s((aic_i32)(%s))", c.reprEnumName(air.TyText(en)), valExpr), nil
	}
	if cl, isCl := types.IsClass(fty); isCl && cl.Derived["ToString"] {
		return fmt.Sprintf("%s(%s, line)", c.reprStructName(air.TyText(cl)), valExpr), nil
	}
	return "", fmt.Errorf("emit(IR): the ToString of a struct does not cover a %s field "+
		"(scalars, payload-free enums and derive ToString structs only)", fty.String())
}

// basicNameOf 取一个基本类型的名字（非基本类型 = 空串）。
func basicNameOf(t types.Type) string {
	if b, ok := t.(*types.Basic); ok {
		return b.Name
	}
	return ""
}

// reprEnumName / reprStructName 是插值助手的 C 名（与 mangling 同源：类型文本进名字）。
func (c *Ctx) reprEnumName(ty string) string   { return "aic_repr_enum_" + ty }
func (c *Ctx) reprStructName(ty string) string { return "aic_repr_" + ty }

// needReprEnum / needReprStruct 登记一个插值助手（两遍机制：第一遍收集、第二遍发）。
func (c *Ctx) needReprEnum(ty string) {
	if c.irReprEnums == nil {
		c.irReprEnums = map[string]bool{}
	}
	c.irReprEnums[ty] = true
}

func (c *Ctx) needReprStruct(ty string) {
	if c.irReprStructs == nil {
		c.irReprStructs = map[string]bool{}
	}
	c.irReprStructs[ty] = true
}

// sortedKeys 取一个字符串集合的字典序键表（确定性；map 序随机会破坏 H2）。
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
