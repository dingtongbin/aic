package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// lambda 发射（核心设计 §三：纯 lambda、**不可捕获**、编译为静态函数）。
//
// 形态：每个 lambda 字面量提升为一个静态函数 `aic_lam_<N>`，字面量的值 = 该函数名
// （C 函数指针）。参数名取 a0/a1…（体内只许自有参数、顶层常量与纯函数，故无捕获
// 问题）。表达式体 → `return <expr>;`；块体 → 原样发射块（体内可 return）。
//
// 编号按函数内源序确定（红线 12/H2）。收集走一遍函数体，故定义能发在宿主函数之前。
// ---------------------------------------------------------------------------

// lamSite 是一个 lambda 字面量的发射计划。
type lamSite struct {
	id        int
	node      *parse.LambdaExpr
	name      string
	params    []string
	paramTy   []types.Type
	result    types.Type
	exprBody  parse.Expr
	blockBody *parse.Block
}

// collectLambdas 按源序收集函数体（含嵌套表达式）里的全部 lambda 字面量。
func (c *Ctx) collectLambdas(body *parse.Block) {
	if body == nil {
		return
	}
	var walkExpr func(e parse.Expr)
	var walkStmts func(stmts []parse.Stmt)

	addLamHint := func(lam *parse.LambdaExpr, hint types.Type) {
		if _, done := c.lamSites[lam]; done {
			return
		}
		c.lamSeq++
		s := &lamSite{id: c.lamSeq, node: lam, name: fmt.Sprintf("aic_lam_%d", c.lamSeq)}
		ft, _ := c.ti(lam).(*types.FuncT)
		if ft == nil {
			// 检查器对块体 lambda 可能没记类型：退到声明处的函数类型提示。
			if h, isFn := hint.(*types.FuncT); isFn {
				ft = h
			}
		}
		if ft == nil {
			// 再退一步：从块体里第一个带值的 return 推结果类型。
			if r := firstReturnValue(lam.Block); r != nil {
				rt := c.ti(r)
				if rt == nil {
					rt = c.literalType(r)
				}
				ft = &types.FuncT{Result: rt}
			}
		}
		for i := range lam.Params {
			// C 形参名 = lambda 的**源形参名**（体内按源名引用；lambda 不可捕获，
			// 故不存在与宿主局部量重名的语义风险）。
			s.params = append(s.params, lam.Params[i])
			var pt types.Type
			if ft != nil && i < len(ft.Params) {
				pt = ft.Params[i]
			}
			s.paramTy = append(s.paramTy, pt)
		}
		if ft != nil {
			s.result = ft.Result
		}
		s.exprBody, s.blockBody = lam.Body, lam.Block
		c.lamSites[lam] = s
		c.lams = append(c.lams, s)
	}
	addLam := func(lam *parse.LambdaExpr) { addLamHint(lam, nil) }

	walkExpr = func(e parse.Expr) {
		switch v := e.(type) {
		case nil:
			return
		case *parse.LambdaExpr:
			addLam(v)
			walkExpr(v.Body)
			walkStmts(blockStmts(v.Block))
		case *parse.Unary:
			walkExpr(v.X)
		case *parse.Binary:
			walkExpr(v.Left)
			walkExpr(v.Right)
		case *parse.Assign:
			walkExpr(v.LHS)
			walkExpr(v.Right)
		case *parse.MultiAssign:
			for _, l := range v.LHS {
				walkExpr(l)
			}
			walkExpr(v.Right)
		case *parse.Call:
			walkExpr(v.Fn)
			sig := c.calleeSig(v)
			for i, a := range v.Args {
				// lambda 作实参：被调形参类型就是它的函数类型（与 argsFor 同源）。
				hint := c.paramHintFor(v, sig, i)
				if lam, isLam := a.(*parse.LambdaExpr); isLam {
					addLamHint(lam, hint)
				} else {
					walkExpr(a)
				}
			}
		case *parse.Index:
			walkExpr(v.X)
			walkExpr(v.Index)
			walkExpr(v.End)
		case *parse.Field:
			walkExpr(v.X)
		case *parse.CheckExpr:
			walkExpr(v.X)
		case *parse.ArrayLit:
			for _, x := range v.Elems {
				walkExpr(x)
			}
		case *parse.CompositeLit:
			for _, f := range v.Fields {
				walkExpr(f.Value)
			}
		case *parse.MatchExpr:
			walkExpr(v.Subject)
			for _, arm := range v.Arms {
				walkExpr(arm.Value)
				walkStmts(blockStmts(arm.Block))
			}
		}
	}

	walkStmts = func(stmts []parse.Stmt) {
		for _, s := range stmts {
			switch v := s.(type) {
			case *parse.VarDecl:
				// 声明处的函数类型是块体 lambda 的结果类型来源之一。
				var hint types.Type
				if v.Type != nil {
					hint = c.typeOfTypeExpr(v.Type)
				}
				if lam, isLam := v.Init.(*parse.LambdaExpr); isLam {
					addLamHint(lam, hint)
				} else {
					walkExpr(v.Init)
				}
			case *parse.ExprStmt:
				walkExpr(v.X)
			case *parse.IfStmt:
				walkExpr(v.Cond)
				walkStmts(blockStmts(v.Then))
				walkStmts(blockStmts(v.Else))
			case *parse.ForStmt:
				if v.Init != nil {
					walkStmts([]parse.Stmt{v.Init})
				}
				walkExpr(v.Cond)
				if v.Post != nil {
					walkStmts([]parse.Stmt{v.Post})
				}
				walkExpr(v.RangeX)
				walkExpr(v.RangeEnd)
				walkStmts(blockStmts(v.Body))
			case *parse.ReturnStmt:
				for _, r := range v.Results {
					walkExpr(r)
				}
			case *parse.MatchStmt:
				walkExpr(v.Subject)
				for _, arm := range v.Arms {
					walkExpr(arm.Value)
					walkStmts(blockStmts(arm.Block))
				}
			case *parse.DeferStmt:
				walkExpr(v.Call)
				walkStmts(blockStmts(v.Block))
			case *parse.RegionStmt:
				walkStmts(blockStmts(v.Body))
			case *parse.Block:
				walkStmts(v.Stmts)
			}
		}
	}

	walkStmts(body.Stmts)
}

// paramHintFor 取调用第 i 个实参对应的形参类型（方法调用的 ParamTypes 首位是 this）。
func (c *Ctx) paramHintFor(call *parse.Call, sig *types.FuncSig, i int) types.Type {
	if sig == nil {
		return nil
	}
	off := 0
	if f, isField := call.Fn.(*parse.Field); isField {
		if c.receiverType(f.X) != nil {
			off = 1
		}
	}
	if i+off < len(sig.ParamTypes) {
		return sig.ParamTypes[i+off]
	}
	return nil
}

// firstReturnValue 取块体里第一个带值的 return 表达式（结果类型兜底用）。
func firstReturnValue(b *parse.Block) parse.Expr {
	if b == nil {
		return nil
	}
	for _, s := range b.Stmts {
		switch v := s.(type) {
		case *parse.ReturnStmt:
			if len(v.Results) > 0 {
				return v.Results[0]
			}
		case *parse.IfStmt:
			if r := firstReturnValue(v.Then); r != nil {
				return r
			}
			if r := firstReturnValue(v.Else); r != nil {
				return r
			}
		case *parse.Block:
			if r := firstReturnValue(v); r != nil {
				return r
			}
		}
	}
	return nil
}

// blockStmts 安全取块内语句（nil 块 → nil）。
func blockStmts(b *parse.Block) []parse.Stmt {
	if b == nil {
		return nil
	}
	return b.Stmts
}

// emitLambdaDefs 发射本函数收集到的全部 lambda 静态函数（在宿主函数之前）。
func (c *Ctx) emitLambdaDefs() error {
	if len(c.lams) == 0 {
		return nil
	}
	sites := c.lams
	c.lams = nil // 定义只发一次；后续 lambdaExpr 查 lamSites
	for _, s := range sites {
		ret := "void"
		if s.result != nil {
			ret = c.cTypeName(s.result)
		}
		parts := make([]string, 0, len(s.params))
		for i, p := range s.params {
			ct := "int"
			if s.paramTy[i] != nil {
				ct = c.cTypeName(s.paramTy[i])
			}
			parts = append(parts, ct+" "+p)
		}
		params := "void"
		if len(parts) > 0 {
			params = joinComma(parts)
		}
		c.srcLine(s.node.Pos)
		c.line("static %s %s(%s) {", ret, s.name, params)
		for _, p := range s.params {
			c.line("    (void)%s;", p)
		}
		// 体里的 return 必须按 **lambda 自己的签名** 发射（否则会沿用宿主函数的
		// 返回形状，把 `return a + b` 发成无值 return）。
		savedFn := c.curFunc
		lamSig := &types.FuncSig{Name: s.name, Params: s.params, ParamTypes: s.paramTy}
		if s.result != nil {
			lamSig.Results = []types.Type{s.result}
		}
		c.curFunc = lamSig
		if s.blockBody != nil {
			if err := c.emitBody(s.blockBody, false); err != nil {
				c.curFunc = savedFn
				return err
			}
		} else if s.exprBody != nil {
			e, err := c.expr(s.exprBody, s.result)
			if err != nil {
				c.curFunc = savedFn
				return err
			}
			c.line("    return %s;", e)
		}
		c.curFunc = savedFn
		c.line("}")
		c.line("")
	}
	return nil
}

// lambdaValue 取 lambda 字面量对应的静态函数名（未收集到 = 内部错误）。
func (c *Ctx) lambdaValue(v *parse.LambdaExpr) (string, error) {
	if s, ok := c.lamSites[v]; ok {
		return s.name, nil
	}
	return "", fmt.Errorf("emit: the lambda was not registered during collection (line %d)", v.Pos.Line)
}
