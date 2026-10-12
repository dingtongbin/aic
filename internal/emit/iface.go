package emit

import (
	"fmt"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// interface 发射（核心设计 §四 值 = {data, vt}；§十 见证表 + trampoline）。
//
//   装箱    class → ((aic_iface){ .data = obj, .vt = &aic_vt_<Ifc>__<Ty> })
//   见证表  aic_vt_<IfcPkg>_<Ifc>__<TyPkg>_<Ty>[槽位] = 薄 trampoline
//   调用    ((retCT (*)(aic_iface, 形参…))(v).vt[槽])（(v), 实参…）
//   nil     调用前判 data == NULL → trap（消息含位置）
//
// 薄 trampoline 首参是 aic_iface，内部取 self.data 后直调具体方法 —— 方法体只有
// 一份（禁为每个接口复制方法体）。见证表按 (接口, 具体类) 配对登记，名字唯一且
// 每 TU 自足（单文件编译 + 无 LTO 下跨 TU extern 见证表会有链接期顺序问题）。
//
// @packed 满足接口 = 装箱**值副本**（当前区域分配带头对象、拷入 packed 值；
// 经接口修改副本不回写原值，§四）。
// ---------------------------------------------------------------------------

// witnessEntry 是一张已登记的见证表。
type witnessEntry struct {
	name string
	cl   *types.Class
	ifc  *types.Interface
}

// witnessName 是见证表的 C 符号名（唯一实现，与 §八 命名规则同源）。
func witnessName(ifc *types.Interface, cl *types.Class) string {
	return fmt.Sprintf("aic_vt_%s_%s__%s_%s", ifc.Pkg, ifc.Name, cl.Pkg, cl.Name)
}

// witnessFor 登记（或复用）一张见证表，返回其 C 名。
func (c *Ctx) witnessFor(cl *types.Class, ifc *types.Interface) (string, error) {
	if cl == nil || ifc == nil {
		return "", fmt.Errorf("emit: a witness table needs a (concrete class, interface) pair")
	}
	if !types.Satisfies(cl, ifc) {
		return "", fmt.Errorf("emit: class %s does not satisfy interface %s", cl.String(), ifc.String())
	}
	name := witnessName(ifc, cl)
	if c.witnessSeen == nil {
		c.witnessSeen = map[string]bool{}
	}
	if !c.witnessSeen[name] {
		if c.seeded {
			return "", fmt.Errorf("emit: witness table %s only surfaced on the second pass (the first pass collected incompletely)", name)
		}
		c.witnessSeen[name] = true
		c.witnesses = append(c.witnesses, witnessEntry{name: name, cl: cl, ifc: ifc})
	}
	return name, nil
}

// emitWitnesses 发射见证表与薄 trampoline（在原型段之前）。
func (c *Ctx) emitWitnesses() error {
	if len(c.witnesses) == 0 {
		return nil
	}
	// trampoline 需要具体方法的原型（方法体在后面发射），故先发全部原型。
	// 具体方法的原型在此处补发：见证表在原型段之前发射，缺了它 C 会报
	// 「implicit declaration」（重复声明同一原型在 C 里合法）。
	for i, w := range c.witnesses {
		for _, slot := range types.InterfaceSlots(w.ifc) {
			msig, _ := w.cl.Method(slot)
			if msig == nil {
				return fmt.Errorf("emit: slot %s of witness table %s has no signature", w.name, slot)
			}
			c.line("static %s %s(%s);", c.retCT(msig), c.cMethodName(w.cl, slot), c.paramList(msig))
			c.line("static %s %s(aic_iface self%s);", c.retCT(msig), c.trampName(i, slot),
				commaPrefix(c.paramListNoThis(msig)))
		}
	}
	c.line("")
	for i, w := range c.witnesses {
		slots := types.InterfaceSlots(w.ifc)
		for _, slot := range slots {
			if err := c.emitTrampoline(i, w, slot); err != nil {
				return err
			}
		}
		needPrint := c.ifacePrint[ifacePrintKey(w.ifc)]
		if needPrint {
			if err := c.emitIfacePrintTramp(i, w); err != nil {
				return err
			}
		}
		c.line("static const void *%s[] = {", w.name)
		for _, slot := range slots {
			c.line("    (const void *)%s,", c.trampName(i, slot))
		}
		if needPrint {
			c.line("    (const void *)%s,", c.ifaceTrampName(i))
		}
		c.line("};")
		c.line("")
	}
	return nil
}

// trampName 是薄 trampoline 的 C 名（按见证表序号 + 槽位名，确定性）。
func (c *Ctx) trampName(table int, slot string) string {
	return fmt.Sprintf("aic_tramp_%d_%s", table+1, slot)
}

// emitTrampoline 发射一个槽位的薄 trampoline：取 self.data 后直调具体方法。
func (c *Ctx) emitTrampoline(table int, w witnessEntry, slot string) error {
	msig, _ := w.cl.Method(slot)
	// data 指向**对象**（typedef 名），不是「指针的指针」：cTypeName(引用类) 带 ` *`，
	// 直接拼会得到 `aic_ok_Rect * *`。@packed 的方法 this 是**值**（装箱副本），
	// 故取 `*` 解引用（§四：@packed 满足接口 = 装箱值副本）。
	ct := c.typedefName(c.ownerPkg(w.cl.Pkg), w.cl.Name)
	recv := fmt.Sprintf("(%s *)self.data", ct)
	if w.cl.Packed {
		recv = "*" + recv
	}
	args := make([]string, 0, len(msig.Params))
	for _, p := range msig.Params {
		args = append(args, p)
	}
	call := fmt.Sprintf("%s(%s%s)", c.cMethodName(w.cl, slot), recv, commaPrefix(joinComma(args)))
	c.line("static %s %s(aic_iface self%s) {", c.retCT(msig), c.trampName(table, slot),
		commaPrefix(c.paramListNoThis(msig)))
	c.line("    return %s;", call)
	c.line("}")
	return nil
}

// commaPrefix 给形参表加前置逗号（空表返回空串，非空返回 ", " + 表）。
func commaPrefix(params string) string {
	if params == "" || params == "void" {
		return ""
	}
	return ", " + params
}

// ---------------------------------------------------------------------------
// 接口值的打印（§十四 的扩展槽）：接口的动态类型在编译期未知，故见证表在方法槽
// 之后追加**打印槽**（下标 = 接口方法数）。打印槽的 trampoline 调具体类型的打印
// 函数；具体类型没有 @derive(ToString) 时打印 `<类名>`（确定性、不 trap）。
// ---------------------------------------------------------------------------

// ifacePrintKey 是接口打印槽的登记键（包名限定，跨包同名接口不混）。
func ifacePrintKey(ifc *types.Interface) string { return ifc.Pkg + "." + ifc.Name }

// interfacePrinter 登记（或复用）一个接口打印函数，返回其 C 名。
func (c *Ctx) interfacePrinter(ifc *types.Interface) (string, error) {
	key := ifacePrintKey(ifc)
	if c.printSeen == nil {
		c.printSeen = map[string]string{}
	}
	if name, has := c.printSeen[key]; has {
		return name, nil
	}
	if c.seeded {
		return "", fmt.Errorf("emit: the printer for interface %s only surfaced on the second pass (the first pass collected incompletely)", ifc.String())
	}
	c.printSeq++
	name := fmt.Sprintf("aic_pr_syn_%d", c.printSeq)
	c.printSeen[key] = name
	if c.ifacePrint == nil {
		c.ifacePrint = map[string]bool{}
	}
	c.ifacePrint[key] = true
	c.printers = append(c.printers, printerEntry{name: name, ty: ifc, iface: ifc})
	return name, nil
}

// emitIfacePrinterBody 发射接口打印体（转发到见证表的打印槽）。
func (c *Ctx) emitIfacePrinterBody(e printerEntry) error {
	slot := len(types.InterfaceSlots(e.iface))
	c.line("    if (v.data == NULL) { aic_pr_nil(); return; }")
	c.line("    ((void (*)(aic_iface))(v.vt[%d]))(v);", slot)
	return nil
}

// emitIfacePrintTramp 发射某个具体类在接口打印槽里的 trampoline。
func (c *Ctx) emitIfacePrintTramp(table int, w witnessEntry) error {
	c.line("static void %s(aic_iface self) {", c.ifaceTrampName(table))
	if w.cl.HasDerive("ToString") {
		pn, err := c.printerFor(w.cl)
		if err != nil {
			return err
		}
		recv := fmt.Sprintf("(%s *)self.data", c.typedefName(c.ownerPkg(w.cl.Pkg), w.cl.Name))
		if w.cl.Packed {
			recv = "*" + recv
		}
		c.line("    %s(%s, 0);", pn, recv)
	} else {
		c.line("    aic_print_str(%s);", c.strLit("<"+w.cl.Name+">"))
	}
	c.line("}")
	return nil
}

// ifaceTrampName 是打印槽 trampoline 的 C 名。
func (c *Ctx) ifaceTrampName(table int) string {
	return fmt.Sprintf("aic_tramp_%d_print", table+1)
}
