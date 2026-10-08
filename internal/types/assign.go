package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// 可赋值性（核心设计 §二.4：无隐式转换，唯一例外 = 未类型化字面量的范围证明
// 与下标/for-in 计数位的任意整型）。
// ---------------------------------------------------------------------------

// requireAssignableAt 校验「值 → 目标类型」；违者报三段式（H6）。
// 未类型化字面量（vt == nil 而 vinfo 有值）在此完成范围证明——这是无隐式
// 转换世界里字面量的唯一落型通道。
func (c *Checker) requireAssignableAt(value parse.Expr, vt Type, vinfo untyped, target Type, what string) {
	if target == nil {
		return
	}
	// 未类型化字面量：范围证明后落型
	if vinfo.kind != unNone && vinfo.kind != unNil {
		if c.untypedFits(vinfo, target) {
			return
		}
		c.errorAt(parse.ExprPos(value), "the literal does not fit the target type",
			what+": value "+literalText(vinfo)+" does not fit "+target.String(),
			"adjust the literal or use a wider type; an integer literal takes the narrowest type that fits (core design §1)")
		return
	}
	if isUntypedNil(vinfo) {
		// nil = 全类型零值（§二.3）：任何类型可赋
		return
	}
	if vt == nil {
		return // 上游错误已报
	}
	if identical(vt, target) {
		return
	}
	// interface 装箱（§四 结构化满足）：类 → 接口，方法签名集完全匹配即可赋值。
	// 无 implements 关键字：满足关系完全由方法签名决定（红线 1 的又一处例外位，
	// 但它是**装箱**不是隐式数值转换，与 §二.4 不冲突）。
	if ifc, isIfc := target.(*Interface); isIfc {
		if cl, isCl := vt.(*Class); isCl {
			if miss, ok := satisfies(cl, ifc); ok {
				return
			} else {
				c.errorAt(parse.ExprPos(value), "the class does not satisfy the interface",
					what+": "+cl.String()+" is missing "+miss,
					"interface method signatures must match exactly (structural satisfaction, core design §4)")
				return
			}
		}
		// 接口 → 接口：**收窄**（目标方法集 ⊆ 源）在设计上是合法的（同一 data 指针 +
		// 目标见证表），但见证表按 (接口, 具体类) 配对、运行期不知道具体类 ⇒ 需要
		// "类 → 各接口见证表"的运行期映射（§十二 R11，随阶段 O/S 落地）。
		// 现在必须给出**可照做**的修复，而不是指向不存在的 `A(big)` 转换。
		if src, isSrc := vt.(*Interface); isSrc {
			if miss, subset := ifaceSubset(src, ifc); subset {
				c.errorAt(parse.ExprPos(value), "narrowing one interface to another is not supported yet",
					what+": "+src.String()+" → "+ifc.String(),
					"declare the variable with the narrower interface from the start, or keep the wider type and call only the methods it has (core design §12 R11)")
				return
			} else {
				c.errorAt(parse.ExprPos(value), "the source interface does not provide the target's methods",
					what+": "+src.String()+" is missing "+miss,
					"an interface value can only narrow to a subset of its method set (core design §4)")
				return
			}
		}
	}
	c.errorAt(parse.ExprPos(value), "type mismatch with no implicit conversion",
		what+": "+vt.String()+" written into "+target.String(),
		conversionFix(target, value))
}

// ifaceSubset 判定 src 的方法集是否**覆盖** ifc（收窄的前提）。返回 (缺失说明, 是否覆盖)。
func ifaceSubset(src, ifc *Interface) (string, bool) {
	for name, m := range ifc.Methods {
		sm, ok := src.Methods[name]
		if !ok {
			return name, false
		}
		if !sameSig(sm, m) {
			return name + " (signature differs)", false
		}
	}
	return "", true
}

// conversionFix 给出**可照做**的修复：只有标量/str/bytes 这类真的有转换语法的目标
// 才能建议 `T(x)`；接口/类/枚举没有转换调用（此前一律建议 `A(big)` ⇒ 指向不存在的函数）。
func conversionFix(target Type, value parse.Expr) string {
	switch t := target.(type) {
	case *Basic:
		if t.Name == "str" {
			return "convert explicitly first: str.fromI64 / str.fromF64 / str.fromBool, or give the type @derive(ToString) (red line 1)"
		}
		return "convert explicitly first: " + target.String() + "(" + exprText(value) + ") (red line 1)"
	case *Slice, *MapT, *SetT, *ArrayT, *Instance, *FuncT:
		if IsBytes(target) {
			return "convert explicitly first: bytes.fromStr(...) (core design §16 N7)"
		}
	}
	return "no conversion exists for " + target.String() + ": build the value explicitly (a composite literal, a constructor, or a field-by-field copy) (red line 1)"
}

// satisfies 判定类是否满足接口（结构化满足，§四）：接口的每个方法都要在类里有
// **签名完全一致**的实现。多出的方法不影响满足关系。
// 返回 (缺失/不匹配的方法说明, 是否满足)。
func satisfies(cl *Class, ifc *Interface) (string, bool) {
	if cl == nil || ifc == nil {
		return "interface", false
	}
	for name, want := range ifc.Methods {
		got, has := cl.Methods[name]
		if !has {
			return "method " + name, false
		}
		if !sameSig(got, want) {
			return "a method with a matching signature: " + name + " (interface -> " + resultsText(want.Results) +
				", class -> " + resultsText(got.Results) + ")", false
		}
	}
	return "", true
}

// sameSig 比较两个方法签名（形参类型与返回位逐一 identical）。
func sameSig(a, b *FuncSig) bool {
	if a == nil || b == nil || len(a.ParamTypes) != len(b.ParamTypes) || len(a.Results) != len(b.Results) {
		return false
	}
	for i := range a.ParamTypes {
		if !identical(a.ParamTypes[i], b.ParamTypes[i]) {
			return false
		}
	}
	for i := range a.Results {
		if !identical(a.Results[i], b.Results[i]) {
			return false
		}
	}
	return true
}

// InterfaceOf 暴露接口满足判定给 emit（见证表合成要用同一判据，禁两处各写一份）。
func InterfaceOf(t Type) (*Interface, bool) { return IsInterface(t) }

// Satisfies 暴露给 emit：类是否满足接口（emit 合成见证表前先确认）。
func Satisfies(cl *Class, ifc *Interface) bool {
	_, ok := satisfies(cl, ifc)
	return ok
}

// InterfaceSlots 返回接口方法的声明序槽位（§十：槽位号 = 声明序）。
// 顺序取自 Methods 的声明序——由 collectInterface 按声明序登记，故这里按
// 接口声明重建：调用方传 nil 时返回名字排序（确定性兜底）。
func InterfaceSlots(ifc *Interface) []string { return ifc.slotOrder() }

func literalText(info untyped) string {
	switch info.kind {
	case unInt:
		return itoa(int(info.ival))
	case unUint:
		return itoa(int(info.uval)) + " (u64 range)"
	case unFloat:
		return "float literal"
	case unStr:
		return "string literal"
	case unBool:
		if info.bval {
			return "true"
		}
		return "false"
	}
	return "literal"
}

// nilAssignable: nil 可赋/可比的目标集合（引用/容器/Err；数值零值也可赋但不
// 参与比较）。
func nilAssignable(t Type) bool {
	if isReference(t) {
		return true
	}
	if isErr(t) {
		return true
	}
	switch t.(type) {
	case *Class, *Interface, *Slice, *MapT, *SetT:
		return true
	}
	return false
}

// assignableToNilPosition: Err 位可写 nil（零值）。
func errSlotOK(t Type) bool { return isErr(t) }

// compositeTarget 描述一个引用存储：深度偏移 + 有效性。
type compositeTarget struct {
	off int
	ok  bool
}

func resultsOrNil(r Type) []Type {
	if r == nil {
		return nil
	}
	return []Type{r}
}
