package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// 语句检查：作用域/遮蔽、区域偏移与存储点守卫判定（⓪–⑥）、返回摘要、
// defer、绑定规则（核心设计 §三/§五/§六）。
// ---------------------------------------------------------------------------

// checkBlock 在新子作用域内检查一个块（内层块可遮蔽外层名；变量声明偏移 =
// 当前 region 偏移）。
func (c *Checker) checkBlock(b *parse.Block) {
	saved := c.scope
	c.scope = NewScope(saved, c.regionOff)
	for _, s := range b.Stmts {
		c.checkStmt(s)
	}
	c.scope = saved
}

func (c *Checker) checkStmt(s parse.Stmt) {
	switch v := s.(type) {
	case nil:
		return
	case *parse.Block:
		c.checkBlock(v)
	case *parse.VarDecl:
		c.checkVarDecl(v)
	case *parse.ExprStmt:
		c.checkExprStmt(v)
	case *parse.IfStmt:
		c.checkIf(v)
	case *parse.ForStmt:
		c.checkFor(v)
	case *parse.ReturnStmt:
		c.fnReturnCount++
		c.checkReturn(v)
	case *parse.BranchStmt:
		if c.scope.LoopScope() == 0 {
			c.errorAt(v.Pos, v.Keyword+" may only appear inside a loop", v.Keyword,
				"there are no loop labels (core design §3): use a flag variable to exit nested loops")
		}
	case *parse.MatchStmt:
		c.checkMatchStmt(v)
	case *parse.DeferStmt:
		c.checkDefer(v)
	case *parse.ErrDeferStmt:
		c.checkErrDefer(v)
	case *parse.RegionStmt:
		c.regionOff++
		c.checkBlock(v.Body)
		c.regionOff--
		// region 块退出后：块内声明的变量全部失效（作用域即块）——块自身
		// 的 Scope 已在 checkRegionBlock 内管理，这里无需清理。
	case *parse.SpawnStmt:
		c.checkStmtOnly(v.Call)
		// §15.7 规则③：spawn 出去的调用**没有人接它的 Err**（结果不进任何绑定）
		// ⇒ 有 Err 位的调用不能被 spawn（要么先处理错误，要么把错误留在任务内处理）。
		c.rejectUnconsumedErr(v.Call, "spawn", v.Pos)
	case *parse.ScopeStmt:
		c.checkScopeTimeout(v)
		c.checkBlock(v.Body)
	case *parse.SelectStmt:
		c.checkSelect(v)
	}
}

// checkRegionBlock: region 块体（偏移在 checkStmt 中已处理）。
func (c *Checker) checkRegionBlock(b *parse.Block) { c.checkBlock(b) }

// --- 变量声明（含多返回绑定规则，§六） --------------------------------------

func (c *Checker) checkVarDecl(v *parse.VarDecl) {
	var declared Type
	if v.Type != nil {
		declared = c.resolveType(v.Type)
	}
	// 多目标 = 多返回绑定：var a, err = f()
	if len(v.Targets) > 1 {
		c.checkMultiBind(v, declared)
		return
	}
	tgt := v.Targets[0]
	if tgt.Blank {
		// parse 层已拒绝单 _ 声明；防御
		return
	}
	var vt Type
	var vb bound
	var vinfo untyped
	if v.Init != nil {
		vt, vb, vinfo = c.checkExprFull(v.Init, declared)
		if vt == nil && vinfo.kind == unNone {
			// 初值检查失败：**仍要登记该名字**（用声明类型兜底），否则后续每一次使用
			// 都会再刷一条"未声明的名字"，把首因埋在级联噪音里（诊断审计实测：
			// 8 个语料各有 1–2 条这种幻影诊断）。无声明类型时按 i32 占位 ——
			// 只影响"后续引用不再重复报错"，真正的首因诊断照旧。
			if len(c.errors) > 0 {
				poison := declared
				if poison == nil {
					poison = TI32
				}
				c.declareVar(tgt, poison, v.Init)
			}
			return
		}
		if _, isMulti := vt.(*MultiType); isMulti {
			c.errorAt(v.Pos, "multiple results must be bound slot by slot", "var "+tgt.Name+" = "+exprText(v.Init),
				"write var a, err = f() or var a = check f(); discard explicitly with _ (red line 2)")
			return
		}
	}
	if declared == nil {
		// 推断：字面量取最窄可容纳（§一）
		declared = c.inferFromInit(vt, vinfo, tgt.Pos)
		if declared == nil {
			return
		}
	} else if v.Init != nil {
		c.requireAssignableAt(v.Init, vt, vinfo, declared, "declaration "+tgt.Name)
	}
	// 推断出的类型也记进类型表：emit 需要按声明类型发射变量与调用返回形状
	// （未定型字面量在表里本就是 nil，记录后不影响字面量的上下文定宽语义）。
	//
	// **只在初值自身没有类型时补记**：初值已有类型（如 `var s Shape = d` 里的 d
	// 是具体类 Dog）时必须原样保留 —— 覆盖成声明类型会让 emit 看不出「具体类装进
	// 接口」这件事，装箱（§四）就发不出去，C 层报 invalid initializer。
	if v.Init != nil && c.types[v.Init] == nil {
		c.types[v.Init] = declared
	}
	// @live 语义（R4）：仅对构造表达式有意义；创建发生在外一层
	if tgt.Live {
		if isStr(declared) || (isValueType(declared) && !containsReference(declared)) {
			c.errorAt(tgt.Pos, "@live is meaningless for value types and str", "@live "+tgt.Name,
				"a value type has no region header; drop @live (core design §5 R4)")
		} else if vb.exact && vb.off == c.regionOff {
			vb.off-- // 创建下沉到外一层
		} else {
			c.errorAt(tgt.Pos, "@live only makes sense on a creation expression", "@live "+tgt.Name+" = …",
				"write Class{...} or Class(...) on the right (inlining the creation at the declaration keeps it in the outer region)")
		}
	}
	c.declareVar(tgt, declared, v.Init)
}

// declareVar 登记变量。init == nil 表示「无初值表达式」——单目标无初值
// （var p C）必然 nil，但多返回绑定（var a, err = f()）的接收值来自调用，
// 属「未证」而非「必 nil」（§九），故由调用方在声明后调用 setCallResultNilState。
func (c *Checker) declareVar(tgt parse.VarTarget, ty Type, init parse.Expr) {
	off := c.regionOff
	if tgt.Live {
		off-- // @live = 外一层创建（R4）；块外 @live 由 parse 层约束 + 这里校验
		if off < 0 {
			c.errorAt(tgt.Pos, "@live is meaningless at the outermost function level (already the task region)", "@live "+tgt.Name,
				"drop @live: an object in the task region already lives as long as the task (core design §5 R4)")
			off = 0
		}
	}
	sym := &Symbol{Kind: SymVar, Name: tgt.Name, Type: ty, DeclOffset: off, Live: tgt.Live}
	if !c.scope.Declare(sym) {
		c.errorAt(tgt.Pos, "duplicate declaration in the same scope", tgt.Name,
			"an inner block may shadow an outer one; a duplicate declaration in the same block is a compile error (core design §3)")
		return
	}
	c.NoteLocal(c.fnKey, tgt.Name)
	// nil 流证明初始化（§九）
	st := &nilState{}
	if init != nil {
		if isNilExpr(init) {
			st.mustNil = true
		} else if c.isProvenNonNil(init) {
			st.nonNil = true
		}
	} else {
		st.mustNil = true // var p C 无初值 = 必然 nil（解引用即编译错）
	}
	c.nilState[tgt.Name] = st
}

// checkMultiBind: var a, err = f()——名字必须全新、被调末位 Err、数量一致。
func (c *Checker) checkMultiBind(v *parse.VarDecl, declared Type) {
	call, ok := v.Init.(*parse.Call)
	if !ok {
		c.errorAt(v.Pos, "a multi-target declaration can only bind a call result", "var "+targetList(v.Targets)+" = …",
			"write a call that returns several values on the right (core design §6)")
		return
	}
	ct, _, _ := c.checkExprFull(call, declared)
	mt, isMulti := ct.(*MultiType)
	if !isMulti {
		c.errorAt(v.Pos, "the callee does not return multiple values", exprText(call),
			"a multi-target binding needs a callee returning 2 or more values")
		return
	}
	blankCount := 0
	for _, tgt := range v.Targets {
		if tgt.Blank {
			blankCount++
			continue
		}
		if _, exists := c.scope.LookupLocal(tgt.Name); exists {
			c.errorAt(tgt.Pos, "a var binding must use a fresh name", tgt.Name,
				"use the assignment form "+targetList(v.Targets)+" = f() (the name must already exist) or pick a new name")
			continue
		}
	}
	if len(v.Targets) != len(mt.Elems) {
		c.errorAt(v.Pos, "the number of binding targets does not match the number of results",
			itoa(len(v.Targets))+" targets but the callee returns "+itoa(len(mt.Elems)),
			"add the missing targets, or discard with _")
		return
	}
	for i, tgt := range v.Targets {
		if tgt.Blank {
			// §15.7 规则③：**有 Err 位的调用结果必须被消费** —— 末位（Err）弃位 = 吞错。
			if i == len(v.Targets)-1 && isErr(mt.Elems[i]) {
				c.errorAt(tgt.Pos, "the Err result must be consumed", "_ = "+exprText(call),
					"bind it (var v, err = f()) and test it, or propagate with f()? / f()! (core design §15.7 rule 3)")
			}
			continue
		}
		c.declareVar(tgt, mt.Elems[i], nil)
		// 接收位 = 调用产物 → 未证（§九：不因「无初值字面量」误判为必 nil）。
		c.nilState[tgt.Name] = &nilState{}
	}
	_ = blankCount
}

func isNilExpr(e parse.Expr) bool {
	_, ok := e.(*parse.NilLit)
	return ok
}

// isProvenNonNil: 构造表达式 = 已证非 nil（§九 教条的第一条）。
func (c *Checker) isProvenNonNil(e parse.Expr) bool {
	switch v := e.(type) {
	case *parse.CompositeLit:
		return true
	case *parse.Call:
		if id, ok := v.Fn.(*parse.Ident); ok {
			if _, isCl := c.classes[id.Name]; isCl {
				return true // 构造函数
			}
		}
		return false
	case *parse.CheckExpr:
		return false
	}
	return false
}

// --- 赋值 / 表达式语句 ------------------------------------------------------

func (c *Checker) checkExprStmt(v *parse.ExprStmt) {
	switch x := v.X.(type) {
	case *parse.Assign:
		c.checkAssign(x)
	case *parse.MultiAssign:
		c.checkMultiAssign(x)
	case *parse.CheckExpr:
		// 独立 check 语句（§六）：传播即可
		c.checkCheckExpr(x, nil)
	default:
		// 裸调用语句：吞 Err 检查（红线 2 + §15.7 规则③：有 Err 位的调用结果必须被消费）
		ty, _, _ := c.checkExprFull(v.X, nil)
		if mt, ok := ty.(*MultiType); ok && len(mt.Elems) > 0 && isErr(mt.Elems[len(mt.Elems)-1]) {
			c.errorAt(v.Pos, "a call returning Err must bind every result", exprText(v.X),
				"write var r, err = f() or var r = check f(); to discard write var _, err = f() (red line 2)")
			return
		}
		// **只返回 Err** 的调用（类型表里是 Err 本身，不是多返回）：同样是吞错。
		if isErr(ty) {
			c.errorAt(v.Pos, "a call returning Err must be consumed", exprText(v.X),
				"write var err = f() and test it, or propagate with f()? / f()! (core design §15.7 rule 3)")
		}
	}
}

// checkAssign: x = e / 复合赋值（数值 only）。目标 = 变量/字段/下标。
func (c *Checker) checkAssign(v *parse.Assign) {
	rhs, rb, rinfo := c.checkExprFull(v.Right, nil)
	switch lhs := v.LHS.(type) {
	case *parse.Ident:
		sym, ok := c.scope.Lookup(lhs.Name)
		if !ok {
			if lhs.Name == "_" {
				c.errorAt(v.Pos, "`_` cannot be a standalone assignment target", "_ = …",
					"discard a multi-return slot with a, _ = f(); to drop a single result call it as a statement: f()")
				return
			}
			c.errorAt(v.Pos, "the assignment target is undeclared", lhs.Name,
				"declare with var first; declaration and assignment are two different things")
			return
		}
		if v.Op != "=" {
			c.checkCompound(v, rhs, sym.Type, rinfo, lhs.Name)
			return
		}
		c.requireAssignableAt(v.Right, rhs, rinfo, sym.Type, "assigned to "+lhs.Name)
		c.markVarStore(v.Right, sym, lhs.Name)
		c.updateNilState(lhs.Name, v.Right)
	case *parse.Field:
		c.checkFieldStore(v, lhs, rhs, rb, rinfo)
	case *parse.Index:
		c.checkIndexStore(v, lhs, rhs, rb, rinfo)
	default:
		c.errorAt(v.Pos, "the left side of an assignment must be a variable, a field, or an index", "got "+exprText(v.LHS),
			"a constant or expression cannot be assigned to; introduce a mutable name")
	}
}

// markVarStore: 变量槽赋值的守卫判定（存储点 ⓪，R0c-② F1）。
func (c *Checker) markVarStore(value parse.Expr, sym *Symbol, name string) {
	c.markVarStoreAt(value, value, sym, name)
}

// markVarStoreAt 同 markVarStore，但守卫标记挂在 markNode 上（多返回解构每个目标
// 一个键，互不覆盖：c.guards 是"节点 → 一个 spec"）。
func (c *Checker) markVarStoreAt(value parse.Expr, markNode parse.Expr, sym *Symbol, name string) {
	vt, vb, _ := c.checkExprFull(value, nil)
	if vt == nil {
		return
	}
	if isValueType(vt) && !containsReference(vt) {
		return
	}
	// 形参派生值写**本函数局部变量**恒安全（参数定理，§五 R3）；但目标更浅时仍要判。
	if vb.off <= sym.DeclOffset {
		return
	}
	if vb.exact {
		// N1 闭包捕获的引用更具体：报"捕获的引用活得没闭包久"，并指向捕获点。
		if ft, isFn := vt.(*FuncT); isFn && ft.HasRefCapture() {
			c.errorAt(parse.ExprPos(value), "closure captures a reference that does not outlive the closure",
				"the closure is created at depth "+itoa(vb.off)+", variable "+name+" lives at depth "+itoa(sym.DeclOffset),
				"create the closure in the variable's region, capture by value instead (copy the fields you need), or add @live to the declaration (core design §16 N1)")
			return
		}
		c.errorAt(parse.ExprPos(value), "a block-local object escapes: assigned to a name outside the block",
			"object created at depth "+itoa(vb.off)+", variable "+name+" declared at depth "+itoa(sym.DeclOffset),
			"move the creation into the variable's region, or add @live to the declaration (core design §5 R5)")
		return
	}
	kind := GuardStore
	if containsReference(vt) {
		kind = GuardComposite
	}
	c.guards[markNode] = GuardSpec{Kind: kind, Delta: c.regionOff - sym.DeclOffset}
}

func (c *Checker) checkFieldStore(v *parse.Assign, lhs *parse.Field, rhs Type, rb bound, rinfo untyped) {
	if _, isThis := lhs.X.(*parse.ThisExpr); !isThis {
		c.checkStmtOnly(lhs.X) // 接收者一致性（非 this 前缀也要先检查）
	}
	var owner *Class
	var ft Type
	if thisExpr, isThis := lhs.X.(*parse.ThisExpr); isThis {
		_ = thisExpr
		if c.thisClass == nil {
			c.errorAt(v.Pos, "this may only appear inside a method body", "this."+lhs.Name, "move the code into a method")
			return
		}
		owner = c.thisClass
	} else {
		xt, _, _ := c.checkExprFull(lhs.X, nil)
		if cl, ok := xt.(*Class); ok {
			owner = cl
		} else if xt != nil {
			c.errorAt(v.Pos, "this type has no assignable field", typeOrUn(xt, untyped{}), "only a class has fields")
			return
		}
	}
	if owner == nil {
		return
	}
	idx, ok := owner.fieldIdx[lhs.Name]
	if !ok {
		c.errorAt(v.Pos, "the class has no such field", owner.String()+"."+lhs.Name,
			"see the class declaration for its fields")
		return
	}
	ft = owner.Fields[idx].Type
	if v.Op != "=" {
		c.checkCompound(v, rhs, ft, rinfo, "this."+lhs.Name)
		return
	}
	c.requireAssignableAt(v.Right, rhs, rinfo, ft, "field "+lhs.Name)
	// 存储点 ①：目标深度 = 宿主对象深度；@packed 赋值即拷贝（值语义）
	ownerBound := bound{off: c.regionOff}
	if thisExpr, isThis := lhs.X.(*parse.ThisExpr); isThis {
		_ = thisExpr
		ownerBound = bound{off: 0} // this 的槽位在入口层
	} else if id, isId := lhs.X.(*parse.Ident); isId {
		if sym, found := c.scope.Lookup(id.Name); found {
			// param 必须一起传：宿主是形参时它与值之间没有深度顺序（见 markStoreGuard）。
			ownerBound = bound{off: sym.DeclOffset, param: sym.Param}
		}
	} else {
		// 间接宿主（链式 a.b.c = …）：以宿主表达式的界为目标
		hostTy, hostB, _ := c.checkExprFull(lhs.X, nil)
		_ = hostTy
		ownerBound = hostB
	}
	c.markStoreGuard(v.Right, ownerBound, "field")
	_ = ft
}

func (c *Checker) checkIndexStore(v *parse.Assign, lhs *parse.Index, rhs Type, rb bound, rinfo untyped) {
	ct, cb, _ := c.checkExprFull(lhs.X, nil)
	if ct == nil {
		return
	}
	c.checkIndexInt(lhs.Index, "index")
	var elem Type
	switch t := ct.(type) {
	case *Slice:
		elem = t.Elem
	case *ArrayT:
		elem = t.Elem
	default:
		c.errorAt(v.Pos, "this type does not support indexed writes", typeOrUn(ct, untyped{}),
			"indexed writes apply to lists and fixed-size arrays (i == len grows the list); use put for maps")
		return
	}
	c.requireAssignableAt(v.Right, rhs, rinfo, elem, "indexed element")
	// 存储点 ②：下标存
	c.markStoreGuard(v.Right, cb, "indexed element")
}

func (c *Checker) checkCompound(v *parse.Assign, rhs Type, target Type, rinfo untyped, name string) {
	if !isNumeric(target) || (rhs != nil && !isNumeric(rhs)) {
		c.errorAt(v.Pos, "compound assignment applies to numeric types only", name+" "+v.Op+" …",
			"both sides must be numeric; use an explicit method call for anything else")
		return
	}
	if rhs == nil && numLit(rinfo) && !c.untypedFits(rinfo, target) {
		c.errorAt(v.Pos, "the literal does not fit the target type",
			name+" "+v.Op+" "+rinfo.text,
			"adjust the literal or use a wider type (core design §2.4)")
		return
	}
	if rhs != nil && !identical(rhs, target) {
		c.errorAt(v.Pos, "the two sides of a compound assignment differ in type", target.String()+" "+v.Op+" "+rhs.String(),
			"convert explicitly first (red line 1)")
	}
}

// checkStmtOnly: 表达式作为语句子位时的轻量检查（也入类型表：emit 需要每个
// 具类型表达式的类型来定宽与选打印原语，缺一条就会在发射期变成「类型未知」）。
func (c *Checker) checkStmtOnly(e parse.Expr) {
	if e != nil {
		c.checkExpr(e, nil)
	}
}

// checkMultiAssign: a, err = f()——名字必须全部已存在（_ 弃位除外），
// 混搭 = 编译错，数量与被调返回一致（核心设计 §六 绑定规则）。
func (c *Checker) checkMultiAssign(v *parse.MultiAssign) {
	ct, _, _ := c.checkExprFull(v.Right, nil)
	mt, isMulti := ct.(*MultiType)
	if !isMulti {
		c.errorAt(v.Pos, "the callee does not return multiple values", exprText(v.Right),
			"a multi-target assignment needs a callee returning 2 or more values (core design §6)")
		return
	}
	if len(v.LHS) != len(mt.Elems) {
		c.errorAt(v.Pos, "the number of binding targets does not match the number of results",
			itoa(len(v.LHS))+" targets but the callee returns "+itoa(len(mt.Elems)),
			"add the missing targets, or discard with _")
		return
	}
	for i, lhs := range v.LHS {
		id, ok := lhs.(*parse.Ident)
		if !ok {
			c.errorAt(parse.ExprPos(lhs), "a multi-target assignment needs variable names as targets", exprText(lhs),
				"write name, name = f(); _ discards a slot")
			continue
		}
		if id.Name == "_" {
			// §15.7 规则③：末位（Err）弃位 = 吞错（赋值形态，与声明形态同一判据）。
			if i == len(v.LHS)-1 && isErr(mt.Elems[i]) {
				c.errorAt(parse.ExprPos(lhs), "the Err result must be consumed", "_ = "+exprText(v.Right),
					"bind it (v, err = f()) and test it, or propagate with f()? / f()! (core design §15.7 rule 3)")
			}
			continue
		}
		sym, exists := c.scope.Lookup(id.Name)
		if !exists {
			c.errorAt(parse.ExprPos(lhs), "the assignment target is undeclared (mixing declaration and assignment is a compile error)", id.Name,
				"declare a new name with var (var "+targetListOf(v.LHS)+" = f()), or declare first and assign afterwards")
			continue
		}
		c.requireAssignableAt(v.Right, mt.Elems[i], untyped{}, sym.Type, "assigned to "+id.Name)
		// 存储点 ⑤（§五 R3）：多返回解构接收 —— 调用结果界 = 调用点偏移 + 被调返回摘要，
		// 目标变量更浅时必须插守卫。此前**从未**插过 ⇒ `region { a, b = mk(1) }`
		// （a/b 声明在块外）静默把块内对象写进更浅的槽，弹出后读到别的对象。
		// 守卫标记挂在**目标名节点**上（每个目标一个键，互不覆盖），发射侧按名取用。
		c.markVarStoreAt(v.Right, lhs, sym, "multi-target receiver "+id.Name)
		c.updateNilState(id.Name, nil) // 接收位 = 调用产物 → 未证
	}
}

func targetListOf(exprs []parse.Expr) string {
	var parts []string
	for _, e := range exprs {
		if id, ok := e.(*parse.Ident); ok {
			parts = append(parts, id.Name)
		}
	}
	return joinComma(parts)
}

func joinComma(parts []string) string {
	s := ""
	for i, p := range parts {
		if i > 0 {
			s += ", "
		}
		s += p
	}
	return s
}
