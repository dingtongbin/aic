package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 按需合成的打印函数（核心设计 §十四：println(v) 任意单值）。
//
// 运行时表（aic_l1_print.h）覆盖标量与"container of scalar elements"；以下三类形状的名字在
// 编译期不可预知，必须由 emit 合成一个 static 函数：
//   - enum（无数据：变体名；带数据：Variant(负载)）
//   - class（须 @derive(ToString)，字段按声明序打印；§十四 无 ToString = 编译错）
//   - 元素非标量的容器（嵌套容器 / enum / class 元素的 list/set/map）与 [T;N]
//
// 合成是**确定性**的：按登记序编号（aic_pr_syn_<n>），登记序由类型遍历序决定，
// 与地址/随机无关（H2）。递归形状靠"emit every prototype first, then the definitions"解决。
// ---------------------------------------------------------------------------

// printerEntry 是一个已登记的合成打印函数。
type printerEntry struct {
	name string
	ty   types.Type
	// iface 非 nil 时该打印函数是接口值的（转发到见证表的打印槽）。
	iface *types.Interface
}

// printerName 返回某类型在 C 侧的打印函数名（唯一实现：禁在别处拼打印函数名）。
// 返回的第二个值报告该名字是否来自运行时表（false = 需要 emit 合成）。
func (c *Ctx) printerName(t types.Type) (string, bool) {
	if t == nil {
		return "aic_pr_nil", true
	}
	switch {
	case types.IsStrType(t):
		return "aic_pr_str", true
	case types.IsErrType(t):
		return "aic_pr_err", true
	}
	if b, ok := t.(*types.Basic); ok {
		if _, has := printSuffixes[b.Name]; has {
			return "aic_pr_" + b.Name, true
		}
		return "", false
	}
	if suf, ok := c.scalarPrintSuffix(types.SliceElem(t)); ok && types.IsSlice(t) {
		return "aic_pr_list_" + suf, true
	}
	if suf, ok := c.scalarPrintSuffix(types.SetElem(t)); ok && types.IsSet(t) {
		return "aic_pr_set_" + suf, true
	}
	if types.IsMap(t) {
		k, v, _ := types.MapParts(t)
		ks, kok := c.scalarPrintSuffix(k)
		vs, vok := c.scalarPrintSuffix(v)
		if kok && vok {
			return fmt.Sprintf("aic_pr_map_%s_%s", ks, vs), true
		}
	}
	return "", false
}

// scalarPrintSuffix 是「元素/键/值 → 运行时打印表后缀」的唯一判据。
// 只认基本类型：class 元素的容器在运行时的槽位是 void*（Box），没有对应打印
// 函数，必须由 emit 合成（否则会发出 aic_pr_list_Box 这种不存在的符号）。
func (c *Ctx) scalarPrintSuffix(t types.Type) (string, bool) {
	b, ok := t.(*types.Basic)
	if !ok {
		return "", false
	}
	if _, has := printSuffixes[b.Name]; !has {
		return "", false
	}
	return b.Name, true
}

// printSuffixes 是 aic_pr_* 打印表已实例化的**标量**后缀。
// 注意它比容器实例化表（types.RuntimeSuffix）更窄：打印表只有标量，class 元素
// 的容器在运行时是 void* 槽位，没有对应打印函数，必须由 emit 合成打印器。
var printSuffixes = map[string]bool{
	"i8": true, "i16": true, "i32": true, "i64": true,
	"u8": true, "u16": true, "u32": true, "u64": true,
	"usize": true, "f32": true, "f64": true,
	"bool": true, "str": true,
}

// printerFor 返回打印 t 的 C 函数名，必要时登记合成（幂等 = 实例缓存同款纪律）。
func (c *Ctx) printerFor(t types.Type) (string, error) {
	if name, ok := c.printerName(t); ok {
		return name, nil
	}
	if t == nil {
		return "aic_pr_nil", nil
	}
	// 接口值：动态类型未知，走见证表的**打印槽**（方法槽之后的追加槽，§十四 扩展）。
	if ifc, isIfc := types.IsInterface(t); isIfc {
		return c.interfacePrinter(ifc)
	}
	// 非标量元素容器 / 数组 / enum / class：登记合成（元素先登记 → 定义序天然自底向上）。
	key := typeSeg(t)
	if c.printSeen == nil {
		c.printSeen = map[string]string{}
	}
	if name, has := c.printSeen[key]; has {
		return name, nil
	}
	c.printSeq++
	name := fmt.Sprintf("aic_pr_syn_%d", c.printSeq)
	// 第二遍才发现 = 第一遍漏了：合成的定义已经发过，此处只登记名字会发出未声明的
	// 调用（C 层报错）。宁可在 emit 侧响亮失败，也不发半截代码。
	if c.seeded {
		return "", fmt.Errorf("emit: printer %s only surfaced on the second pass (the first pass collected incompletely)", t.String())
	}
	c.printSeen[key] = name
	// 先登记子打印函数（递归形状：内层先有名字）。
	if err := c.registerChildPrinters(t); err != nil {
		return "", err
	}
	c.printers = append(c.printers, printerEntry{name: name, ty: t})
	return name, nil
}

// registerChildPrinters 登记元素/负载/字段的打印函数（定义序自底向上）。
func (c *Ctx) registerChildPrinters(t types.Type) error {
	if types.IsSlice(t) {
		_, err := c.printerFor(types.SliceElem(t))
		return err
	}
	if types.IsSet(t) {
		_, err := c.printerFor(types.SetElem(t))
		return err
	}
	if types.IsMap(t) {
		k, v, _ := types.MapParts(t)
		if _, err := c.printerFor(k); err != nil {
			return err
		}
		_, err := c.printerFor(v)
		return err
	}
	if elem, _, ok := types.ArrayElem(t); ok {
		_, err := c.printerFor(elem)
		return err
	}
	if en, ok := types.IsEnum(t); ok {
		for _, va := range en.Variants {
			if va.Payload == nil {
				continue
			}
			if _, err := c.printerFor(va.Payload); err != nil {
				return err
			}
		}
		return nil
	}
	if cl, ok := types.IsClass(t); ok {
		for i := range cl.Fields {
			if _, err := c.printerFor(cl.Fields[i].Type); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("emit: type %s cannot be printed (§14: a class needs @derive(ToString))", t.String())
}

// emitPrinters 发射合成打印函数的原型与定义（在类型定义之后、函数原型之前）。
func (c *Ctx) emitPrinters() error {
	if len(c.printers) == 0 {
		return nil
	}
	for _, e := range c.printers {
		c.line("static void %s(%s, int depth);", e.name, c.cTypeDecl(e.ty, "v"))
	}
	c.line("")
	for _, e := range c.printers {
		if err := c.emitOnePrinter(e); err != nil {
			return err
		}
	}
	return nil
}

// emitOnePrinter 发射一个合成打印函数的定义。
func (c *Ctx) emitOnePrinter(e printerEntry) error {
	// [T;N] 是内联数组：C 形参必须写声明子形式 aic_i32 v[3]（类型名不能跟参数名）。
	c.line("static void %s(%s, int depth) {", e.name, c.cTypeDecl(e.ty, "v"))
	switch {
	case e.iface != nil:
		if err := c.emitIfacePrinterBody(e); err != nil {
			return err
		}
	case types.IsSlice(e.ty), types.IsSet(e.ty), types.IsMap(e.ty):
		if err := c.emitContainerPrinterBody(e); err != nil {
			return err
		}
	case types.IsArray(e.ty):
		if err := c.emitArrayPrinterBody(e); err != nil {
			return err
		}
	default:
		if en, ok := types.IsEnum(e.ty); ok {
			if err := c.emitEnumPrinterBody(e, en); err != nil {
				return err
			}
		} else if cl, ok := types.IsClass(e.ty); ok {
			if err := c.emitClassPrinterBody(e, cl); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("emit: cannot synthesise a printer for %s", e.ty.String())
		}
	}
	c.line("}")
	c.line("")
	return nil
}

// emitContainerPrinterBody 发射容器合成打印体（元素打印走子函数）。
func (c *Ctx) emitContainerPrinterBody(e printerEntry) error {
	var elem, key types.Type
	open, close := "aic_pr_lbracket()", "aic_pr_rbracket()"
	switch {
	case types.IsSlice(e.ty):
		elem = types.SliceElem(e.ty)
		c.line("    aic_usize i;")
		c.line("    if (v == NULL) { aic_pr_nil(); return; }")
		c.line("    %s;", open)
		c.line("    for (i = 0; i < v->len; i++) {")
		c.line("        if (i != 0) aic_pr_sep();")
	case types.IsSet(e.ty):
		elem = types.SetElem(e.ty)
		open, close = "aic_pr_lbrace()", "aic_pr_rbrace()"
		c.line("    aic_usize i;")
		c.line("    if (v == NULL) { aic_pr_nil(); return; }")
		c.line("    %s;", open)
		c.line("    for (i = 0; i < v->len; i++) {")
		c.line("        if (i != 0) aic_pr_sep();")
	case types.IsMap(e.ty):
		key, elem, _ = types.MapParts(e.ty)
		open, close = "aic_pr_lbrace()", "aic_pr_rbrace()"
		c.line("    aic_usize i;")
		c.line("    if (v == NULL) { aic_pr_nil(); return; }")
		c.line("    %s;", open)
		c.line("    for (i = 0; i < v->len; i++) {")
		c.line("        if (i != 0) aic_pr_sep();")
	}
	if key != nil {
		kn, err := c.printerFor(key)
		if err != nil {
			return err
		}
		c.line("        %s(v->keys[v->order[i]], depth + 1);", kn)
		c.line("        aic_pr_colon();")
	}
	en, err := c.printerFor(elem)
	if err != nil {
		return err
	}
	// 元素访问器：list 用 data[i]，set 用 slots[order[i]]，map 用 vals[order[i]]。
	switch {
	case types.IsSlice(e.ty):
		c.line("        %s(v->data[i], depth + 1);", en)
	case types.IsSet(e.ty):
		c.line("        %s(v->slots[v->order[i]], depth + 1);", en)
	default:
		c.line("        %s(v->vals[v->order[i]], depth + 1);", en)
	}
	c.line("    }")
	c.line("    %s;", close)
	return nil
}

// emitArrayPrinterBody 发射 [T;N] 的合成打印体（内联数组，值语义）。
func (c *Ctx) emitArrayPrinterBody(e printerEntry) error {
	elem, n, _ := types.ArrayElem(e.ty)
	en, err := c.printerFor(elem)
	if err != nil {
		return err
	}
	c.line("    int i;")
	c.line("    aic_pr_lbracket();")
	c.line("    for (i = 0; i < %d; i++) {", n)
	c.line("        if (i != 0) aic_pr_sep();")
	c.line("        %s(v[i], depth + 1);", en)
	c.line("    }")
	c.line("    aic_pr_rbracket();")
	return nil
}

// emitEnumPrinterBody 发射 enum 合成打印体：无数据 = 变体名；带数据 = Variant(负载)。
func (c *Ctx) emitEnumPrinterBody(e printerEntry, en *types.Enum) error {
	ct := c.cTypeName(e.ty)
	tag := "v"
	if en.HasData {
		tag = "v.tag"
	}
	c.line("    switch ((int)%s) {", tag)
	for i, va := range en.Variants {
		c.line("    case %d:", i)
		if va.Payload == nil {
			c.line("        aic_print_str(%s);", c.strLit(va.Name))
			c.line("        break;")
			continue
		}
		pn, err := c.printerFor(va.Payload)
		if err != nil {
			return err
		}
		c.line("        aic_print_str(%s);", c.strLit(va.Name))
		c.line("        aic_pr_lparen();")
		c.line("        %s(v.u.%s, depth + 1);", pn, va.Name)
		c.line("        aic_pr_rparen();")
		c.line("        break;")
	}
	c.line("    default:")
	c.line("        aic_print_str(%s);", c.strLit("?"+ct))
	c.line("        break;")
	c.line("    }")
	return nil
}

// emitClassPrinterBody 发射 class 合成打印体（@derive(ToString)：字段按声明序）。
// @packed = 值类型（C struct，按值传参，用 `.`）；其余 = 引用对象（句柄，用 `->`）。
func (c *Ctx) emitClassPrinterBody(e printerEntry, cl *types.Class) error {
	arrow := "->"
	if cl.Packed {
		arrow = "."
	} else {
		c.line("    if (v == NULL) { aic_pr_nil(); return; }")
	}
	c.line("    aic_print_str(%s);", c.strLit(cl.Name+"{"))
	for i := range cl.Fields {
		fd := cl.Fields[i]
		pn, err := c.printerFor(fd.Type)
		if err != nil {
			return err
		}
		if i > 0 {
			c.line("    aic_pr_sep();")
		}
		c.line("    aic_print_str(%s);", c.strLit(fd.Name+": "))
		c.line("    %s(v%s%s, depth + 1);", pn, arrow, fd.Name)
	}
	c.line("    aic_print_str(%s);", c.strLit("}"))
	return nil
}

// printValue 发射「打印表达式 e（类型 t）」的 C 表达式（不含换行）。
func (c *Ctx) printValue(e string, t types.Type) (string, error) {
	if t == nil {
		return fmt.Sprintf("aic_pr_nil()"), nil
	}
	// @packed 值类型 / 数组：按值传参；引用与容器：按句柄传参。
	pn, err := c.printerFor(t)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s(%s, 0)", pn, e), nil
}

// printExprOf 取表达式自身的定型（含未定型字面量的定宽回退）。
func (c *Ctx) printExprOf(arg parse.Expr) types.Type {
	t := c.ti(arg)
	if t == nil {
		t = c.literalType(arg)
	}
	return t
}
