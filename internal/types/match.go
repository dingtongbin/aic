package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// match（核心设计 §三）：仅作用于 enum、强制穷尽、无通配符、模式一层、
// match 不消费枚举（共享语义，原值仍可用）。
// ---------------------------------------------------------------------------

// resolvePattern 把一个模式对准主体枚举，返回命中的变体。
func (c *Checker) resolvePattern(p parse.MatchPattern, subject *Enum, targs []Type) (*Variant, bool) {
	name := p.Name
	if dot := lastIndexDot(name); dot >= 0 {
		pkgOrEnum, variant := name[:dot], name[dot+1:]
		if pkgOrEnum != subject.Name {
			c.errorAt(p.Pos, "the qualified pattern name does not match the subject enum", name,
				"the subject enum is "+subject.Name+"; write "+subject.Name+"."+variant)
			return nil, false
		}
		name = variant
	}
	idx, ok := subject.variantI[name]
	if !ok {
		c.errorAt(p.Pos, "the enum has no such variant", subject.Name+"."+name,
			"see enum "+subject.Name+" declaration")
		return nil, false
	}
	va := &subject.Variants[idx]
	if va.Payload != nil && p.Binding == "" {
		c.errorAt(p.Pos, "a pattern for a payload variant needs a binding name", subject.Name+"."+name,
			"write "+name+"(binding) to introduce a name; to discard write "+name+"(_)")
		return nil, false
	}
	if va.Payload == nil && p.Binding != "" {
		c.errorAt(p.Pos, "a pattern for a payload-free variant takes no binding", subject.Name+"."+name,
			"write "+name+" => …")
		return nil, false
	}
	return va, true
}

// UnqualifiedVariant 去掉变体名里的 `Enum.` 限定前缀（`Shape.Circle` → `Circle`）。
// **唯一实现**：检查器（resolvePattern）与 AIR 降级（match 臂的负载绑定）必须用同一套
// 规则 —— AIR 侧曾经直接用原始 `p.Name` 去查变体表，带限定的模式查不到就**静默跳过绑定**，
// 于是 arm 里引用绑定的名字成了"先用后定"（verifier V2.4 报编译器缺陷）。
func UnqualifiedVariant(name string) string {
	if dot := lastIndexDot(name); dot >= 0 {
		return name[dot+1:]
	}
	return name
}

// VariantByName 把变体名（可带 `Enum.` 限定）对准枚举，返回 (下标, 变体, 是否找到)。
func VariantByName(en *Enum, name string) (int, Variant, bool) {
	if en == nil {
		return 0, Variant{}, false
	}
	idx, ok := en.variantI[UnqualifiedVariant(name)]
	if !ok {
		return 0, Variant{}, false
	}
	return idx, en.Variants[idx], true
}

func lastIndexDot(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

// checkArms 校验全部模式并返回覆盖的变体索引集合（穷尽性判定用）。
func (c *Checker) checkArms(arms []parse.MatchArm, subject *Enum, targs []Type, at parse.Pos, inExpr bool) map[int]bool {
	covered := map[int]bool{}
	for _, arm := range arms {
		if inExpr && arm.Block != nil {
			// parse 层已拒绝表达式形的块分支；防御性跳过
			continue
		}
		for _, pat := range arm.Patterns {
			va, ok := c.resolvePattern(pat, subject, targs)
			if !ok {
				continue
			}
			if covered[indexOfVariant(subject, va.Name)] {
				c.errorAt(pat.Pos, "the same variant is covered twice", subject.Name+"."+va.Name,
					"one arm per variant")
				continue
			}
			covered[indexOfVariant(subject, va.Name)] = true
			// 负载绑定 = 分支作用域新声明（泛型枚举的负载经实参代换）
			if va.Payload != nil && pat.Binding != "" && pat.Binding != "_" {
				bindTy := va.Payload
				if len(targs) > 0 {
					bindTy = subst(bindTy, subject.TypeParams, targs)
				}
				c.scope.Declare(&Symbol{Kind: SymVar, Name: pat.Binding, Type: bindTy, DeclOffset: c.regionOff})
			}

		}
	}
	// 强制穷尽：无通配符（红线 4）
	for i := range subject.Variants {
		if !covered[i] {
			c.errorAt(at, "match is not exhaustive: missing variant", subject.Name+"."+subject.Variants[i].Name,
				"add "+subject.Variants[i].Name+" arms (exhaustiveness is the point; no wildcards)")
			break
		}
	}
	return covered
}

func indexOfVariant(en *Enum, name string) int { return en.variantI[name] }

func (c *Checker) matchSubject(e parse.Expr) (*Enum, []Type) {
	st, _, _ := c.checkExprFull(e, nil)
	if st == nil {
		return nil, nil
	}
	if inst, ok := st.(*Instance); ok {
		if en, isEnum := inst.Base.(*Enum); isEnum {
			return en, inst.Args
		}
	}
	en, ok := st.(*Enum)
	if !ok {
		c.errorAt(parse.ExprPos(e), "match works on enums only", typeOrUn(st, untyped{}),
			"use if for numeric and string branches; the subject of match must be an enum value (core design §3)")
		return nil, nil
	}
	return en, nil
}

func (c *Checker) checkMatchExpr(v *parse.MatchExpr, expect Type) (Type, bound, untyped) {
	en, targs := c.matchSubject(v.Subject)
	if en == nil {
		return nil, nilBound, untyped{}
	}
	armScope := NewScope(c.scope, c.regionOff)
	saved := c.scope
	c.scope = armScope
	covered := c.checkArms(v.Arms, en, targs, v.Pos, true)
	_ = covered

	// 全分支表达式类型一致（并与期望一致）。
	// **必须在 armScope 仍生效时检查分支值**：负载绑定（`Some(x) => x`）由 checkArms
	// 声明进 armScope，此前这里先 `c.scope = saved` 再检查 ⇒ 分支体里的 x 报"未声明的名字 x"
	// （表达式位 match 的负载绑定根本用不了；语句位 match 正常，因为它在自己的块里）。
	var unified Type
	for _, arm := range v.Arms {
		if arm.Value == nil {
			continue
		}
		at, _, ainfo := c.checkExprFull(arm.Value, expect)
		if at == nil {
			// 未定型字面量分支（`A => "lit"`）：按字面量默认类型定型 —— 否则整条
			// match 表达式静默无类型（调用方 `var x = match …` 跟着静默消失）。
			at = c.literalDefault(ainfo, parse.ExprPos(arm.Value))
		}
		if at == nil {
			continue
		}
		if unified == nil {
			unified = at
			continue
		}
		if !identical(at, unified) {
			c.errorAt(parse.ExprPos(arm.Value), "match expression arms have inconsistent types",
				"the arm is "+at.String()+", previously "+unified.String(),
				"every arm must produce the same type; convert explicitly where needed")
		}
	}
	c.scope = saved
	if unified == nil {
		return nil, nilBound, untyped{}
	}
	if expect != nil && !identical(unified, expect) {
		c.errorAt(v.Pos, "the match expression type does not match the context",
			"match produces "+unified.String()+", expected "+expect.String(),
			"adjust the arms or convert explicitly")
	}
	return unified, bound{off: c.regionOff}, untyped{}
}
