package types

import (
	"strings"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// 控制流语句：if / for / return / defer / match 语句（核心设计 §三/§五/§六）。
// ---------------------------------------------------------------------------

func (c *Checker) requireCond(e parse.Expr, what string) {
	ty, _, info := c.checkExprFull(e, TBool)
	if ty == nil {
		return
	}
	if !isBool(ty) && info.kind != unBool {
		c.errorAt(parse.ExprPos(e), "the condition must be bool", what+": the condition is "+typeOrUn(ty, info),
			"write an explicit comparison (x != 0 / b == true); there is no truthiness coercion (core design §3)")
	}
}

func (c *Checker) checkIf(v *parse.IfStmt) {
	c.requireCond(v.Cond, "if")
	name, inverted, isNilTest := c.checkIfNilFlow(v.Cond)
	savedNil := c.nilState

	// then 分支：p != nil 分支内已证非 nil；p == nil 分支内必 nil
	thenNil := cloneNilStates(savedNil)
	c.nilState = thenNil
	if isNilTest {
		if st := c.nilState[name]; st != nil {
			if inverted {
				*st = nilState{nonNil: true}
			} else {
				*st = nilState{mustNil: true}
			}
		}
	}
	c.checkBlock(v.Then)
	thenNil = c.nilState

	// else 分支：条件取反
	elseNil := cloneNilStates(savedNil)
	c.nilState = elseNil
	if v.Else != nil {
		if isNilTest {
			if st := c.nilState[name]; st != nil {
				if inverted {
					*st = nilState{mustNil: true}
				} else {
					*st = nilState{nonNil: true}
				}
			}
		}
		c.checkBlock(v.Else)
		elseNil = c.nilState
	}

	if isNilTest && thenTerminates(v.Then) {
		// if p == nil { return … } 早退：出口后沿用 else 侧状态（§九）
		c.nilState = elseNil
		return
	}
	c.nilState = mergeNilStates(thenNil, elseNil)
	if v.Else == nil {
		// 无 else：条件为假路径未被走过，恢复进入前状态再合并 then 侧
		c.nilState = mergeNilStates(thenNil, cloneNilStates(savedNil))
	}
}

func cloneNilStates(m map[string]*nilState) map[string]*nilState {
	out := make(map[string]*nilState, len(m))
	for k, v := range m {
		cp := *v
		out[k] = &cp
	}
	return out
}

// thenTerminates: 块是否必然终止（return；粗粒度 = 语句级扫描，覆盖早退惯用法）。
func thenTerminates(b *parse.Block) bool {
	for _, s := range b.Stmts {
		switch v := s.(type) {
		case *parse.ReturnStmt:
			return true
		case *parse.ExprStmt:
			if ce, ok := v.X.(*parse.CheckExpr); ok {
				_ = ce
				return true // check 传播即终止
			}
		}
	}
	return false
}

// RangeElemType 是可迭代类型的元素类型（str → u8 字节）。
func RangeElemType(iter Type) Type {
	switch t := iter.(type) {
	case *Slice:
		return t.Elem
	case *MapT:
		return t.Value
	case *SetT:
		return t.Elem
	case *Basic:
		if t.Name == "str" {
			return TU8
		}
	}
	return nil
}

// RangeBindTypes 是 for-in 绑定名类型的**唯一实现**（检查器与 emit 的名字表共用，
// 禁第二份推导——两份推导必然漂移，693 的实际故障就是 emit 侧一律记 usize）。
//
//	1 个名字：map → 键；str → 单字节视图 str；其余 → 元素
//	2 个名字：第一位 usize（下标/键），第二位元素（map → 值；str → u8 字节）
//	区间形态：第一位 usize 计数器
func RangeBindTypes(iter Type, names int, isInterval bool) (Type, Type) {
	if isInterval {
		return TUsize, nil
	}
	if names <= 1 {
		switch t := iter.(type) {
		case *MapT:
			return t.Key, nil
		case *Basic:
			if t.Name == "str" {
				return TStr, nil
			}
		}
		return RangeElemType(iter), nil
	}
	return TUsize, RangeElemType(iter)
}

func (c *Checker) checkFor(v *parse.ForStmt) {
	if v.IsRange {
		// for x in xs / for i, x in xs / for i in a..b
		var elemTy Type
		var iterTy Type
		if v.RangeEnd != nil {
			c.checkIndexInt(v.RangeX, "range start")
			c.checkIndexInt(v.RangeEnd, "range end")
		} else {
			xt, _, _ := c.checkExprFull(v.RangeX, nil)
			iterTy = xt
			switch t := xt.(type) {
			case *Slice:
				elemTy = t.Elem
			case *MapT:
				elemTy = t.Value // for k, v in m
			case *SetT:
				elemTy = t.Elem
			case *Basic:
				if t.Name == "str" {
					elemTy = TU8 // for i, b in s → 字节
				}
			case nil:
			default:
				c.errorAt(parse.ExprPos(v.RangeX), "this type is not iterable", typeOrUn(xt, untyped{}),
					"iterable: list / map / set / str (core design §2.5)")
			}
		}
		// 绑定名：1 个 = 值或计数；2 个 = (索引/键, 值)
		inner := NewScope(c.scope, c.regionOff)
		inner.LoopDepth = c.scope.LoopScope() + 1
		switch len(v.Names) {
		case 1:
			n := v.Names[0]
			bindType, _ := RangeBindTypes(iterTy, 1, v.RangeEnd != nil)
			if v.RangeEnd == nil && elemTy == nil {
				bindType = nil
			}
			if !n.Blank {
				inner.Declare(&Symbol{Kind: SymVar, Name: n.Name, Type: bindType, DeclOffset: c.regionOff})
			}
		case 2:
			iN, xN := v.Names[0], v.Names[1]
			first, second := RangeBindTypes(iterTy, 2, false)
			if !iN.Blank {
				inner.Declare(&Symbol{Kind: SymVar, Name: iN.Name, Type: first, DeclOffset: c.regionOff})
			}
			if !xN.Blank && second != nil {
				inner.Declare(&Symbol{Kind: SymVar, Name: xN.Name, Type: second, DeclOffset: c.regionOff})
			}
		default:
			c.errorAt(v.Pos, "for-in takes at most two loop variables", "for a, b, c in …",
				"write for x in xs or for i, x in xs")
		}
		saved, savedNil := c.scope, c.nilState
		c.scope, c.nilState = inner, map[string]*nilState{}
		c.checkBlock(v.Body)
		c.scope, c.nilState = saved, savedNil
		return
	}
	// C 形 / cond 形
	if v.Init != nil {
		c.checkStmt(v.Init)
	}
	if v.Cond != nil {
		c.requireCond(v.Cond, "for")
	}
	if v.Post != nil {
		c.checkStmt(v.Post)
	}
	inner := NewScope(c.scope, c.regionOff)
	inner.LoopDepth = c.scope.LoopScope() + 1
	saved, savedNil := c.scope, c.nilState
	c.scope, c.nilState = inner, map[string]*nilState{}
	c.checkBlock(v.Body)
	c.scope, c.nilState = saved, savedNil
}

// checkReturn: 返回摘要必须 = 0（§五 R3）——精确 > 0 = 编译错；非精确 > 0 =
// 返回点守卫。结果类型逐位校验。
func (c *Checker) checkReturn(v *parse.ReturnStmt) {
	want := c.fn.Results
	if len(v.Results) > len(want) {
		c.errorAt(v.Pos, "wrong number of results",
			"got "+itoa(len(v.Results))+", the signature returns "+itoa(len(want)),
			"match the function signature: add or delete")
		return
	}
	if len(v.Results) < len(want) {
		c.errorAt(v.Pos, "wrong number of results",
			"got "+itoa(len(v.Results))+", the signature returns "+itoa(len(want)),
			"fill missing slots with the zero value and nil (the Err slot takes nil)")
		return
	}
	for i, r := range v.Results {
		rt, rb, rinfo := c.checkExprFull(r, want[i])
		c.requireAssignableAt(r, rt, rinfo, want[i], "result "+itoa(i+1))
		if isReference(rt) || containsReference(rt) {
			c.checkReturnBound(r, rb)
		}
	}
}

// checkReturnBound: 返回点规则（R0c-② F2）。
func (c *Checker) checkReturnBound(e parse.Expr, rb bound) {
	if rb.off <= 0 {
		return
	}
	if rb.exact {
		c.errorAt(parse.ExprPos(e), "returning an object from a deeper region (a creation expression returned directly)",
			"object created at depth "+itoa(rb.off)+", the function entry is 0",
			"create it outside the block before returning, or add live to the creation (core design §5 R5)")
		return
	}
	c.guards[e] = GuardSpec{Kind: GuardReturn}
}

func (c *Checker) checkDefer(v *parse.DeferStmt) {
	if v.Block != nil {
		c.checkBlock(v.Block)
		return
	}
	call, ok := v.Call.(*parse.Call)
	if !ok {
		c.errorAt(v.Pos, "defer must be followed by a call or a block", "defer …",
			"write defer f(...) or defer { ... } (core design §3)")
		return
	}
	ty, _, _ := c.checkExprFull(v.Call, nil)
	_ = ty
	// §15.7 规则③：defer 的调用结果没人接 ⇒ 有 Err 位的调用不能被 defer。
	c.rejectUnconsumedErr(v.Call, "defer", v.Pos)
	// defer 实参：界 ≤ 入口（精确 > 0 = 编译错；非精确 > 0 = 注册点守卫，R5）
	for _, a := range call.Args {
		at, ab, _ := c.checkExprFull(a, nil)
		if at == nil {
			continue
		}
		if isValueType(at) && !containsReference(at) {
			continue // 值类型无寿命问题
		}
		if ab.off <= 0 {
			continue
		}
		if ab.exact {
			c.errorAt(parse.ExprPos(a), "a block-local object cannot be a defer argument (it would outlive the block)",
				"object created at depth "+itoa(ab.off),
				"create the object outside the block, or pass a value type (core design §5 R5)")
			continue
		}
		// 守卫的 limit = **函数入口深度**（§五 R5：defer/errdefer 实参的界 ≤ 入口）
		// ⇒ Delta = 当前偏移（发射侧 limit = aic_depth − Delta）。
		c.guards[a] = GuardSpec{Kind: GuardDefer, Delta: c.regionOff}
	}
}

// ---------------------------------------------------------------------------
// N8 `errdefer`（核心设计 §16 N8）：**仅在函数因 Err 返回时**执行的清理。
// 语法/语义与 defer 同源（LIFO、实参在注册点求值），差别只有"何时跑"。
// ---------------------------------------------------------------------------

func (c *Checker) checkErrDefer(v *parse.ErrDeferStmt) {
	// 没有 Err 位 = 这个清理永远不会执行 = 死代码：必须报错，不能静默忽略。
	if !c.fnHasErr() {
		c.errorAt(v.Pos, "errdefer needs a function with an Err result slot", "this function has no Err result",
			"add -> Err to the signature, or use defer if the cleanup must run on every exit (core design §16 N8)")
		return
	}
	if v.Block != nil {
		c.checkBlock(v.Block)
		return
	}
	call, ok := v.Call.(*parse.Call)
	if !ok {
		c.errorAt(v.Pos, "errdefer must be followed by a call or a block", "errdefer …",
			"write errdefer f(...) or errdefer { ... } (core design §16 N8)")
		return
	}
	c.checkExprFull(v.Call, nil)
	// 实参边界与 defer 完全一致（注册点求值 + 不得比块活得更久）。
	for _, a := range call.Args {
		at, ab, _ := c.checkExprFull(a, nil)
		if at == nil {
			continue
		}
		if isValueType(at) && !containsReference(at) {
			continue
		}
		if ab.off <= 0 {
			continue
		}
		if ab.exact {
			c.errorAt(parse.ExprPos(a), "a block-local object cannot be an errdefer argument (it would outlive the block)",
				"object created at depth "+itoa(ab.off),
				"create the object outside the block, or pass a value type (core design §5 R5)")
			continue
		}
		// 守卫的 limit = **函数入口深度**（§五 R5：defer/errdefer 实参的界 ≤ 入口）
		// ⇒ Delta = 当前偏移（发射侧 limit = aic_depth − Delta）。
		c.guards[a] = GuardSpec{Kind: GuardDefer, Delta: c.regionOff}
	}
}

// isNegativeLiteral 报告一个表达式是不是**字面量负数**（`-1` 是 Unary 包着 IntLit，
// 不是 IntLit）—— 用于容量/长度这类必须非负的编译期实参。
func isNegativeLiteral(e parse.Expr) bool {
	switch v := e.(type) {
	case *parse.IntLit:
		return strings.HasPrefix(strings.TrimSpace(v.Text), "-")
	case *parse.FloatLit:
		return strings.HasPrefix(strings.TrimSpace(v.Text), "-")
	case *parse.Unary:
		if v.Op != "-" {
			return false
		}
		switch v.X.(type) {
		case *parse.IntLit, *parse.FloatLit:
			return true
		}
	}
	return false
}

func (c *Checker) checkMatchStmt(v *parse.MatchStmt) {
	en, targs := c.matchSubject(v.Subject)
	armScope := NewScope(c.scope, c.regionOff)
	saved := c.scope
	c.scope = armScope
	c.checkArms(v.Arms, en, targs, v.Pos, false)
	for _, arm := range v.Arms {
		if arm.Block != nil {
			c.checkBlock(arm.Block)
		}
		if arm.Value != nil {
			c.checkStmtOnly(arm.Value)
		}
	}
	c.scope = saved
}
