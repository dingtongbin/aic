package air

import (
	"fmt"
	"sort"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// `defer { … }` / `errdefer { … }` 块形（035/713 锚点）。
//
// 语义（核心设计 §三回滚模式）：块在**函数出口**跑（LIFO，与调用形同一个栈），
// 块内对外层局部量的读写必须是**按引用**的 —— `var failed bool = false` 之后
// `failed = true`，出口的块要读到 true（否则整个 errdefer 模式失去意义）。
//
// 降级形状（IR 先无损装下，R14）：
//  ① 块体提升为一个静态 Func `<pkg>_dfblk<N>`，每个被捕获的局部量一个
//     **按引用形参**（Param.Ref）；
//  ② 注册点发 `DeferInit{Callee: <提升体>, Vals: [&cap…]}` —— 值形态是 AddrRHS
//     （取地址），于是出口读到的是变量槽的最终值；
//  ③ 后端只需认识已有的 DeferInit 机制（上下文结构体 + trampoline + 出口跑栈）。
//
// 捕获集 = 块内引用的、**属于所在函数**的局部/形参（检查器的 Locals 表是唯一
// 事实来源，不在这儿第二套作用域分析），剔除块内自己声明的名字。
// ---------------------------------------------------------------------------

// capture 是一个按引用捕获的外层局部量。
type capture struct {
	name string
	ty   string
}

// deferBlockCaptures 取块形 defer 的捕获表，按名排序（H8：.air 文本禁 map 序）。
// 空表 = 块不碰外层局部量（纯清理，提升体无参）。
func (l *lowerer) deferBlockCaptures(b *parse.Block) []capture {
	if l.info == nil || l.fnKey == "" || b == nil {
		return nil
	}
	locals := l.info.Locals[l.fnKey]
	if len(locals) == 0 {
		return nil
	}
	// 块内自己声明的名字挡住外层同名量（不是捕获）。
	shadowed := map[string]bool{}
	collectBlockDecls(b, shadowed)
	seen := map[string]bool{}
	var out []capture
	var walkExpr func(e parse.Expr)
	var walkStmts func(stmts []parse.Stmt)

	walkExpr = func(e parse.Expr) {
		switch v := e.(type) {
		case nil:
			return
		case *parse.Ident:
			if !locals[v.Name] || shadowed[v.Name] || seen[v.Name] {
				return
			}
			t := l.info.LookupType(v)
			if t == nil {
				return // 未定型引用：宁可不捕获也不猜类型
			}
			seen[v.Name] = true
			out = append(out, capture{name: v.Name, ty: l.ty(t)})
		case *parse.Field:
			walkExpr(v.X)
		case *parse.Index:
			walkExpr(v.X)
			walkExpr(v.Index)
			walkExpr(v.End)
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
			for _, a := range v.Args {
				walkExpr(a)
			}
		case *parse.CheckExpr:
			walkExpr(v.X)
		case *parse.OrExpr:
			walkExpr(v.X)
			walkExpr(v.Default)
		case *parse.CatchExpr:
			walkExpr(v.X)
			walkStmts(blockStmts(v.Block))
		case *parse.AssertExpr:
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
		case *parse.InterpLit:
			for _, x := range v.Values {
				walkExpr(x)
			}
		}
	}

	walkStmts = func(stmts []parse.Stmt) {
		for _, s := range stmts {
			switch v := s.(type) {
			case *parse.VarDecl:
				for _, tgt := range v.Targets {
					if tgt.Name != "_" {
						shadowed[tgt.Name] = true
					}
				}
				walkExpr(v.Init)
			case *parse.ExprStmt:
				walkExpr(v.X)
			case *parse.IfStmt:
				walkExpr(v.Cond)
				walkStmts(blockStmts(v.Then))
				walkStmts(blockStmts(v.Else))
			case *parse.ForStmt:
				walkExpr(v.RangeX)
				walkExpr(v.RangeEnd)
				for _, n := range v.Names {
					if n.Name != "_" {
						shadowed[n.Name] = true
					}
				}
				walkStmts(blockStmts(v.Body))
			case *parse.ReturnStmt:
				for _, r := range v.Results {
					walkExpr(r)
				}
			case *parse.MatchStmt:
				walkExpr(v.Subject)
				for _, arm := range v.Arms {
					walkStmts(blockStmts(arm.Block))
				}
			case *parse.Block:
				walkStmts(v.Stmts)
			}
		}
	}

	walkStmts(b.Stmts)
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// collectBlockDecls 收块内声明的名字（var / for-in 绑定）——它们挡住外层同名量。
func collectBlockDecls(b *parse.Block, out map[string]bool) {
	if b == nil {
		return
	}
	var stmts func([]parse.Stmt)
	stmts = func(ls []parse.Stmt) {
		for _, s := range ls {
			switch v := s.(type) {
			case *parse.VarDecl:
				for _, tgt := range v.Targets {
					if tgt.Name != "_" {
						out[tgt.Name] = true
					}
				}
			case *parse.IfStmt:
				stmts(blockStmts(v.Then))
				stmts(blockStmts(v.Else))
			case *parse.ForStmt:
				for _, n := range v.Names {
					if n.Name != "_" {
						out[n.Name] = true
					}
				}
				stmts(blockStmts(v.Body))
			case *parse.MatchStmt:
				for _, arm := range v.Arms {
					stmts(blockStmts(arm.Block))
				}
			case *parse.Block:
				stmts(v.Stmts)
			}
		}
	}
	stmts(b.Stmts)
}

// blockStmts 安全取块内语句（nil 块 → nil）。air 包内多个走查器共用。
func blockStmts(b *parse.Block) []parse.Stmt {
	if b == nil {
		return nil
	}
	return b.Stmts
}

// deferBlockStmt 降级块形 defer/errdefer：提升体 + 注册点（见文件头）。
func (l *lowerer) deferBlockStmt(v *parse.DeferStmt, onErr bool) error {
	caps := l.deferBlockCaptures(v.Block)
	// deferSeq 是本函数的 defer 计数（finish 靠它决定收尾块发不发 DeferRun）；
	// dfblkSeq 是**模块级**提升体编号（deferSeq 每函数重置，提升体符号跨函数
	// 唯一 ⇒ 两个计数器各管一件事，035 实测：只换 dfblkSeq 会让 defer 栈根本不跑）。
	l.deferSeq++
	l.dfblkSeq++
	id := l.deferSeq
	sym := fmt.Sprintf("%s_dfblk%d", l.in.Pkg, l.dfblkSeq)
	f := &Func{Sym: sym, Flags: []string{"static"}, Loc: LocOf(v.Pos)}
	for _, cp := range caps {
		f.Params = append(f.Params, Param{Name: cp.name, Ty: cp.ty, Ref: true})
	}
	entry := l.newBlock("entry", v.Pos)
	f.Blocks = append(f.Blocks, entry)
	saved := l.begin(f, entry, v.Pos)
	if err := l.block(v.Block); err != nil {
		l.end(saved)
		return err
	}
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: l.exitLabel, Loc: LocOf(v.Pos)}
	}
	l.finish(f, v.Pos)
	l.end(saved)
	l.mod.Funcs = append(l.mod.Funcs, f)

	// 注册点：每个捕获量取一次地址（值形态 = AddrRHS；类型 = 指向的类型的指针文本）。
	vals := make([]string, 0, len(caps))
	limits := make([]string, 0, len(caps))
	for _, cp := range caps {
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: cp.ty + "*",
			Rhs: &AddrRHS{Val: cp.name}, Loc: LocOf(v.Pos)})
		vals = append(vals, t)
		limits = append(limits, "")
	}
	if onErr {
		l.deferErr = true
	}
	l.cur.Insts = append(l.cur.Insts, &DeferReg{OnErr: onErr, Loc: LocOf(v.Pos)})
	l.cur.Insts = append(l.cur.Insts, &DeferInit{
		ID: id, Callee: sym, Vals: vals, Limits: limits, OnErr: onErr, Loc: LocOf(v.Pos)})
	return nil
}
