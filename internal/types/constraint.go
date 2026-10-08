package types

import (
	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// N3 泛型约束（核心设计 §16 N3）。
//
// 语法：`func max[T: Ord](a T, b T) -> T` / `[T: Hash + Eq]` / `[T: Shape]` /
// `[T: value]` / `[T: ref]`。
//
// 三类约束，语义各不相同：
//   - **能力约束**（Eq / Ord / Hash / Print / Zero）= @derive 的**同一封闭集合**，
//     不引入新概念；
//   - **接口约束** = 结构化满足（与接口变量同一机制，按见证表分发）；
//   - **值性约束**（value / ref）= 统一现在散落的"安全集"判断。
//
// 报错位置：**声明处**（约束名不认识）与**调用点**（实参不满足）——
// 绝不报在模板体内部（那会让用户去读一份他没写的代码）。
// ---------------------------------------------------------------------------

// constraintsOfParam 取**当前函数**某个类型形参的约束（不在泛型体里 = nil）。
func (c *Checker) constraintsOfParam(name string) []string {
	if c.fn == nil {
		return nil
	}
	for i := range c.fn.Constraints {
		if c.fn.Constraints[i].Name == name {
			return c.fn.Constraints[i].Constraints
		}
	}
	return nil
}

// opAllowedByConstraint 报告约束集合是否授权某个运算符（N3）：
// Ord ⇒ </<=/>/>=/==/!=；Eq ⇒ ==/!=；接口约束 ⇒ ==/!=（接口按内容比较不安全，
// 故接口只授权方法调用，不授权运算符）。
func opAllowedByConstraint(cons []string, op string) bool {
	has := func(name string) bool {
		for _, c := range cons {
			if c == name {
				return true
			}
		}
		return false
	}
	switch op {
	case "==", "!=":
		return has("Eq") || has("Ord")
	case "<", "<=", ">", ">=":
		return has("Ord")
	}
	return false
}

// resolveTypeArgName 把一个**类型名表达式**解析成语义类型（N4 内省原语的实参）：
// 支持 `i32` 这样的基本类型名、本包具名类型、跨包 `pkg.Type`。
// 非类型名 = 返回 nil（调用方报"需要类型名"）。
func (c *Checker) resolveTypeArgName(e parse.Expr) Type {
	switch v := e.(type) {
	case *parse.Ident:
		if b, ok := basicType(v.Name); ok {
			return b
		}
		if v.Name == "bytes" {
			return TBytes
		}
		return c.lookupType(v.Name)
	case *parse.Field:
		id, isID := v.X.(*parse.Ident)
		if !isID {
			return nil
		}
		if dep := c.deps[id.Name]; dep != nil {
			if cl, ok := dep.Classes[v.Name]; ok {
				return cl
			}
			if en, ok := dep.Enums[v.Name]; ok {
				return en
			}
			if ifc, ok := dep.Interfaces[v.Name]; ok {
				return ifc
			}
		}
	}
	return nil
}

// checkComptimeCall 检查一次内省调用（N4）：定型 + 记折叠结果（emit 按它发常量）。
func (c *Checker) checkComptimeCall(v *parse.Call, name string, expect Type) (Type, bound, untyped) {
	if len(v.Args) != 1 {
		c.errorAt(v.Pos, name+" takes exactly one type argument", name+"(T)",
			"write a type name: "+name+"(i32) / "+name+"(Point) / "+name+"(pkg.Point) (core design §16 N4)")
		return nil, nilBound, untyped{}
	}
	t := c.resolveTypeArgName(v.Args[0])
	if t == nil {
		c.errorAt(v.Pos, name+" needs a type name as its argument", exprText(v.Args[0]),
			"the introspection primitives take a **type**, not a value: "+name+"(Point) (core design §16 N4)")
		return nil, nilBound, untyped{}
	}
	cv := ComptimeVal{Kind: name, Ty: t}
	val, ferr := foldComptime(v, c)
	if ferr == "" {
		cv.Const = val
	} else if name != "sizeOf" {
		c.errorAt(v.Pos, ferr, name+"("+typeText(t)+")",
			"see core design §16 N4 for the introspection surface")
		return nil, nilBound, untyped{}
	}
	if c.comptime != nil {
		c.comptime[v] = cv
	}
	switch name {
	case "typeName":
		return TStr, nilBound, untyped{}
	case "sizeOf":
		return TI32, nilBound, untyped{}
	case "isValueType":
		return TBool, nilBound, untyped{}
	}
	return nil, nilBound, untyped{}
}

// CapabilityNames 是能力约束的封闭枚举（与 @derive 同源，禁第二份）。
var CapabilityNames = map[string]bool{
	"Eq": true, "Ord": true, "Hash": true, "Print": true, "Zero": true,
}

// ValueConstraintNames 是值性约束。
var ValueConstraintNames = map[string]bool{"value": true, "ref": true}

// checkConstraintDecl 校验声明处的约束名（不认识 = 编译错，位置 = 声明处）。
func (c *Checker) checkConstraintDecl(owner string, tpc parse.TypeParamConstraint) {
	for _, name := range tpc.Constraints {
		if CapabilityNames[name] || ValueConstraintNames[name] {
			continue
		}
		if _, ok := c.ifaces[name]; ok {
			continue
		}
		if c.lookupInterfaceRef(name) != nil {
			continue
		}
		c.errorAt(tpc.Pos, "unknown generic constraint", owner+"["+tpc.Name+": "+name+"]",
			"constraints are the @derive capabilities (Eq/Ord/Hash/Print/Zero), a declared interface, or value/ref (core design §16 N3)")
	}
}

// satisfiesConstraints 在**调用点**校验实参是否满足约束（逐个约束给具体理由）。
// 返回 false = 已报错。
func (c *Checker) satisfiesConstraints(at parse.Pos, owner, param string, t Type, cons []string) bool {
	ok := true
	for _, name := range cons {
		switch {
		case name == "Eq":
			if bad := comparableType(t); bad != "" {
				c.errorAt(at, t.String()+" does not satisfy Eq", owner+"["+param+": Eq]: "+bad,
					"== needs comparable fields: numeric / bool / str / payload-free enum / @packed / Eq-enabled class (core design §16 N3)")
				ok = false
			}
		case name == "Ord":
			if !orderedType(t) {
				c.errorAt(at, t.String()+" does not satisfy Ord", owner+"["+param+": Ord]",
					"Ord needs a total order: numeric / str, or a class with @derive(Compare) (core design §16 N3)")
				ok = false
			}
		case name == "Hash":
			if bad := hashableType(t); bad != "" {
				c.errorAt(at, t.String()+" does not satisfy Hash", owner+"["+param+": Hash]: "+bad,
					"Hash needs hashable fields: numeric / bool / str / payload-free enum / @packed / Hash-enabled class (core design §16 N3)")
				ok = false
			}
		case name == "Print":
			if bad := unprintableClass(t, 0); bad != nil {
				c.errorAt(at, t.String()+" does not satisfy Print", owner+"["+param+": Print]: "+bad.String()+" has no ToString",
					"add @derive(ToString) to the type, or drop the Print constraint (core design §16 N3)")
				ok = false
			}
		case name == "Zero":
			// 每种类型都有零值（§二.3），恒满足 —— 保留这个名字是为了让约束
			// 集合与 @derive 完全一致（同一封闭集合，不引入新概念）。
		case name == "value":
			if !isValueType(t) {
				c.errorAt(at, t.String()+" does not satisfy value", owner+"["+param+": value]",
					"a value constraint asks for a type without a region header: numeric / bool / str / Err / enum / @packed / [T;N] / fn (core design §16 N3)")
				ok = false
			}
		case name == "ref":
			if !isReference(t) {
				c.errorAt(at, t.String()+" does not satisfy ref", owner+"["+param+": ref]",
					"a ref constraint asks for a region-managed type: class / interface / container (core design §16 N3)")
				ok = false
			}
		default:
			// 接口约束：结构化满足 + 见证表分发。
			ifc := c.lookupInterfaceRef(name)
			if ifc == nil {
				continue // 声明处已报过
			}
			switch tv := t.(type) {
			case *Class:
				if bad, ok2 := interfaceMissing(c, tv, ifc); !ok2 {
					c.errorAt(at, t.String()+" does not satisfy "+name, owner+"["+param+": "+name+"]: "+bad,
						"give the class the missing method, or drop the constraint (core design §16 N3)")
					ok = false
				}
			case *Instance:
				cl, isCl := IsClass(tv.Base)
				if !isCl {
					c.errorAt(at, t.String()+" does not satisfy "+name, owner+"["+param+": "+name+"]",
						"only a class (or class instance) can satisfy an interface (core design §16 N3)")
					ok = false
					continue
				}
				if bad, ok2 := interfaceMissing(c, cl, ifc); !ok2 {
					c.errorAt(at, t.String()+" does not satisfy "+name, owner+"["+param+": "+name+"]: "+bad,
						"give the class the missing method, or drop the constraint (core design §16 N3)")
					ok = false
				}
			default:
				c.errorAt(at, t.String()+" does not satisfy "+name, owner+"["+param+": "+name+"]",
					"an interface constraint needs a class argument (core design §16 N3)")
				ok = false
			}
		}
	}
	return ok
}

// comparableType 报告类型是否可比较（返回空串 = 可以；否则给理由）。
// 与 comparableField 同源（字段级判据唯一所有者在那里），这里只补类型级入口。
func comparableType(t Type) string {
	switch v := t.(type) {
	case *Basic:
		return ""
	case *Class:
		why, _ := firstNonComparable(v)
		return why
	case *Enum:
		if !v.HasData {
			return ""
		}
		for _, va := range v.Variants {
			if va.Payload == nil {
				continue
			}
			if why := comparableType(va.Payload); why != "" {
				return "enum payload: " + why
			}
		}
		return ""
	case *ArrayT:
		if why := comparableType(v.Elem); why != "" {
			return "array element: " + why
		}
		return ""
	case nil:
		return ""
	}
	return "this type has no equality (containers, interfaces and functions compare by nothing)"
}

// hashableType 报告类型是否可哈希（返回空串 = 可以；否则给理由）。
func hashableType(t Type) string {
	switch v := t.(type) {
	case *Basic:
		return ""
	case *Class:
		if !v.Packed {
			return "a reference class has no content hash; use @packed or @derive(Hash)"
		}
		why, _ := firstNonHashable(v)
		return why
	case *Enum:
		if !v.HasData {
			return ""
		}
		for _, va := range v.Variants {
			if va.Payload == nil {
				continue
			}
			if why := hashableType(va.Payload); why != "" {
				return "enum payload: " + why
			}
		}
		return ""
	case *ArrayT:
		if why := hashableType(v.Elem); why != "" {
			return "array element: " + why
		}
		return ""
	case nil:
		return ""
	}
	return "this type has no hash (containers, interfaces and functions are not hashable)"
}

// orderedType 报告类型是否自带全序（数值 / str / @derive(Compare) 的类）。
func orderedType(t Type) bool {
	if b, ok := t.(*Basic); ok {
		switch b.Name {
		case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize",
			"f32", "f64", "str":
			return true
		}
		return false
	}
	if cl, ok := t.(*Class); ok {
		return cl.HasDerive("Compare")
	}
	return false
}

// interfaceMissing 报告类相对接口缺什么（满足 = ok）。
func interfaceMissing(c *Checker, cl *Class, ifc *Interface) (string, bool) {
	for _, slot := range InterfaceSlots(ifc) {
		want, _ := ifc.Method(slot)
		got, has := cl.Method(slot)
		if !has {
			return "missing method " + slot, false
		}
		if !sameSig(want, got) {
			return "method " + slot + " has a different signature", false
		}
	}
	return "", true
}

// checkCallConstraints 在泛型调用/实例化点校验约束（m 是解出的类型实参）。
// owner = 诊断里显示的模板名（函数名或类名）。
func (c *Checker) checkCallConstraints(at parse.Pos, owner string, tps []parse.TypeParamConstraint, m map[string]Type) bool {
	ok := true
	for _, tpc := range tps {
		if len(tpc.Constraints) == 0 {
			continue
		}
		arg := m[tpc.Name]
		if arg == nil {
			continue // 解不出已在别处报错
		}
		if !c.satisfiesConstraints(at, owner, tpc.Name, arg, tpc.Constraints) {
			ok = false
		}
	}
	return ok
}
