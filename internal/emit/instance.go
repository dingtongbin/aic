package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 泛型实例（T1）与 std option。
//
// 名字规则沿用实例命名：类型 typedef = aic_<pkg>_<T>[_<A1>_…]，
// 例如 Option[i32] → aic_option_Option_i32。实例登记是**确定性**的（按登记序）。
//
// T1 覆盖 enum 实例（Option[T] 是第一个，也是泛型枚举的全部形态）；
// class 实例（Box[T]）随后的批次补——此处显式 warn，不静默发错。
// ---------------------------------------------------------------------------

// instEntry 是一个已登记的泛型实例。
type instEntry struct {
	inst *types.Instance
	name string
}

// instPkgName 取实例基类型的 (包名, 类型名) 段。
func instPkgName(inst *types.Instance) (string, string) {
	switch b := inst.Base.(type) {
	case *types.Enum:
		return types.EnumPkg(b), b.Name
	case *types.Class:
		return b.Pkg, b.Name
	}
	return "pkg", "T"
}

// typeSeg 是实例实参的 mangling 段（不是 C 类型名：i32 而非 aic_i32）。
// 与类型 typedef 规则同源，禁第二份拼法。
func typeSeg(t types.Type) string {
	if t == nil {
		return "t"
	}
	switch {
	case types.IsStrType(t):
		return "str"
	case types.IsErrType(t):
		return "Err"
	}
	if b, ok := t.(*types.Basic); ok {
		return b.Name
	}
	if cl, ok := types.IsClass(t); ok {
		return cl.Pkg + "_" + cl.Name
	}
	if en, ok := types.IsEnum(t); ok {
		return types.EnumPkg(en) + "_" + en.Name
	}
	if inst, ok := t.(*types.Instance); ok {
		pkg, typ := instPkgName(inst)
		out := pkg + "_" + typ
		for _, a := range inst.Args {
			out += "_" + typeSeg(a)
		}
		return out
	}
	if types.IsSlice(t) {
		return typeSeg(types.SliceElem(t)) + "_arr"
	}
	if types.IsSet(t) {
		return typeSeg(types.SetElem(t)) + "_set"
	}
	if types.IsMap(t) {
		k, v, _ := types.MapParts(t)
		return typeSeg(k) + "_map_" + typeSeg(v)
	}
	if elem, n, ok := types.ArrayElem(t); ok {
		return fmt.Sprintf("%s_array%d", typeSeg(elem), n)
	}
	if params, res, ok := types.FuncParts(t); ok {
		out := "fn"
		for _, p := range params {
			out += "_" + typeSeg(p)
		}
		return out + "_" + typeSeg(res)
	}
	return "t"
}

// instanceName 按实例命名规则拼出 C 类型名（键与名同源）。
func (c *Ctx) instanceName(inst *types.Instance) string {
	pkg, typ := instPkgName(inst)
	name := "aic_" + pkg + "_" + typ
	for _, a := range inst.Args {
		name += "_" + typeSeg(a)
	}
	return name
}

// registerInstance 登记实例并返回其 C 类型名（同一实例只登记一次 = 实例缓存）。
func (c *Ctx) registerInstance(inst *types.Instance) string {
	if inst == nil {
		return "void"
	}
	name := c.instanceName(inst)
	if c.instSeen == nil {
		c.instSeen = map[string]bool{}
	}
	if !c.instSeen[name] {
		c.instSeen[name] = true
		c.insts = append(c.insts, instEntry{inst: inst, name: name})
	}
	return name
}

// emitInstances 发射实例的 tag 枚举与结构定义（在用户具名类型之后、原型之前）。
func (c *Ctx) emitInstances() {
	if len(c.insts) == 0 {
		return
	}
	// 索引式循环：发射过程中可能发现嵌套实例（Option[Option[i32]]），
	// 新登记项也要在同一趟里发出去。
	for i := 0; i < len(c.insts); i++ {
		e := c.insts[i]
		// 泛型**类**实例：typedef 名 = 实例键，字段按实参代换（Box[i32] → aic_i32 v）。
		if cl, isClass := e.inst.Base.(*types.Class); isClass {
			c.line("typedef struct %s %s;", e.name, e.name)
			c.line("struct %s {", e.name)
			if !cl.Struct {
				c.line("    aic_hdr hdr;")
			}
			for fi := range cl.Fields {
				fd := cl.Fields[fi]
				ft := types.Subst(fd.Type, cl.TypeParams, e.inst.Args)
				c.line("    %s %s;", c.cTypeName(ft), fd.Name)
			}
			if len(cl.Fields) == 0 {
				c.line("    aic_u8 _empty;")
			}
			c.line("};")
			c.line("")
			continue
		}
		en, isEnum := e.inst.Base.(*types.Enum)
		if !isEnum {
			c.warn("generic instance %s has no emittable base type", e.name)
			continue
		}
		hasData := en.HasData
		if !hasData {
			c.line("typedef enum %s %s;", e.name, e.name)
			c.line("enum %s {", e.name)
			for i, va := range en.Variants {
				sep := ","
				if i == len(en.Variants)-1 {
					sep = ""
				}
				c.line("    %s_%s = %d%s", e.name, va.Name, i, sep)
			}
			c.line("};")
			c.line("")
			continue
		}
		c.line("typedef enum %s_tag %s_tag;", e.name, e.name)
		c.line("typedef struct %s %s;", e.name, e.name)
		c.line("enum %s_tag {", e.name)
		for i, va := range en.Variants {
			sep := ","
			if i == len(en.Variants)-1 {
				sep = ""
			}
			c.line("    %s_%s = %d%s", e.name, va.Name, i, sep)
		}
		c.line("};")
		c.line("struct %s {", e.name)
		c.line("    %s_tag tag;", e.name)
		c.line("    union {")
		for _, va := range en.Variants {
			if va.Payload == nil {
				continue
			}
			pt := types.Subst(va.Payload, en.TypeParams, e.inst.Args)
			if pt == nil {
				continue
			}
			c.line("        %s %s;", c.cTypeName(pt), va.Name)
		}
		c.line("    } u;")
		c.line("};")
		c.line("")
	}
}

// optionVariantTag 返回 Option 变体在实例 tag 枚举里的名字（<实例名>_<变体>）。
func optionTags(instName string) (some, none string) {
	return instName + "_Some", instName + "_None"
}

// someInstanceShallow 只看「期望类型 / 实参类型（含字面量定宽）」，**不查调用结果类型**
// ——callResultType 会回调到这里，查结果类型会无限递归。
func (c *Ctx) someInstanceShallow(v *parse.Call, want types.Type) (*types.Instance, bool) {
	if inst, ok := types.IsOptionInstance(want); ok {
		return inst, true
	}
	if len(v.Args) == 1 {
		at := c.ti(v.Args[0])
		if at == nil {
			at = c.literalType(v.Args[0])
		}
		if at != nil {
			cand := &types.Instance{Base: types.OptionBaseEnum(), Args: []types.Type{at}}
			if inst, ok := types.IsOptionInstance(cand); ok {
				return inst, true
			}
		}
	}
	return nil, false
}
