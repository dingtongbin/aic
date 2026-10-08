package emit

import (
	"fmt"
	"sort"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 泛型实例（T1）与 std option。
//
// 名字规则沿用 §八 实例命名：类型 typedef = aic_<pkg>_<T>[_<A1>_…]，
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
// 与 §八 类型 typedef 规则同源，禁第二份拼法。
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

// instanceName 按实例命名规则拼出 C 类型名（键与名同源，红线 10）。
func (c *Ctx) instanceName(inst *types.Instance) string {
	pkg, typ := instPkgName(inst)
	name := "aic_" + pkg + "_" + typ
	for _, a := range inst.Args {
		name += "_" + typeSeg(a)
	}
	return name
}

// registerInstance 登记实例并返回其 C 类型名（同一实例只登记一次 = 实例缓存，红线 11）。
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

// scanInstanceTypes 递归登记类型里出现的全部泛型实例（容器/数组/函数类型都穿透）。
func (c *Ctx) scanInstanceTypes(t types.Type, depth int) {
	if t == nil || depth > 8 {
		return
	}
	if inst, ok := t.(*types.Instance); ok {
		c.registerInstance(inst)
		for _, a := range inst.Args {
			c.scanInstanceTypes(a, depth+1)
		}
		return
	}
	if elem, _, ok := types.ArrayElem(t); ok {
		c.scanInstanceTypes(elem, depth+1)
		return
	}
	if types.IsSlice(t) {
		c.scanInstanceTypes(types.SliceElem(t), depth+1)
		return
	}
	if types.IsSet(t) {
		c.scanInstanceTypes(types.SetElem(t), depth+1)
		return
	}
	if types.IsMap(t) {
		k, v, _ := types.MapParts(t)
		c.scanInstanceTypes(k, depth+1)
		c.scanInstanceTypes(v, depth+1)
		return
	}
	if params, res, ok := types.FuncParts(t); ok {
		for _, p := range params {
			c.scanInstanceTypes(p, depth+1)
		}
		c.scanInstanceTypes(res, depth+1)
	}
}

// collectInstances 在发射类型定义之前收集全部实例（emitTypes 之前调用）。
// 来源：表达式类型表 / 函数签名 / 类字段 / 枚举负载 / pass-1 名字表。
//
// **遍历顺序必须确定**（H2）：Types/Classes/Enums/names 都是 Go map，直接 range 的
// 顺序随机会让实例登记序（以及 aic_pr_syn_N / 见证表编号）在两次发射间不同 ——
// 实测这就是 H2「两次 emit 文本不一致」的根因。故一律先排序再遍历。
func (c *Ctx) collectInstances() {
	if c.Info == nil {
		return
	}
	// 表达式类型表：按源位置排序（同一份输入 → 同一顺序）。
	type typedExpr struct {
		pos parse.Pos
		typ types.Type
	}
	exprs := make([]typedExpr, 0, len(c.Info.Types))
	for e, t := range c.Info.Types {
		exprs = append(exprs, typedExpr{pos: parse.ExprPos(e), typ: t})
	}
	sort.Slice(exprs, func(i, j int) bool {
		a, b := exprs[i].pos, exprs[j].pos
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
	for _, te := range exprs {
		c.scanInstanceTypes(te.typ, 0)
	}
	for _, sig := range c.allSigs() {
		for _, p := range sig.ParamTypes {
			c.scanInstanceTypes(p, 0)
		}
		for _, r := range sig.Results {
			c.scanInstanceTypes(r, 0)
		}
	}
	for _, name := range sortedClassNames(c.Info.Classes) {
		cl := c.Info.Classes[name]
		for i := range cl.Fields {
			c.scanInstanceTypes(cl.Fields[i].Type, 0)
		}
	}
	for _, name := range sortedEnumNames(c.Info.Enums) {
		en := c.Info.Enums[name]
		for _, va := range en.Variants {
			c.scanInstanceTypes(va.Payload, 0)
		}
	}
	keys := make([]string, 0, len(c.names))
	for k := range c.names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, name := range sortedTypeNames(c.names[k]) {
			c.scanInstanceTypes(c.names[k][name], 0)
		}
	}
}

// sortedClassNames / sortedEnumNames / sortedTypeNames：map 键排序（确定性遍历）。
func sortedClassNames(m map[string]*types.Class) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedEnumNames(m map[string]*types.Enum) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedTypeNames(m map[string]types.Type) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
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
			if !cl.Packed {
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

// someInstance 解析 Some(x) 的 Option 实例：浅层线索 → 调用结果类型。
func (c *Ctx) someInstance(v *parse.Call, want types.Type) (*types.Instance, bool) {
	if inst, ok := c.someInstanceShallow(v, want); ok {
		return inst, true
	}
	if inst, ok := types.IsOptionInstance(c.ti(v)); ok {
		return inst, true
	}
	return nil, false
}

// optionSomeCT 发射 Some(x) 或 option.ok(x)：目标类型 = 期望类型 / 结果类型。
func (c *Ctx) optionSomeCT(v *parse.Call, arg parse.Expr, want types.Type) (string, error) {
	inst, ok := c.someInstance(v, want)
	if !ok {
		return "", fmt.Errorf("emit: the result type of Some/option.ok is not an Option instance (line %d)", v.Pos.Line)
	}
	ty := c.cTypeName(inst)
	some, _ := optionTags(ty)
	a, err := c.expr(arg, inst.Args[0])
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("((%s){ .tag = %s, .u.Some = %s })", ty, some, a), nil
}

// optionNoneCT 发射裸 None 或 option.none()：目标类型 = 该表达式的类型。
func (c *Ctx) optionNoneCT(e parse.Expr, line int) (string, error) {
	inst, ok := types.IsOptionInstance(c.ti(e))
	if !ok {
		return "", fmt.Errorf("emit: the target type of None is not an Option instance (line %d)", line)
	}
	ty := c.cTypeName(inst)
	_, none := optionTags(ty)
	return fmt.Sprintf("((%s){ .tag = %s })", ty, none), nil
}

// stdOptionCall: option 包 (ok/none/isSome/isNone/unwrap)。
func (c *Ctx) stdOptionCall(v *parse.Call, fn string) (string, error) {
	if c.Info == nil || !c.Info.Imported["option"] {
		return "", fmt.Errorf("emit: option.%s needs import option (line %d)", fn, v.Pos.Line)
	}
	argCT := func(i int) (types.Instance, string, error) {
		if i >= len(v.Args) {
			return types.Instance{}, "", fmt.Errorf("emit: option.%s is missing its argument (line %d)", fn, v.Pos.Line)
		}
		inst, ok := types.IsOptionInstance(c.ti(v.Args[i]))
		if !ok {
			return types.Instance{}, "", fmt.Errorf("emit: the argument of option.%s is not an Option value (line %d)", fn, v.Pos.Line)
		}
		e, err := c.expr(v.Args[i], inst)
		return *inst, e, err
	}
	switch fn {
	case "ok":
		if len(v.Args) != 1 {
			return "", fmt.Errorf("emit: option.ok takes exactly one argument (line %d)", v.Pos.Line)
		}
		return c.optionSomeCT(v, v.Args[0], nil)
	case "none":
		return c.optionNoneCT(v, v.Pos.Line)
	case "isSome", "isNone":
		_, arg, err := argCT(0)
		if err != nil {
			return "", err
		}
		op := "== 0"
		if fn == "isNone" {
			op = "!= 0"
		}
		return fmt.Sprintf("((%s).tag %s)", paren("", arg), op), nil
	case "unwrap":
		_, arg, err := argCT(0)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("((%s).u.Some)", paren("", arg)), nil
	}
	return "", fmt.Errorf("emit: unsupported option.%s (line %d)", fn, v.Pos.Line)
}
