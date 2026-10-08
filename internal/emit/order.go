package emit

import (
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// C15 / L18：求值顺序显式化（发射层）。
//
// C 标准**不规定**函数实参、二元操作数与初始化列表的求值顺序（本机 gcc 实测为
// 右→左），因此只要一条 C 语句里出现两个可观察副作用的子表达式，程序语义就取决于
// 编译器实现 —— 五配置（tcc/gcc/clang）之间必然分叉。
//
// 对策：把带副作用的操作数**按源码序绑到临时量**，求值顺序由语句序编码：
//
//	f(g(), h())   →  aic_i32 t1 = g(); aic_i32 t2 = h(); f(t1, t2)
//	g() + h()     →  aic_i32 t1 = g(); aic_i32 t2 = h(); (t1 + t2)
//	xs[f()]       →  aic_usize t1 = f(); aic_list_get_xs(xs, t1, …)
//
// 判据（H10 扩展）：产物中一条 C 语句内不得含两个带副作用的调用。
// 例外：`&&` / `||` **必须短路**，其操作数一律不提升（提升会改变语义）。
// ---------------------------------------------------------------------------

// sideEffect 报告一个表达式的求值是否可能产生可观察副作用（调用 / 分配 / 控制流）。
// 保守不等于"一律提升"：把纯表达式也提升会白白膨胀产物，故这里只把真正会发射
// 调用或控制流的形态判为真，其余按结构递归。
func (c *Ctx) sideEffect(e parse.Expr) bool { return c.sideEffectAt(e, 0) }

func (c *Ctx) sideEffectAt(e parse.Expr, depth int) bool {
	if e == nil || depth > 64 {
		return false
	}
	switch v := e.(type) {
	case *parse.Call:
		return true
	case *parse.CheckExpr:
		return true // 传播 = 控制流
	case *parse.MatchExpr:
		return true // 发射 switch + 结果槽（语句）
	case *parse.ArrayLit:
		return true // 列表字面量发射 aic_list_new/append（语句）
	case *parse.CompositeLit:
		// 引用对象字面量发射 alloc + 逐字段 store；@packed 是纯初始化列表，
		// 只有字段值可能带副作用。
		if cl, _ := c.compositeClassOf(v.TypeName); cl != nil && cl.Packed {
			for _, f := range v.Fields {
				if c.sideEffectAt(f.Value, depth+1) {
					return true
				}
			}
			return false
		}
		return true
	case *parse.Binary:
		return c.sideEffectAt(v.Left, depth+1) || c.sideEffectAt(v.Right, depth+1)
	case *parse.Unary:
		return c.sideEffectAt(v.X, depth+1)
	case *parse.Index:
		return c.sideEffectAt(v.X, depth+1) || c.sideEffectAt(v.Index, depth+1) ||
			c.sideEffectAt(v.End, depth+1)
	case *parse.Field:
		return c.sideEffectAt(v.X, depth+1)
	case *parse.Assign, *parse.MultiAssign:
		return true
	}
	return false
}

// hoist 把一个**已发射**的操作数文本绑到临时量（求值顺序显式化的落点）。
// 类型未知时原样返回：检查器已定型，正常编译到不了这一支。
func (c *Ctx) hoist(e parse.Expr, want types.Type, txt string) string {
	t := want
	if t == nil {
		t = c.ti(e)
	}
	if t == nil {
		return txt
	}
	tmp := c.tmp("ord")
	c.line("%s = %s;", c.cTypeDecl(t, tmp), txt)
	return tmp
}

// orderedOperand 发射一个操作数；hoist 为真且该操作数带副作用时先绑临时量。
func (c *Ctx) orderedOperand(e parse.Expr, want types.Type, hoist bool) (string, error) {
	txt, err := c.expr(e, want)
	if err != nil {
		return "", err
	}
	if hoist && c.sideEffect(e) {
		return c.hoist(e, want, txt), nil
	}
	return txt, nil
}

// orderedList 发射一串操作数（实参表 / 初始化列表）：两个以上操作数时按源码序
// 提升带副作用者 —— 一条 C 语句里不再有两个未定序的副作用点。
func (c *Ctx) orderedList(exprs []parse.Expr, wants []types.Type) ([]string, error) {
	hoist := len(exprs) > 1
	out := make([]string, 0, len(exprs))
	for i, e := range exprs {
		var want types.Type
		if i < len(wants) {
			want = wants[i]
		}
		txt, err := c.orderedOperand(e, want, hoist)
		if err != nil {
			return nil, err
		}
		out = append(out, txt)
	}
	return out, nil
}
