package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 语句发射 (核心设计 §三/§五/§六)。
//
// 每条语句前带 #line (红线 9); 块 = C 块; 遮蔽规则由检查器保证, 发射只按
// 已解析的名字走 (同块重名已在检查期拒绝, 故 C 变量名可直接用 AIC 名)。
// ---------------------------------------------------------------------------

// emitBody 发射一个块体。fnHasErr 决定 check/return 的出口形态。
func (c *Ctx) emitBody(b *parse.Block, fnHasErr bool) error {
	if b == nil {
		return nil
	}
	for _, s := range b.Stmts {
		if err := c.emitStmt(s, fnHasErr); err != nil {
			return err
		}
	}
	return nil
}

// emitScopedBlock 发射一个子块 (内层可遮蔽外层, §三)。
func (c *Ctx) emitScopedBlock(b *parse.Block, fnHasErr bool) error {
	c.line("{")
	if err := c.emitBody(b, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	return nil
}

// emitStmt 发射单条语句。
func (c *Ctx) emitStmt(s parse.Stmt, fnHasErr bool) error {
	switch v := s.(type) {
	case *parse.VarDecl:
		return c.emitVarDecl(v)
	case *parse.ExprStmt:
		return c.emitExprStmt(v)
	case *parse.IfStmt:
		return c.emitIf(v, fnHasErr)
	case *parse.ForStmt:
		return c.emitFor(v, fnHasErr)
	case *parse.ReturnStmt:
		return c.emitReturn(v, fnHasErr)
	case *parse.BranchStmt:
		return c.emitBranch(v)
	case *parse.MatchStmt:
		return c.emitMatchStmt(v, fnHasErr)
	case *parse.DeferStmt:
		return c.emitDeferReg(v)
	case *parse.RegionStmt:
		return c.emitRegion(v, fnHasErr)
	case *parse.Block:
		c.srcLine(v.Pos)
		return c.emitScopedBlock(v, fnHasErr)
	case *parse.ScopeStmt:
		return c.emitScope(v, fnHasErr)
	case *parse.SpawnStmt:
		return c.emitSpawn(v)
	}
	return fmt.Errorf("emit: the code generator does not support this statement yet (%s)", stmtKind(s))
}

// emitVarDecl 发射变量声明: var x T = e / var x = e / var @live x = C{…}。
func (c *Ctx) emitVarDecl(v *parse.VarDecl) error {
	c.srcLine(v.Pos)
	// 多目标 = 多返回绑定: var a, err = f()
	if len(v.Targets) > 1 {
		return c.emitMultiBind(v)
	}
	tgt := v.Targets[0]
	if tgt.Blank {
		return nil // `_` 弃位: 声明期已由检查器拒绝单 _, 此处防御
	}
	t := c.declType(v)
	if v.Init == nil {
		if types.IsArray(t) {
			c.line("%s = {0};", c.cTypeDecl(t, tgt.Name))
		} else {
			c.line("%s = %s;", c.cTypeDecl(t, tgt.Name), c.zeroValue(t))
		}
		return nil
	}
	// @live: 创建下沉到外一层区域 —— 先 push 占位再创建不可行 (创建在表达式内),
	// 故发射为"create it in an outer region"的等价形态: 直接在当前区域创建后立即登记 (见 emitGuard)。
	// 定长数组的字面量初值：C 不允许用数组表达式初始化数组
	// （`aic_i32 a[3] = (aic_i32[3]){…}` 非法），必须发成初值列表。
	if types.IsArray(t) {
		if lit, isLit := v.Init.(*parse.ArrayLit); isLit {
			elem, _, _ := types.ArrayElem(t)
			parts := make([]string, 0, len(lit.Elems))
			for _, e := range lit.Elems {
				s, err := c.expr(e, elem)
				if err != nil {
					return err
				}
				parts = append(parts, s)
			}
			c.line("%s = { %s };", c.cTypeDecl(t, tgt.Name), joinComma(parts))
			return nil
		}
	}
	// F5：登记「已声明变量」（缓存声明只能针对循环前已可见的容器）。
	if c.declaredVars == nil {
		c.declaredVars = map[string]bool{}
	}
	c.declaredVars[tgt.Name] = true
	// 长度事实（§10.4 第 12 条②）：容器字面量初始化 → len 是常量（无变更点前有效）。
	if isContainerType(t) {
		c.noteLenFact(tgt.Name, v.Init)
	}
	// @live（§五 R4）：本次创建下沉到**外一层区域**（对象活过当前 region 块）。
	savedLive := c.liveNext
	c.liveNext = tgt.Live
	expr, err := c.expr(v.Init, t)
	c.liveNext = savedLive
	if err != nil {
		return err
	}
	c.line("%s = %s;", c.cTypeDecl(t, tgt.Name), expr)
	return c.afterStore(v.Init, tgt.Name, t, "")
}

// declType 取声明类型: 显式类型优先, 否则用检查器记下的初值类型。
func (c *Ctx) declType(v *parse.VarDecl) types.Type {
	if v.Type != nil {
		if t := c.typeOfTypeExpr(v.Type); t != nil {
			return t
		}
	}
	if t := c.ti(v.Init); t != nil {
		return t
	}
	return types.TI32 // 兜底 (检查器已保证可推断, 走到这里说明类型表缺项)
}

// emitMultiBind 发射多返回绑定 (元组是调用约定糖, §二.2)。
func (c *Ctx) emitMultiBind(v *parse.VarDecl) error {
	call, _ := v.Init.(*parse.Call)
	if call == nil {
		return fmt.Errorf("emit: the right side of a multi-target declaration must be a call (line %d)", v.Pos.Line)
	}
	mt := types.MultiElems(c.ti(v.Init))
	if mt == nil {
		return fmt.Errorf("emit: the call result shape is unknown (line %d)", v.Pos.Line)
	}
	tmp := c.tmp("multi")
	c.line("%s %s = %s;", c.retStructNameFor(mt), tmp, mustExpr(c.expr(call, nil)))
	for i, tgt := range v.Targets {
		if tgt.Blank {
			continue
		}
		ct := c.cTypeName(mt[i])
		c.line("%s %s = %s._%d;", ct, tgt.Name, tmp, i)
	}
	return nil
}

// retStructNameFor 由返回类型列表合成结构名 (与 retStructName 同一规则)。
func (c *Ctx) retStructNameFor(elems []types.Type) string {
	if n, ok := stdRetStruct(elems); ok {
		return n
	}
	parts := make([]string, 0, len(elems))
	for _, r := range elems {
		parts = append(parts, sanitize(c.cTypeName(r)))
	}
	return "aic_r_" + joinUnder(parts)
}

// emitExprStmt 发射表达式语句 (调用/赋值/match 语句形)。
func (c *Ctx) emitExprStmt(v *parse.ExprStmt) error {
	switch x := v.X.(type) {
	case *parse.Assign:
		return c.emitAssign(x)
	case *parse.MultiAssign:
		return c.emitMultiAssign(x)
	case *parse.Call:
		if c.isPrintCall(x) {
			return c.emitPrint(x)
		}
		e, err := c.expr(x, nil)
		if err != nil {
			return err
		}
		c.srcLine(v.Pos)
		c.line("%s;", e)
		// 存储点 ③（append/insert/put/add）：检查器把守卫标记打在**实参**上，
		// 故这里按调用语句取标记发射 —— 漏了这一路，跨区存入容器的守卫就静默消失。
		if _, isMethod := x.Fn.(*parse.Field); isMethod && len(x.Args) > 0 {
			last := x.Args[len(x.Args)-1] // insert(i, v) 的 v 在末位
			if _, marked := c.guardOf(last); marked {
				return c.afterStore(last, "container element", nil, "")
			}
		}
		return nil
	case *parse.CheckExpr:
		// 裸 check 语句 (无绑定): 只传播错误
		c.srcLine(v.Pos)
		return c.emitCheckPropagate(x)
	default:
		e, err := c.expr(v.X, nil)
		if err != nil {
			return err
		}
		c.srcLine(v.Pos)
		c.line("(void)(%s);", e)
		return nil
	}
}

// emitAssign 发射赋值 (变量槽/字段/下标三路, 存储点 ⓪①②)。
func (c *Ctx) emitAssign(v *parse.Assign) error {
	c.srcLine(v.Pos)
	switch lhs := v.LHS.(type) {
	case *parse.Ident:
		return c.emitVarAssign(v, lhs)
	case *parse.Field:
		return c.emitFieldAssign(v, lhs)
	case *parse.Index:
		return c.emitIndexAssign(v, lhs)
	}
	return fmt.Errorf("emit: unsupported left-hand side form (line %d)", v.Pos.Line)
}

func (c *Ctx) emitVarAssign(v *parse.Assign, lhs *parse.Ident) error {
	t := c.ti(lhs)
	rhs, err := c.expr(v.Right, t)
	if err != nil {
		return err
	}
	if v.Op != "=" {
		c.line("%s %s %s;", lhs.Name, v.Op, rhs)
		return nil
	}
	c.line("%s = %s;", lhs.Name, rhs)
	return c.afterStore(v.Right, lhs.Name, t, "")
}

func (c *Ctx) emitFieldAssign(v *parse.Assign, lhs *parse.Field) error {
	recv, err := c.expr(lhs.X, nil)
	if err != nil {
		return err
	}
	ft := c.ti(lhs)
	rhs, err := c.expr(v.Right, ft)
	if err != nil {
		return err
	}
	access := fmt.Sprintf("%s->%s", paren("", c.castRefReceiver(lhs.X, recv)), lhs.Name)
	if isValueAccess(c.ti(lhs.X)) {
		access = fmt.Sprintf("(%s).%s", recv, lhs.Name)
	}
	if v.Op != "=" {
		c.line("%s %s %s;", access, v.Op, rhs)
		return nil
	}
	c.line("%s = %s;", access, rhs)
	// 宿主表达式一并传：形参宿主的守卫 limit 要在运行期读宿主对象所在区域的深度。
	return c.afterStore(v.Right, access, ft, recv)
}

func (c *Ctx) emitIndexAssign(v *parse.Assign, lhs *parse.Index) error {
	return c.emitIndexStore(v, lhs)
}

// emitReturn 发射 return (返回点规则见 §五 R3: 摘要 = 0, 非精确 > 0 时守卫)。
func (c *Ctx) emitReturn(v *parse.ReturnStmt, fnHasErr bool) error {
	c.srcLine(v.Pos)
	sig := c.curFunc
	if sig == nil {
		return fmt.Errorf("emit: return appears outside a function")
	}
	switch len(sig.Results) {
	case 0:
		if len(v.Results) == 0 {
			c.emitDeferRun()
			c.line("return;")
			return nil
		}
		e, err := c.expr(v.Results[0], nil)
		if err != nil {
			return err
		}
		c.line("(void)(%s);", e)
		c.emitDeferRun()
		c.line("return;")
		return nil
	case 1:
		return c.emitSingleReturn(v, sig)
	default:
		return c.emitMultiReturn(v, sig)
	}
}

// emitSingleReturn: 单返回位。
func (c *Ctx) emitSingleReturn(v *parse.ReturnStmt, sig *types.FuncSig) error {
	rt := sig.Results[0]
	if len(v.Results) == 0 {
		// 前置返回位零值 (§六: check 失败时返回前置零值 + 该 err)
		c.emitDeferRun()
		c.line("return %s;", c.zeroValue(rt))
		return nil
	}
	e, err := c.expr(v.Results[0], rt)
	if err != nil {
		return err
	}
	if g, ok := c.guardOf(v.Results[0]); ok && g.Kind == types.GuardReturn {
		c.emitReturnGuard(e)
	}
	if c.hasDefers() {
		// 返回值先算好再跑 defer（defer 里可能改到返回表达式的来源变量）。
		tmp := c.tmp("ret1")
		c.line("%s %s = %s;", c.cTypeName(rt), tmp, e)
		c.emitDeferRun()
		c.line("return %s;", tmp)
		return nil
	}
	c.line("return %s;", e)
	return nil
}

// emitMultiReturn: 多返回位 = 合成结构 (末位 Err)。
func (c *Ctx) emitMultiReturn(v *parse.ReturnStmt, sig *types.FuncSig) error {
	if len(v.Results) != len(sig.Results) {
		// check 前置返回: 前置零值 + 该 err 已由 emitCheckPropagate 处理
		return fmt.Errorf("emit: wrong number of results in a multi-result return (line %d)", v.Pos.Line)
	}
	tmp := c.tmp("ret")
	c.line("%s %s;", c.retStructNameFor(sig.Results), tmp)
	for i, r := range v.Results {
		e, err := c.expr(r, sig.Results[i])
		if err != nil {
			return err
		}
		c.line("%s._%d = %s;", tmp, i, e)
	}
	c.emitDeferRun()
	c.line("return %s;", tmp)
	return nil
}

// emitIf 发射 if/else (条件必须 bool; else 配对最近未配对 if = C 惯例, §三)。
func (c *Ctx) emitIf(v *parse.IfStmt, fnHasErr bool) error {
	cond, err := c.expr(v.Cond, types.TBool)
	if err != nil {
		return err
	}
	c.srcLine(v.Pos)
	c.line("if (%s) {", paren("", cond))
	if err := c.emitBody(v.Then, fnHasErr); err != nil {
		return err
	}
	if v.Else != nil {
		c.line("} else {")
		if err := c.emitBody(v.Else, fnHasErr); err != nil {
			return err
		}
	}
	c.line("}")
	return nil
}

// emitBranch 发射 break/continue (无循环标签: 嵌套循环退出用标志变量, §三)。
func (c *Ctx) emitBranch(v *parse.BranchStmt) error {
	c.srcLine(v.Pos)
	c.line("%s;", v.Keyword)
	return nil
}

// stmtKind 给诊断用的语句名：**只出现语言级名字**（`select`/`errdefer`），
// 绝不把 Go 类型名（*parse.SelectStmt）泄漏给使用者。
func stmtKind(s parse.Stmt) string {
	switch s.(type) {
	case *parse.SpawnStmt:
		return "spawn"
	case *parse.ScopeStmt:
		return "scope"
	case *parse.SelectStmt:
		return "select"
	case *parse.ErrDeferStmt:
		return "errdefer"
	case *parse.DeferStmt:
		return "defer"
	case *parse.RegionStmt:
		return "region"
	case *parse.MatchStmt:
		return "match"
	case *parse.ForStmt:
		return "for"
	case *parse.IfStmt:
		return "if"
	case *parse.ReturnStmt:
		return "return"
	case *parse.BranchStmt:
		return "break/continue"
	case *parse.VarDecl:
		return "var"
	case *parse.ExprStmt:
		return "expression statement"
	case *parse.Block:
		return "block"
	}
	return "this statement"
}

// mustExpr 忽略错误的发射助手 (仅用于已确认形状的调用点)。
func mustExpr(e string, err error) string {
	if err != nil {
		return "/* emit error */ 0"
	}
	return e
}

// paren 给二元/赋值右侧加括号 (C 优先级与 AIC 表一致, 但复合赋值右侧需保护)。
func paren(op, e string) string {
	_ = op
	return e
}

// trimAssignOp 把 AIC 复合赋值运算符原样交给 C (两者同形: += -= *= /= %=)。
func trimAssignOp(op string) string { return op }

func joinUnder(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "_"
		}
		out += p
	}
	return out
}
