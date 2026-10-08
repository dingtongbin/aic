package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// defer 发射（核心设计 §三：函数级 LIFO、无条件执行、region 块退出不触发；
// §五：defer 实参界 ≤ 函数入口或值类型）。
//
// 形态：**每个注册点一个 trampoline + 一块上下文**，注册时把上下文按值拷进
// 运行时 defer 栈（`aic_defer_push_copy`，循环内注册 = 每次一份独立副本），
// 函数每个出口执行 `AIC_DEFER_RUN`（LIFO）。
//
//	调用/打印形 defer f(a, b)
//	  实参在**注册点**求值进上下文（值语义），trampoline 里以同名局部量回放调用
//	  （回放靠 c.subst 把实参节点替换成捕获的局部量，故实参只求值一次）。
//	块形 defer { … }
//	  上下文持**指向外层局部量的指针**；trampoline 拷入 → 执行块 → 拷回。
//	  读写都落回原变量（Go 语义），循环内多次注册共享同一槽位。
//
// 确定性：编号按函数内源序分配（红线 12 / H2），名字全部由编号拼出，无随机。
// ---------------------------------------------------------------------------

// deferLocal 是注册点可见的一个外层局部量（C 名 + 类型）。
type deferLocal struct {
	cname string
	typ   types.Type
}

// deferSite 是一个 defer 注册点的发射计划。
type deferSite struct {
	id      int
	pos     parse.Pos
	isPrint bool
	block   *parse.Block
	call    *parse.Call

	fnName  string // trampoline 符号：aic_df_<id>
	ctxName string // 上下文结构：aic_dfctx_<id>
	tmpName string // 注册点上下文变量：aic_dc_<id>

	// 值捕获（调用/打印形）。
	valArgs   []parse.Expr
	valWants  []types.Type
	valFields []string
	valNames  []string
	valTypes  []string

	// 接收者（方法形：this.m(...) / xs.append(...)）。
	recvNode  parse.Expr
	recvWant  types.Type
	recvField string
	recvName  string
	recvType  string

	// 指针捕获（块形）。
	ptrVars   []string
	ptrFields []string
	ptrTypes  []string
}

// deferState 是一个函数内的 defer 发射状态。
type deferState struct {
	sites  []*deferSite
	byStmt map[*parse.DeferStmt]*deferSite
	stack  string // C 侧的 defer 栈变量名（无注册点时为空）
}

// collectDeferSites 按源序收集函数体内的 defer 注册点（含嵌套块/循环/if/match/region）。
func (c *Ctx) collectDeferSites(sig *types.FuncSig, body *parse.Block) (*deferState, error) {
	st := &deferState{byStmt: map[*parse.DeferStmt]*deferSite{}}
	if body == nil {
		return st, nil
	}
	scope := make([]deferLocal, 0, len(sig.Params)+4)
	if sig.Recv != "" {
		if cls := c.recvClass(sig.Recv); cls != nil {
			scope = append(scope, deferLocal{cname: "this__", typ: cls})
		}
	}
	for i, p := range sig.Params {
		var t types.Type
		if i < len(sig.ParamTypes) {
			t = sig.ParamTypes[i]
		}
		scope = append(scope, deferLocal{cname: p, typ: t})
	}

	var walkBlock func(b *parse.Block, sc []deferLocal) ([]deferLocal, error)
	var walkStmt func(s parse.Stmt, sc []deferLocal) ([]deferLocal, error)

	walkStmt = func(s parse.Stmt, sc []deferLocal) ([]deferLocal, error) {
		switch v := s.(type) {
		case *parse.DeferStmt:
			site, err := c.buildDeferSite(v, sc)
			if err != nil {
				return sc, err
			}
			c.deferSeq++
			site.id = c.deferSeq
			site.fnName = fmt.Sprintf("aic_df_%d", site.id)
			site.ctxName = fmt.Sprintf("aic_dfctx_%d", site.id)
			site.tmpName = fmt.Sprintf("aic_dc_%d", site.id)
			st.sites = append(st.sites, site)
			st.byStmt[v] = site
			if site.block != nil {
				// defer 块内再出现 defer / return / break / continue：语义未定
				// （出口已在进行中），明确拒绝而不是发射半截语义。
				if err := rejectDeferBlockControl(site.block); err != nil {
					return sc, err
				}
			}
			return sc, nil
		case *parse.Block:
			return walkBlock(v, sc)
		case *parse.IfStmt:
			if _, err := walkBlock(v.Then, sc); err != nil {
				return sc, err
			}
			if v.Else != nil {
				return walkBlock(v.Else, sc)
			}
			return sc, nil
		case *parse.ForStmt:
			inner := sc
			if v.Init != nil {
				var err error
				if inner, err = walkStmt(v.Init, inner); err != nil {
					return sc, err
				}
			}
			if v.IsRange {
				for _, n := range v.Names {
					if n.Blank {
						continue
					}
					inner = append(inner, deferLocal{cname: n.Name, typ: c.nameType(n.Name)})
				}
			}
			_, err := walkBlock(v.Body, inner)
			return sc, err
		case *parse.RegionStmt:
			_, err := walkBlock(v.Body, sc)
			return sc, err
		case *parse.MatchStmt:
			for _, arm := range v.Arms {
				if arm.Block == nil {
					continue
				}
				inner := sc
				for _, p := range arm.Patterns {
					// 负载绑定名才是局部量（p.Name 是变体名，不是变量）。
					if p.Binding != "" && p.Binding != "_" {
						inner = append(inner, deferLocal{cname: p.Binding, typ: c.nameType(p.Binding)})
					}
				}
				if _, err := walkBlock(arm.Block, inner); err != nil {
					return sc, err
				}
			}
			return sc, nil
		case *parse.VarDecl:
			for _, t := range v.Targets {
				if t.Blank {
					continue
				}
				sc = append(sc, deferLocal{cname: t.Name, typ: c.nameType(t.Name)})
			}
			return sc, nil
		}
		return sc, nil
	}

	walkBlock = func(b *parse.Block, sc []deferLocal) ([]deferLocal, error) {
		if b == nil {
			return sc, nil
		}
		cur := sc
		for _, s := range b.Stmts {
			next, err := walkStmt(s, cur)
			if err != nil {
				return cur, err
			}
			cur = next
		}
		return cur, nil
	}
	if _, err := walkBlock(body, scope); err != nil {
		return nil, err
	}
	return st, nil
}

// buildDeferSite 分析一个注册点：调用/打印形捕获实参值，块形捕获外层局部量指针。
func (c *Ctx) buildDeferSite(v *parse.DeferStmt, scope []deferLocal) (*deferSite, error) {
	s := &deferSite{pos: v.Pos, block: v.Block}
	if v.Block != nil {
		for _, loc := range scope {
			if loc.typ == nil || isArrayLike(loc.typ) {
				// 定长数组在 C 里不可赋值（无整体拷贝），跳过捕获：
				// 若块真的引用它，C 编译期会报未声明，不会被静默错译。
				continue
			}
			s.ptrVars = append(s.ptrVars, loc.cname)
			s.ptrFields = append(s.ptrFields, fmt.Sprintf("p%d", len(s.ptrFields)))
			s.ptrTypes = append(s.ptrTypes, c.cTypeName(loc.typ))
		}
		return s, nil
	}
	call, ok := v.Call.(*parse.Call)
	if !ok {
		return nil, fmt.Errorf("emit: defer must be followed by a call or a block (line %d)", v.Pos.Line)
	}
	s.call = call
	s.isPrint = c.isPrintCall(call)
	sig := c.calleeSig(call)
	if f, isField := call.Fn.(*parse.Field); isField {
		// 只有"value receiver"才需要捕获；包限定调用（print./str./os./math./option.）
		// 的 Fn.X 是包名，不是值，捕获它会发射出一个不存在的标识符。
		if rt := c.receiverType(f.X); rt != nil {
			s.recvNode = f.X
			s.recvField = "r0"
			s.recvName = fmt.Sprintf("aic_dfr_%d", v.Pos.Line)
			s.recvWant = rt
			s.recvType = c.cTypeName(rt)
		}
	}
	for i, a := range call.Args {
		var want types.Type
		if sig != nil && i < len(sig.ParamTypes) {
			want = sig.ParamTypes[i]
		}
		if want == nil {
			want = c.ti(a)
		}
		s.valArgs = append(s.valArgs, a)
		s.valWants = append(s.valWants, want)
		s.valFields = append(s.valFields, fmt.Sprintf("a%d", i))
		s.valNames = append(s.valNames, fmt.Sprintf("aic_dfv_a%d", i))
		s.valTypes = append(s.valTypes, c.deferCT(a, want))
	}
	return s, nil
}

// receiverType 取方法调用接收者的类型（this → 当前函数接收者类）。
func (c *Ctx) receiverType(e parse.Expr) types.Type {
	if _, isThis := e.(*parse.ThisExpr); isThis {
		if c.curFunc != nil && c.curFunc.Recv != "" {
			if cls := c.recvClass(c.curFunc.Recv); cls != nil {
				return cls
			}
		}
		return nil
	}
	return c.ti(e)
}

// calleeSig 解析调用目标签名（自由函数 / 方法 / 内建），失败返回 nil。
func (c *Ctx) calleeSig(call *parse.Call) *types.FuncSig {
	if c.Info == nil {
		return nil
	}
	switch fn := call.Fn.(type) {
	case *parse.Ident:
		if sig, ok := c.Info.Funcs[fn.Name]; ok {
			return sig
		}
	case *parse.Field:
		if cls, ok := c.receiverType(fn.X).(*types.Class); ok {
			if sig, ok := cls.Method(fn.Name); ok {
				return sig
			}
		}
	}
	return nil
}

// deferCT 决定捕获值的 C 类型：优先目标形参类型，其次实参自身类型，最后字面量默认。
func (c *Ctx) deferCT(arg parse.Expr, want types.Type) string {
	if want != nil {
		return c.cTypeName(want)
	}
	if t := c.ti(arg); t != nil {
		return c.cTypeName(t)
	}
	switch arg.(type) {
	case *parse.StrLit:
		return "aic_str"
	case *parse.IntLit:
		return "aic_i32"
	case *parse.FloatLit:
		return "aic_f64"
	case *parse.BoolLit:
		return "bool"
	case *parse.NilLit:
		return "void *"
	}
	return "int"
}

// isArrayLike 报告该类型是否为 C 里不可整体赋值的定长数组。
func isArrayLike(t types.Type) bool {
	_, ok := t.(*types.ArrayT)
	return ok
}

// rejectDeferBlockControl 拒绝 defer 块内的 defer/return/check/break/continue。
func rejectDeferBlockControl(b *parse.Block) error {
	var check func(stmts []parse.Stmt) error
	var checkExpr func(e parse.Expr) error
	checkExpr = func(e parse.Expr) error {
		if e == nil {
			return nil
		}
		switch v := e.(type) {
		case *parse.CheckExpr:
			return fmt.Errorf("emit: check is not supported inside a defer block (line %d) - move it into a named function", parse.ExprPos(e).Line)
		case *parse.Unary:
			return checkExpr(v.X)
		case *parse.Binary:
			if err := checkExpr(v.Left); err != nil {
				return err
			}
			return checkExpr(v.Right)
		case *parse.Assign:
			if err := checkExpr(v.LHS); err != nil {
				return err
			}
			return checkExpr(v.Right)
		case *parse.Call:
			if err := checkExpr(v.Fn); err != nil {
				return err
			}
			for _, a := range v.Args {
				if err := checkExpr(a); err != nil {
					return err
				}
			}
		case *parse.Index:
			if err := checkExpr(v.X); err != nil {
				return err
			}
			if err := checkExpr(v.Index); err != nil {
				return err
			}
			return checkExpr(v.End)
		case *parse.Field:
			return checkExpr(v.X)
		case *parse.ArrayLit:
			for _, x := range v.Elems {
				if err := checkExpr(x); err != nil {
					return err
				}
			}
		case *parse.CompositeLit:
			for _, f := range v.Fields {
				if err := checkExpr(f.Value); err != nil {
					return err
				}
			}
		}
		return nil
	}
	check = func(stmts []parse.Stmt) error {
		for _, s := range stmts {
			switch v := s.(type) {
			case *parse.DeferStmt:
				return fmt.Errorf("emit: registering another defer inside a defer block is not supported (line %d)", v.Pos.Line)
			case *parse.ReturnStmt:
				return fmt.Errorf("emit: return is not supported inside a defer block (line %d)", v.Pos.Line)
			case *parse.BranchStmt:
				return fmt.Errorf("emit: %s is not supported inside a defer block (line %d)", v.Keyword, v.Pos.Line)
			case *parse.ExprStmt:
				if err := checkExpr(v.X); err != nil {
					return err
				}
			case *parse.VarDecl:
				if err := checkExpr(v.Init); err != nil {
					return err
				}
			case *parse.IfStmt:
				if err := checkExpr(v.Cond); err != nil {
					return err
				}
				if err := check(v.Then.Stmts); err != nil {
					return err
				}
				if v.Else != nil {
					if err := check(v.Else.Stmts); err != nil {
						return err
					}
				}
			case *parse.Block:
				if err := check(v.Stmts); err != nil {
					return err
				}
			case *parse.ForStmt:
				if err := checkExpr(v.Cond); err != nil {
					return err
				}
				if err := check(v.Body.Stmts); err != nil {
					return err
				}
			case *parse.RegionStmt:
				if err := check(v.Body.Stmts); err != nil {
					return err
				}
			case *parse.MatchStmt:
				for _, arm := range v.Arms {
					if arm.Block != nil {
						if err := check(arm.Block.Stmts); err != nil {
							return err
						}
					}
				}
			}
		}
		return nil
	}
	return check(b.Stmts)
}

// emitDeferTrampolines 发射各注册点的上下文结构与 trampoline（定义在宿主函数之前）。
func (c *Ctx) emitDeferTrampolines(st *deferState) error {
	if st == nil || len(st.sites) == 0 {
		return nil
	}
	st.stack = "aic_dfs"
	for _, s := range st.sites {
		c.line("struct %s {", s.ctxName)
		for i := range s.valFields {
			c.line("    %s %s; /* 注册点求值 */", s.valTypes[i], s.valFields[i])
		}
		if s.recvNode != nil {
			c.line("    %s %s; /* 接收者 */", s.recvType, s.recvField)
		}
		for i := range s.ptrFields {
			c.line("    %s *%s; /* 指向外层局部量 */", s.ptrTypes[i], s.ptrFields[i])
		}
		c.line("};")
		c.line("static void %s(void *ctx) {", s.fnName)
		c.line("    struct %s *c = (struct %s *)ctx;", s.ctxName, s.ctxName)
		for i := range s.valNames {
			c.line("    %s %s = c->%s;", s.valTypes[i], s.valNames[i], s.valFields[i])
		}
		if s.recvNode != nil {
			c.line("    %s %s = c->%s;", s.recvType, s.recvName, s.recvField)
		}
		for i, name := range s.ptrVars {
			c.line("    %s %s = *c->%s;", s.ptrTypes[i], name, s.ptrFields[i])
		}
		c.line("    (void)c;")
		c.srcLine(s.pos)
		if s.block != nil {
			if err := c.emitBody(s.block, false); err != nil {
				return err
			}
			for i, name := range s.ptrVars {
				c.line("    *c->%s = %s;", s.ptrFields[i], name)
			}
		} else {
			saved := c.subst
			c.subst = map[parse.Expr]string{}
			if s.recvNode != nil {
				c.subst[s.recvNode] = s.recvName
			}
			for i, a := range s.valArgs {
				c.subst[a] = s.valNames[i]
			}
			var err error
			if s.isPrint {
				err = c.emitPrint(s.call)
			} else {
				var e string
				if e, err = c.expr(s.call, nil); err == nil {
					c.line("(void)(%s);", e)
				}
			}
			c.subst = saved
			if err != nil {
				return err
			}
		}
		c.line("}")
		c.line("")
	}
	return nil
}

// emitDeferReg 发射一次注册：填上下文 + 按值拷进 defer 栈（emitStmt 的入口）。
func (c *Ctx) emitDeferReg(v *parse.DeferStmt) error {
	st := c.deferCur
	if st == nil {
		return fmt.Errorf("emit: defer appears outside a function (line %d)", v.Pos.Line)
	}
	s, ok := st.byStmt[v]
	if !ok {
		return fmt.Errorf("emit: unregistered defer (line %d)", v.Pos.Line)
	}
	c.srcLine(v.Pos)
	c.line("struct %s %s;", s.ctxName, s.tmpName)
	for i, a := range s.valArgs {
		e, err := c.expr(a, s.valWants[i])
		if err != nil {
			return err
		}
		c.line("%s.%s = %s;", s.tmpName, s.valFields[i], e)
		// §五 R5 的 defer 实参守卫：实参的界必须 ≤ **函数入口**（块内对象经变量中转
		// 作 defer 实参 = 运行时 trap）。检查器按 GuardDefer 标记，此前**发射侧没有
		// 任何消费者** ⇒ 守卫从未发出，defer 在函数退出时读已弹出的区域（实测静默读到
		// 别的对象）。limit = 入口深度 = aic_depth − Delta。
		if g, marked := c.guardOf(a); marked && g.Kind == types.GuardDefer {
			limit := "aic_depth"
			if g.Delta > 0 {
				limit = fmt.Sprintf("(aic_depth - %du)", g.Delta)
			}
			c.line("AIC_GUARD(%s.%s, %s, %s, %d); /* 存储点 defer 实参 */",
				s.tmpName, s.valFields[i], limit, cstr(c.Path), v.Pos.Line)
			c.guards.Kept++
			c.guards.Store++
		}
	}
	if s.recvNode != nil {
		e, err := c.expr(s.recvNode, s.recvWant)
		if err != nil {
			return err
		}
		c.line("%s.%s = %s;", s.tmpName, s.recvField, e)
	}
	for i, name := range s.ptrVars {
		c.line("%s.%s = &%s;", s.tmpName, s.ptrFields[i], name)
	}
	c.line("aic_defer_push_copy(&%s, %s, &%s, sizeof(%s), %s, %d);",
		st.stack, s.fnName, s.tmpName, s.tmpName, cstr(c.Path), v.Pos.Line)
	return nil
}

// emitDeferRun 在函数出口执行全部已注册 defer（LIFO）；无注册点时不发射。
func (c *Ctx) emitDeferRun() {
	if c.deferCur == nil || c.deferCur.stack == "" {
		return
	}
	c.line("AIC_DEFER_RUN(&%s);", c.deferCur.stack)
}

// hasDefers 报告当前函数是否有 defer 注册点（出口需要先算返回值再跑 defer）。
func (c *Ctx) hasDefers() bool {
	return c.deferCur != nil && c.deferCur.stack != ""
}
