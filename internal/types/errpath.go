package types

import (
	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// 错误通路 E1–E4（核心设计 §15.7 / §16 N5）。
//
//   E1 传播 `expr?`      —— 等价于既有 `check expr`（解析层已归一到 CheckExpr）
//   E2 降级 `expr or d`  —— 末位 Err 非 nil 取默认值，不中断
//   E3 就地 `expr catch e { … }` —— 在本处处理：块要么总是退出，要么以一条
//                          表达式语句结尾（那条表达式的值即整个表达式的值）
//   E4 断言 `expr!`      —— 出错 = trap（含 code/msg/位置）
//
// 四条通路只对**末位 Err**生效（Err 恒为返回位末位，红线 2）。E2/E3/E4 都
// **不要求所在函数带 Err 位**：它们不传播。
// ---------------------------------------------------------------------------

// rejectUnconsumedErr 实现 §15.7 规则③在**无人接收结果**的语句位上（defer / spawn）：
// 调用的末位是 Err ⇒ 那个错误永远观察不到 ⇒ 编译错。
func (c *Checker) rejectUnconsumedErr(e parse.Expr, where string, at parse.Pos) {
	call, ok := e.(*parse.Call)
	if !ok {
		return
	}
	ct, _, _ := c.checkExprFull(call, nil)
	hasErr := isErr(ct)
	if mt, isMulti := ct.(*MultiType); isMulti && len(mt.Elems) > 0 {
		hasErr = isErr(mt.Elems[len(mt.Elems)-1])
	}
	if !hasErr {
		return
	}
	c.errorAt(at, "a "+where+" call cannot report an Err", where+" "+exprText(call),
		"handle the error before the "+where+" call: bind it (var v, err = f()) and test it, or propagate with f()? / f()! (core design §15.7 rule 3)")
}

// errPayload 是 E2–E4 三者的共同前提：操作数必须是一个「末位 Err」的调用。
// 返回负载类型（仅 Err 的调用返回 nil）与多返回表。
func (c *Checker) errPayload(e parse.Expr, at parse.Pos, path string) (Type, []Type, bool) {
	call, ok := e.(*parse.Call)
	if !ok {
		c.errorAt(at, "the operand of `"+path+"` must be a call", exprText(e),
			"`"+path+"` consumes the trailing Err of a call; bind the value to a name first if it is not a call")
		return nil, nil, false
	}
	// 泛型调用点也要定型（checkExprFull 落表，emit/lower 都靠它）。
	ct, _, _ := c.checkExprFull(call, nil)
	// **只返回 Err 的调用**：类型表里是 Err 本身（不是多返回），必须与"末位不是 Err"
	// 区分开 —— 否则 `errOnly()?` 会报"末位必须是 Err"这条**反了**的诊断
	// （诊断审计实测：准确的文案在下面，却永远到不了）。
	if isErr(ct) {
		c.errorAt(at, "the operand of `"+path+"` returns Err only: there is no value to use", exprText(call),
			"bind it explicitly: var err = f(); or declare the result it should carry: -> (T, Err)")
		return nil, []Type{ct}, false
	}
	mt := MultiElems(ct)
	if mt == nil || len(mt) == 0 || !isErr(mt[len(mt)-1]) {
		c.errorAt(at, "the last result of the `"+path+"` operand must be Err", exprText(call),
			"only a call whose signature ends with Err has an error path; an ordinary call needs no `"+path+"`")
		return nil, nil, false
	}
	if len(mt) == 1 {
		c.errorAt(at, "the operand of `"+path+"` returns Err only: there is no value to use", exprText(call),
			"bind it explicitly: var err = f(); or declare the result it should carry: -> (T, Err)")
		return nil, mt, false
	}
	var head Type
	if len(mt) == 2 {
		head = mt[0]
	} else {
		head = &MultiType{Elems: mt[:len(mt)-1]}
	}
	return head, mt, true
}

// checkOr: E2 `expr or <默认值>`。
func (c *Checker) checkOr(v *parse.OrExpr, expect Type) (Type, bound, untyped) {
	head, _, ok := c.errPayload(v.X, v.Pos, "or")
	if !ok {
		return nil, nilBound, untyped{}
	}
	want := expect
	if want == nil {
		want = head
	}
	dt, db, di := c.checkExprFull(v.Default, want)
	c.requireAssignableAt(v.Default, dt, di, head, "the fallback value of `or`")
	_ = db
	return head, nilBound, untyped{}
}

// checkAssert: E4 `expr!` —— 出错即 trap，值 = 负载。
func (c *Checker) checkAssert(v *parse.AssertExpr, expect Type) (Type, bound, untyped) {
	head, _, ok := c.errPayload(v.X, v.Pos, "!")
	if !ok {
		return nil, nilBound, untyped{}
	}
	if expect != nil {
		c.requireAssignableAt(v.X, head, untyped{}, expect, "the asserted value")
	}
	return head, nilBound, untyped{}
}

// checkCatch: E3 `expr catch e { … }`。
//
// 块的值规则（唯一定义，检查器裁决）：
//   - 块**总是退出**（以 return / 全分支 return 的 if / 穷尽 match 结尾）→ 表达式
//     永不产出值，类型取负载类型（可在任意位置使用，控制流不会到达取值点）；
//   - 否则块必须**以一条表达式语句结尾**，其类型须可赋给负载类型 —— 那条表达式
//     的值就是整个 catch 表达式的值。
func (c *Checker) checkCatch(v *parse.CatchExpr, expect Type) (Type, bound, untyped) {
	head, _, ok := c.errPayload(v.X, v.Pos, "catch")
	if !ok {
		return nil, nilBound, untyped{}
	}
	want := expect
	if want == nil {
		want = head
	}
	c.scope = NewScope(c.scope, c.regionOff)
	// e 绑定的是**该 Err 本身**（E1–E4 只对末位 Err 生效）。
	c.scope.Declare(&Symbol{Name: v.Name, Type: TErr, Kind: SymVar, DeclOffset: c.regionOff})
	if v.Block != nil {
		c.checkBlock(v.Block)
		if !alwaysExitsBlock(v.Block) {
			last := lastStmt(v.Block)
			es, isExpr := last.(*parse.ExprStmt)
			if !isExpr {
				c.errorAt(v.Pos, "a catch block must end with a `return` or with the expression that yields its value",
					"catch "+v.Name+" { … }",
					"end the block with the value: catch e { 0 }; if the error means giving up, end it with return")
			} else {
				vt, _, vi := c.checkExprFull(es.X, want)
				c.requireAssignableAt(es.X, vt, vi, head, "the value of the catch block")
			}
		}
	}
	c.scope = c.scope.parent
	return head, nilBound, untyped{}
}

// alwaysExitsBlock 报告一个块是否**必然离开当前函数**（末尾语句总是退出）。
// 这是「catch 块不必产出值」的判定，也是 catch 语义可静态裁决的全部依据。
func alwaysExitsBlock(b *parse.Block) bool {
	if b == nil {
		return false
	}
	return alwaysExits(lastStmt(b))
}

// BlockAlwaysExits 是 alwaysExitsBlock 的导出形式（AIR 侧按同一裁决把 catch
// 块降级成「有值」或「必退出」两种形态 —— 禁第二份判定）。
func BlockAlwaysExits(b *parse.Block) bool { return alwaysExitsBlock(b) }

// ---------------------------------------------------------------------------
// N2 字符串插值（核心设计 §16）：占位符只接受能**最短往返**格式化的类型。
// 数值走与 print 族同一套 aic_fmt_*（红线 20：禁第二套格式化），字符串恒等，
// bool 为 true/false。其余类型（class/enum/容器/函数/Err）一律编译错。
// ---------------------------------------------------------------------------

// checkInterp: 插值字面量的类型恒为 str；每个占位符单独定型并校验可格式化性。
func (c *Checker) checkInterp(v *parse.InterpLit) (Type, bound, untyped) {
	for _, e := range v.Values {
		vt, _, _ := c.checkExprFull(e, nil)
		if vt == nil {
			continue
		}
		if interpFormattable(vt) {
			continue
		}
		c.errorAt(parse.ExprPos(e), "this type cannot be interpolated into a string",
			exprText(e)+" has type "+vt.String(),
			"interpolation formats str, integers, floats and bool only; convert explicitly first (str.fromI64 / str.fromF64 / str.fromBool), or give the type @derive(ToString) and interpolate its fields (core design §16 N2)")
	}
	return TStr, nilBound, untyped{}
}

// interpFormattable 报告某类型能否直接插值。
func interpFormattable(t Type) bool {
	b, ok := t.(*Basic)
	if !ok {
		return false
	}
	switch b.Name {
	case "str", "bool", "f32", "f64",
		"i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize":
		return true
	}
	return false
}

// InterpFormattable 暴露给降级/发射层（唯一判定，禁第二份）。
func InterpFormattable(t Type) bool { return interpFormattable(t) }

// ---------------------------------------------------------------------------
// N1 闭包捕获（核心设计 §16 N1）。
//
// 语义：**按值捕获**——捕获时点拷贝一份；捕获项集合 = 环境结构体。
// 判定「是不是捕获」只看名字的解析落点：命中点在当前闭包自己的作用域
// （lambdaBase 及其内部块）之内 = 局部量；之外 = 自由变量 = 捕获项。
// ---------------------------------------------------------------------------

// lookupInsideLambda 在「当前闭包自己的作用域链」里查名字（到 lambdaBase 为止）。
func (c *Checker) lookupInsideLambda(name string) bool {
	for s := c.scope; s != nil; s = s.parent {
		if _, ok := s.names[name]; ok {
			return true
		}
		if s == c.lambdaBase {
			return false
		}
	}
	return false
}

// noteCapture 登记一次捕获（同名字只记一次；顺序 = 首次出现序，确定性），
// 并把**本次引用**记进 capRefs（AIR 按节点判定，同名局部量遮蔽时不会误改）。
func (c *Checker) noteCapture(e parse.Expr, name string, sym *Symbol) {
	if c.caps == nil {
		return
	}
	// 捕获物不得是可变全局（红线 21：无顶层 var）—— 语言层已不可能，这里只做
	// 防御：常量/函数/类型不是值，下面按 Kind 过滤。
	switch sym.Kind {
	case SymVar, SymConst:
	default:
		return
	}
	slot := -1
	for i := range *c.caps {
		if (*c.caps)[i].Name == name {
			slot = i
			break
		}
	}
	if slot < 0 {
		t := sym.Type
		if t == nil {
			t = TStr // 未定型 const 字面量：按 str 记（只影响区域判定）
		}
		*c.caps = append(*c.caps, capture{Name: name, Type: t, Off: sym.DeclOffset})
		slot = len(*c.caps) - 1
	}
	if c.capRefs != nil && e != nil {
		c.capRefs[e] = slot
	}
}

// closureBound 是闭包值的区域界。捕获项一律来自**外层或当前块**（深度 ≤ 创建点），
// 故闭包与其它创建表达式同形：off = 创建点深度，exact = true ——
// 「在 region 内创建、存进 region 外的变量」因此是**编译错**（N1 的既定语义）。
func closureBound(caps []capture, cur int) bound {
	off := cur
	for _, cp := range caps {
		if (isReference(cp.Type) || containsReference(cp.Type)) && cp.Off > off {
			off = cp.Off
		}
	}
	return bound{off: off, exact: true}
}

func lastStmt(b *parse.Block) parse.Stmt {
	if b == nil || len(b.Stmts) == 0 {
		return nil
	}
	return b.Stmts[len(b.Stmts)-1]
}

// alwaysExits 报告一条语句是否必然使控制流离开当前函数。
func alwaysExits(s parse.Stmt) bool {
	switch v := s.(type) {
	case *parse.ReturnStmt:
		return true
	case *parse.Block:
		return alwaysExitsBlock(v)
	case *parse.IfStmt:
		return v.Else != nil && alwaysExitsBlock(v.Then) && alwaysExitsBlock(v.Else)
	case *parse.MatchStmt:
		// 穷尽性由检查器保证 ⇒ 全部臂都退出即必然退出（无负载枚举恒穷尽）。
		if len(v.Arms) == 0 {
			return false
		}
		for _, arm := range v.Arms {
			if arm.Block == nil || !alwaysExitsBlock(arm.Block) {
				return false
			}
		}
		return true
	case *parse.ScopeStmt:
		return alwaysExitsBlock(v.Body)
	case *parse.RegionStmt:
		return alwaysExitsBlock(v.Body)
	}
	return false
}
