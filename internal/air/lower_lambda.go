package air

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 去糖收尾（T2 第四批）：`match` **表达式** 与 `lambda`。
//
//   match 表达式  →  结果槽变量 + `enum.tag` + `switch`，每臂 store 进槽 + br join，
//                    表达式的值 = 该槽（AIR 里变量名即值名）
//   lambda        →  提升为一个静态 Func（模块内新增），表达式的值 = 其符号常量；
//                    纯函数、不捕获（§三）——捕获由源语言禁止，故无环境参数
// ---------------------------------------------------------------------------

// matchExpr：表达式形态的 match（每臂产出一个值）。
func (l *lowerer) matchExpr(v *parse.MatchExpr) (string, error) {
	subj, err := l.value(v.Subject)
	if err != nil {
		return "", err
	}
	en, inst := enumOf(l.typeOf(v.Subject))
	resTy := l.tyOf(v)
	slot := l.tmp("mres")
	l.cur.Insts = append(l.cur.Insts, &Var{Name: slot, Ty: resTy, Loc: LocOf(v.Pos)})

	tag := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: tag, Ty: "i32",
		Rhs: &EnumTag{Val: subj}, Loc: LocOf(v.Pos)})
	join := l.newBlock("mjoin", v.Pos)
	labels := make([]string, 0, len(v.Arms))
	armBlocks := make([]*Block, 0, len(v.Arms))
	for i := range v.Arms {
		b := l.newBlock(fmt.Sprintf("arm%d", i+1), v.Arms[i].Pos)
		armBlocks = append(armBlocks, b)
		labels = append(labels, b.Label)
	}
	l.cur.Term = &Switch{Val: tag, Labels: labels, Loc: LocOf(v.Pos)}

	for i, arm := range v.Arms {
		l.cur = armBlocks[i]
		if err := l.bindArm(en, inst, subj, arm); err != nil {
			return "", err
		}
		val, err := l.value(arm.Value)
		if err != nil {
			return "", err
		}
		l.cur.Insts = append(l.cur.Insts, &Store{
			Place: &VarPlace{Name: slot}, Val: val, Loc: LocOf(arm.Pos)})
		l.cur.Term = &Br{Label: join.Label, Loc: LocOf(arm.Pos)}
	}
	l.cur = join
	return slot, nil
}

// lambdaExpr：把 lambda/闭包提升为模块内的静态函数。
//
// 无捕获 → 表达式的值 = 该符号（纯函数指针）。
// 有捕获（N1）→ 静态函数多一个首形参 `env__`（环境指针），体内对捕获项的引用
// 走 `field env__, <槽>`；表达式的值 = `closure <符号> [<创建点的值名>…]`。
// 捕获是**按值**的：Env 里的名字就是创建点那一刻的值（拷贝语义），
// 之后原变量被改写不影响闭包（锚点 ok/756 冻结这条语义）。
func (l *lowerer) lambdaExpr(v *parse.LambdaExpr) (string, error) {
	ft, _ := l.typeOf(v).(*types.FuncT)
	if ft == nil {
		return "", fmt.Errorf("air: lambda without an expected function type (line %d)", v.Pos.Line)
	}
	caps := l.info.Captures(v)
	l.lamSeq++
	sym := fmt.Sprintf("%s_lambda%d", l.in.Pkg, l.lamSeq)
	f := &Func{Sym: sym, Flags: []string{"static"}, Loc: LocOf(v.Pos)}
	if len(caps) > 0 {
		f.Params = append(f.Params, Param{Name: envParam, Ty: "env(" + sym + ")"})
	}
	for i, p := range ft.Params {
		f.Params = append(f.Params, Param{Name: v.Params[i], Ty: l.ty(p)})
	}
	rets := resultsOfFuncT(ft)
	for _, r := range rets {
		f.Rets = append(f.Rets, l.ty(r))
	}
	entry := l.newBlock("entry", v.Pos)
	f.Blocks = append(f.Blocks, entry)

	saved := l.begin(f, entry, v.Pos)
	savedEnv := l.envCur
	if len(caps) > 0 {
		l.envCur = envParam
	}
	for i, r := range f.Rets {
		entry.Insts = append(entry.Insts, &Var{Name: fmt.Sprintf("ret%d", i), Ty: r, Loc: LocOf(v.Pos)})
	}
	if v.Block != nil {
		if err := l.block(v.Block); err != nil {
			l.envCur = savedEnv
			return "", err
		}
	} else if v.Body != nil {
		val, err := l.value(v.Body)
		if err != nil {
			l.envCur = savedEnv
			return "", err
		}
		if len(f.Rets) > 0 {
			entry.Insts = append(entry.Insts, &Store{
				Place: &VarPlace{Name: "ret0"}, Val: val, Loc: LocOf(v.Pos)})
		}
	}
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: l.exitLabel, Loc: LocOf(v.Pos)}
	}
	l.finish(f, v.Pos)
	l.end(saved)
	l.envCur = savedEnv

	l.mod.Funcs = append(l.mod.Funcs, f)
	if len(caps) == 0 {
		return "const " + sym, nil
	}
	// 环境实参 = 创建点的值名（捕获顺序 = checker 记下的序，确定性）
	names := make([]string, 0, len(caps))
	for _, name := range caps {
		names = append(names, name)
	}
	t := l.tmp("clo")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.ty(ft),
		Rhs: &Closure{Sym: sym, Env: names}, Loc: LocOf(v.Pos)})
	return t, nil
}

// envParam 是闭包环境形参的保留名（`__` 在标识符里被禁用，源码不可能撞名）。
const envParam = "env__"

// resultsOfFuncT 摊平函数类型的返回位（单返回简写 + 多返回列表）。
func resultsOfFuncT(ft *types.FuncT) []types.Type {
	if ft == nil {
		return nil
	}
	if len(ft.Results) > 0 {
		return ft.Results
	}
	if ft.Result != nil {
		return []types.Type{ft.Result}
	}
	return nil
}
