package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// nil 非零流证明（核心设计 §九 教条：宁保守插检查，禁错误消除）。
//
// 已证非 nil：构造赋值后、p != nil 分支内、if p == nil { return … } 早退后、
// 被赋以已证非 nil 表达式之后。调用产物 / 字段 / 元素读 = 未证。字段不做流
// 跟踪（调用可变异），一律运行时守。可证必然为 nil 的解引用 = 编译错误
// （仅解引用位：方法调用、字段访问、下标解引用）。
// ---------------------------------------------------------------------------

type nilState struct {
	mustNil bool // 全路径必然 nil（解引用 = 编译错）
	nonNil  bool // 已证非 nil（解引用可消运行时检查）
}

// updateNilState: 赋值后的流状态转移。
func (c *Checker) updateNilState(name string, init parse.Expr) {
	st, ok := c.nilState[name]
	if !ok {
		st = &nilState{}
		c.nilState[name] = st
	}
	switch {
	case isNilExpr(init):
		*st = nilState{mustNil: true}
	case c.isProvenNonNil(init):
		*st = nilState{nonNil: true}
	default:
		*st = nilState{} // 调用产物/字段读 = 未证
	}
}

// checkDeref: 解引用位校验——可证必 nil = 编译错（红线 6）。
func (c *Checker) checkDeref(e parse.Expr, what string) {
	id, ok := e.(*parse.Ident)
	if !ok {
		return // 字段/元素读不做流跟踪（§九）
	}
	st, found := c.nilState[id.Name]
	if !found || !st.mustNil {
		return
	}
	c.errorAt(parse.ExprPos(e), "a dereference provably nil (compile error)",
		what+": variable "+id.Name+" is still nil on every path so far",
		"assign a constructed value first ("+id.Name+" = C{...}) or return early on nil; dereferencing without assigning always crashes (core design §9)")
}

// mergeNilStates: if 分支汇合——状态一致则保留，否则未证。
func mergeNilStates(a, b map[string]*nilState) map[string]*nilState {
	out := map[string]*nilState{}
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	for k := range seen {
		sa, ha := a[k]
		sb, hb := b[k]
		switch {
		case ha && hb && *sa == *sb:
			st := *sa
			out[k] = &st
		case ha && hb:
			out[k] = &nilState{} // 分支状态不同 = 未证
		default:
			// 单侧新声明（块作用域变量不外泄），忽略
		}
	}
	return out
}

// checkIfNilFlow: if p != nil { … } / if p == nil { return … } 的流分支。
// 返回 true 表示识别为 nil 判定（调用方负责分支作用域）。
func (c *Checker) checkIfNilFlow(cond parse.Expr) (name string, inverted bool, ok bool) {
	bin, isBin := cond.(*parse.Binary)
	if !isBin || (bin.Op != "==" && bin.Op != "!=") {
		return "", false, false
	}
	var idExpr *parse.Ident
	if n, isId := bin.Left.(*parse.Ident); isId {
		if _, isNil := bin.Right.(*parse.NilLit); isNil {
			idExpr = n
		}
	}
	if n, isId := bin.Right.(*parse.Ident); isId {
		if _, isNil := bin.Left.(*parse.NilLit); isNil {
			idExpr = n
		}
	}
	if idExpr == nil {
		return "", false, false
	}
	// 主体必须是引用类型变量
	if sym, found := c.scope.Lookup(idExpr.Name); found && sym.Kind == SymVar {
		if !isReference(sym.Type) && !isErr(sym.Type) {
			return "", false, false
		}
	}
	return idExpr.Name, bin.Op == "==", true
}
