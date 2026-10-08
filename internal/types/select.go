package types

import (
	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// N6 `select` / `scope.timeout`（核心设计 §16 N6）的检查。
//
// 语义：
//   - 分支只能是**通道收发**或**超时**（别的形态 = 编译错，不给"默认分支"留口子）；
//   - 多个分支同时就绪时**任选一个** —— 故锚点必须与选择顺序无关；
//   - recv 分支可绑 1 个（值）或 2 个（值, 是否收到）名字；
//   - `scope.timeout(ms)` = 到点取消该域内全部任务（结构化取消，任务树向下传播），
//     取消点是 AIC 侧的等待原语（select / recv / send / sleep），C 调用不可取消（§15.8 I9）。
// ---------------------------------------------------------------------------

// checkSelect 检查 select 语句：逐臂定型 + 绑定名落在**本臂作用域**里。
//
// 作用域规则与 Go 的 select case 一致：分支绑定只在**本分支块**内可见，
// 两个分支可以各绑一个同名 v（否则最常见的写法会在第二个分支报重名）。
func (c *Checker) checkSelect(v *parse.SelectStmt) {
	if len(v.Arms) == 0 {
		c.errorAt(v.Pos, "select needs at least one arm", "select is empty",
			"write case ch.recv() { … } / case ch.send(v) { … } / case time.after(ms) { … } (core design §16 N6)")
		return
	}
	timeArms := 0
	for i := range v.Arms {
		arm := &v.Arms[i]
		if arm.Expr == nil {
			continue
		}
		// 通道类型只能从**接收者**取：`ch.recv()` 的类型是多返回 (T, bool)、
		// `ch.send(v)` 是 void —— 拿调用结果类型判通道一定判错。
		var ch *ChanT
		isChan := false
		if arm.Kind == "recv" || arm.Kind == "send" {
			ch, isChan = c.selectArmChan(arm.Expr)
		}
		ok := true
		switch arm.Kind {
		case "recv":
			ok = c.checkSelectRecv(arm, ch, isChan)
		case "send":
			if !isChan {
				c.errorAt(arm.Pos, "a select send arm needs a channel send", "case "+exprText(arm.Expr),
					"write case ch.send(v) { … }; the expression must be a chan[T].send call (core design §16 N6)")
				ok = false
			}
			if len(arm.Targets) > 0 {
				c.errorAt(arm.Pos, "a select send arm binds nothing", "case … = ch.send(v)",
					"drop the targets: only a recv arm binds values (core design §16 N6)")
				ok = false
			}
			c.checkExprFull(arm.Expr, nil)
		case "time":
			if !isTimeAfter(arm.Expr) {
				c.errorAt(arm.Pos, "a select time arm needs time.after(ms)", "case "+exprText(arm.Expr),
					"write case time.after(ms) { … } (core design §16 N6)")
				ok = false
			}
			if len(arm.Targets) > 0 {
				c.errorAt(arm.Pos, "a select time arm binds nothing", "case … = time.after(ms)",
					"drop the targets: only a recv arm binds values (core design §16 N6)")
				ok = false
			}
			timeArms++
			if timeArms > 1 {
				// 两个超时分支没有可定义的语义（最早到点者必然先就绪，后者永不执行）
				// ⇒ 编译期直接拒，别留给用户"看起来能用"的歧义。
				c.errorAt(arm.Pos, "a select allows at most one time.after arm", "case "+exprText(arm.Expr),
					"keep the earliest deadline and drop the rest (core design §16 N6)")
				ok = false
			}
			c.checkExprFull(arm.Expr, nil)
		default:
			c.errorAt(arm.Pos, "a select arm must be a channel receive, a channel send, or time.after",
				"case "+exprText(arm.Expr),
				"the three arm forms are case v, ok = ch.recv() / case ch.send(v) / case time.after(ms) (core design §16 N6)")
			continue
		}
		// 本臂作用域：绑定只在本分支块内可见。
		saved := c.scope
		c.scope = NewScope(saved, c.regionOff)
		if ok && arm.Kind == "recv" {
			c.bindSelectRecv(arm, ch)
		}
		if arm.Block != nil {
			c.checkBlock(arm.Block)
		}
		c.scope = saved
	}
}

// selectArmChan 取一条通道分支的通道类型（从 `ch.recv()` / `ch.send(v)` 的接收者取）。
func (c *Checker) selectArmChan(e parse.Expr) (*ChanT, bool) {
	saved := c.scope
	call, ok := e.(*parse.Call)
	if !ok {
		return nil, false
	}
	field, ok := call.Fn.(*parse.Field)
	if !ok || !isChannelMethod(field.Name) {
		return nil, false
	}
	// 只**探类型**，不改作用域/不落诊断：接收者是不是通道由调用点检查负责报错
	// （这里是判定，不是检查；重复报错会把一条错变成两条）。
	c.scope = NewScope(saved, c.regionOff)
	rt, _, _ := c.checkExprFull(field.X, nil)
	c.scope = saved
	ch, isCh := rt.(*ChanT)
	return ch, isCh
}

// isChannelMethod 报告方法名是不是通道原语（recv/send/len）。
func isChannelMethod(name string) bool {
	return name == "recv" || name == "send" || name == "len"
}

// checkSelectRecv 检查一条 recv 分支的头部（绑定个数），返回是否可用。
func (c *Checker) checkSelectRecv(arm *parse.SelectArm, ch *ChanT, isChan bool) bool {
	if !isChan {
		c.errorAt(arm.Pos, "a select recv arm needs a channel receive", "case "+exprText(arm.Expr),
			"write case v, ok = ch.recv() { … }; the expression must be a chan[T].recv call (core design §16 N6)")
		return false
	}
	if len(arm.Targets) > 2 {
		c.errorAt(arm.Pos, "a select recv arm binds at most two names", "case a, b, c = ch.recv()",
			"write case v = ch.recv() or case v, ok = ch.recv() (core design §16 N6)")
		return false
	}
	c.checkExprFull(arm.Expr, nil)
	return true
}

// bindSelectRecv 把 recv 分支的绑定名登记进**本臂作用域**。
// 第二个绑定 = "是否真的收到"（通道为空且本 scope 已无存活任务时为 false，§七【O2】）。
func (c *Checker) bindSelectRecv(arm *parse.SelectArm, ch *ChanT) {
	for i := range arm.Targets {
		tgt := arm.Targets[i]
		if tgt.Name == "_" || tgt.Blank {
			continue
		}
		ty := ch.Elem
		if i == 1 {
			ty = TBool
		}
		c.declareVar(tgt, ty, arm.Expr)
	}
}

// isTimeAfter 报告表达式是否形如 time.after(…)（时间分支的唯一形态）。
func isTimeAfter(e parse.Expr) bool {
	call, ok := e.(*parse.Call)
	if !ok {
		return false
	}
	field, ok := call.Fn.(*parse.Field)
	if !ok || field.Name != "after" {
		return false
	}
	id, isID := field.X.(*parse.Ident)
	return isID && id.Name == "time"
}

// checkScopeTimeout 检查 `scope.timeout(ms) { … }`：超时值是整数毫秒（> 0）。
func (c *Checker) checkScopeTimeout(v *parse.ScopeStmt) {
	if v.Timeout == nil {
		return
	}
	tt, _, tinfo := c.checkExprFull(v.Timeout, TI64)
	if tt != nil {
		c.requireAssignableAt(v.Timeout, tt, tinfo, TI64, "scope timeout in milliseconds")
	}
	if isNegativeLiteral(v.Timeout) {
		c.errorAt(v.Pos, "a scope timeout cannot be negative", "scope.timeout("+exprText(v.Timeout)+")",
			"a timeout is a duration in milliseconds: write scope.timeout(50) (core design §16 N6)")
	}
}
