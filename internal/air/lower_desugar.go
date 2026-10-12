package air

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 去糖补齐（T2 第三批）：`check` / `match`（语句与表达式）/ 容器 `for-in` /
// `scope`+`spawn` / 多目标绑定。全部按 §10.1 的结构落成 AIR：
//   - `check` → 判 err.code ≠ 0 → **checkfail**（终结符）→ 唯一收尾块；
//   - `match` → `enum.tag` + `switch` + 每臂 `enum.payload` 绑定；
//   - 容器 `for-in` → 下标循环（`len` + `elem`；map 走 keyAt/valAt）；
//   - `scope/spawn` → region.enter/exit + 逐 spawn 一条 `call`（T1 串行 L2）。
// ---------------------------------------------------------------------------

// matchStmt：语句位 match → **IR-1 原语** MatchTerm（§13.2）。
// subject 与臂体块由 lowering 建好，分发链由 desugar 补。
func (l *lowerer) matchStmt(v *parse.MatchStmt) error {
	subj, en, inst, err := l.matchSubject(v.Subject)
	if err != nil {
		return err
	}
	after := l.newBlock("mafter", v.Pos)
	arms, err := l.matchArms(v.Arms, en, inst, "", after)
	if err != nil {
		return err
	}
	// MatchTerm 是**终结符**（ir_prim.go）：pre 块的 Term 即原语本身，
	// desugar 把它换成 Br dispatch + Switch（PrimAfter 返回 After 供可达性走）。
	l.cur.Term = &MatchTerm{
		Subject: subj, SubjTy: l.ty(l.typeOf(v.Subject)), Arms: arms,
		After: after.Label, Loc: LocOf(v.Pos),
	}
	l.cur = after
	return nil
}

// enumOf 解包枚举类型（泛型实例 → 基枚举 + 实参）。
func enumOf(t types.Type) (*types.Enum, *types.Instance) {
	if en, ok := types.IsEnum(t); ok {
		return en, nil
	}
	if inst, ok := t.(*types.Instance); ok {
		if en, ok2 := types.IsEnum(inst.Base); ok2 {
			return en, inst
		}
	}
	return nil, nil
}

// rangeForContainer：`for x in xs` / `for i, x in xs` / `for k, v in m` → 下标循环。
func (l *lowerer) rangeForContainer(v *parse.ForStmt) error {
	xs, err := l.value(v.RangeX)
	if err != nil {
		return err
	}
	xt := l.typeOf(v.RangeX)
	if xt == nil {
		// 未定型字面量主体：str 字面量按 str 定（最窄可容纳）
		if _, isStr := v.RangeX.(*parse.StrLit); isStr {
			xt = types.TStr
		}
	}
	if xt == nil {
		return fmt.Errorf("air: for-in subject type unknown (line %d)", v.Pos.Line)
	}
	// 长度：列表/集合/数组/str 用 len；map 用 len（方法）
	n := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: n, Ty: "usize",
		Rhs: &LenRHS{Place: &VarPlace{Name: xs}}, Loc: LocOf(v.Pos)})
	// **map 迭代的名字位不是下标**（单名 = 键，两名 = (键, 值)），故内部
	// 下标变量一律用 i__；其他容器 Names[0] 才是使用者写的下标名。
	// 索引名只在**两名形态**才对用户可见（`for i, x in xs` 的 i）⇒ 只有那时要
	// 别名；单名形态下源名指的是元素，索引是隐形的（D1 的重命名不该把体内的
	// 元素引用改到索引上）。
	isMap := types.IsMap(xt)
	idxVisible := !isMap && len(v.Names) >= 2
	idxName := "i__"
	if !isMap && len(v.Names) >= 1 && v.Names[0].Name != "_" {
		idxName = l.loopVarName(v.Names[0].Name, idxVisible)
	}
	l.cur.Insts = append(l.cur.Insts, &Var{Name: idxName, Ty: "usize", Init: "const 0", Loc: LocOf(v.Pos)})

	head := l.newBlock("forhead", v.Pos)
	body := l.newBlock("forbody", v.Pos)
	post := l.newBlock("forpost", v.Pos)
	done := l.newBlock("fordone", v.Pos)
	l.cur.Term = &Br{Label: head.Label, Loc: LocOf(v.Pos)}

	l.cur = head
	c := l.tmp("t")
	head.Insts = append(head.Insts, &Let{Tmp: c, Ty: "bool",
		Rhs: &Cmp{Op: "lt", A: idxName, B: n}, Loc: LocOf(v.Pos)})
	head.Term = &Cbr{Cond: c, Then: body.Label, Else: done.Label, Loc: LocOf(v.Pos)}

	l.cur = body
	// 元素绑定：单名 = 元素；两名 = (下标, 元素)
	elemIdx := 0
	if len(v.Names) == 1 {
		elemIdx = 0
	} else if len(v.Names) >= 2 {
		elemIdx = 1
	}
	elemName := ""
	if len(v.Names) > 0 && v.Names[elemIdx].Name != "_" {
		elemName = v.Names[elemIdx].Name
		// **map 迭代：单名 = 键，两名 = (键, 值)**。键/值按插入序下标取
		// （runtime 的 key_at/val_at）。走通用的 elem 读会报 "element read on
		// the host type map[str]i32 is not covered"（031/700 实测）。
		if isMap {
			if err := l.bindMapIter(xt, xs, idxName, v); err != nil {
				return err
			}
		} else if types.IsStrType(xt) && len(v.Names) == 1 {
			// **str 单名迭代 = 单字节视图（str）**（`for ch in s` 打印该字符）；
			// 只有 `for i, b in s` 的两名形态才出 u8 字节；两种都发 u8 会让
			// `println(ch)` 打出 104/101/121（693 实测）。
			elemName = l.loopVarName(elemName, true)
			hi := l.tmp("t")
			l.cur.Insts = append(l.cur.Insts, &Let{Tmp: hi, Ty: "usize",
				Rhs: &Binop{Op: "+", A: idxName, B: "const 1"}, Loc: LocOf(v.Pos)})
			el := l.tmp("t")
			l.cur.Insts = append(l.cur.Insts, &Let{Tmp: el, Ty: "str",
				Rhs: &StrViewRHS{Base: xs, Lo: idxName, Hi: hi}, Loc: LocOf(v.Pos)})
			l.cur.Insts = append(l.cur.Insts, &Var{Name: elemName,
				Ty: "str", Init: el, Loc: LocOf(v.Names[0].Pos)})
		} else {
			el := l.tmp("t")
			l.cur.Insts = append(l.cur.Insts, &Let{Tmp: el, Ty: l.elemTypeText(xt),
				Rhs: &ElemRHS{Place: &VarPlace{Name: xs}, Idx: idxName}, Loc: LocOf(v.Pos)})
			elemName = l.loopVarName(elemName, true)
			l.cur.Insts = append(l.cur.Insts, &Var{Name: elemName,
				Ty: l.elemTypeText(xt), Init: el, Loc: LocOf(v.Names[elemIdx].Pos)})
		}
	}
	l.loops = append(l.loops, loopCtx{post: post.Label, done: done.Label})
	if err := l.block(v.Body); err != nil {
		return err
	}
	l.loops = l.loops[:len(l.loops)-1]
	// 循环体降级结束：撤掉本层循环的别名（源名指回外层变量）。
	if idxVisible {
		l.unaliasLoopVar(v.Names[0].Name, idxName)
	}
	if elemName != "" {
		l.unaliasLoopVar(v.Names[elemIdx].Name, elemName)
	}
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: post.Label, Loc: LocOf(v.Pos)}
	}
	l.cur = post
	nx := l.tmp("t")
	post.Insts = append(post.Insts, &Let{Tmp: nx, Ty: "usize",
		Rhs: &Binop{Op: "+", A: idxName, B: "const 1"}, Loc: LocOf(v.Pos)})
	post.Insts = append(post.Insts, &Store{Place: &VarPlace{Name: idxName}, Val: nx, Loc: LocOf(v.Pos)})
	post.Term = &Br{Label: head.Label, Loc: LocOf(v.Pos)}
	l.cur = done
	return nil
}

// bindMapIter 绑定 map 迭代的名字位：单名 = 键；两名 = (键, 值)。
// 取用走 runtime 的 key_at/val_at（按插入序），符号拼写与容器方法调用同源
// （`recvTypeSym + "_" + 方法名`，emit 侧 irMapCall 只认这一张表）。
func (l *lowerer) bindMapIter(xt types.Type, xs, idxName string, v *parse.ForStmt) error {
	k, val, _ := types.MapParts(xt)
	sym := recvTypeSym(xt)
	key := ""
	if v.Names[0].Name != "_" {
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.ty(k),
			Rhs: &Call{Sym: sym + "_keyAt", Args: []string{xs, idxName}}, Loc: LocOf(v.Names[0].Pos)})
		key = l.loopVarName(v.Names[0].Name, true)
		l.cur.Insts = append(l.cur.Insts, &Var{Name: key, Ty: l.ty(k), Init: t,
			Loc: LocOf(v.Names[0].Pos)})
	}
	if len(v.Names) >= 2 && v.Names[1].Name != "_" {
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.ty(val),
			Rhs: &Call{Sym: sym + "_valAt", Args: []string{xs, idxName}}, Loc: LocOf(v.Names[1].Pos)})
		valName := l.loopVarName(v.Names[1].Name, true)
		l.cur.Insts = append(l.cur.Insts, &Var{Name: valName, Ty: l.ty(val), Init: t,
			Loc: LocOf(v.Names[1].Pos)})
	}
	_ = key
	return nil
}

// elemTypeText 取容器元素类型的 AIR 文本。
func (l *lowerer) elemTypeText(t types.Type) string {
	switch {
	case types.IsSlice(t):
		return l.ty(types.SliceElem(t))
	case types.IsSet(t):
		return l.ty(types.SetElem(t))
	case types.IsMap(t):
		_, v, _ := types.MapParts(t)
		return l.ty(v)
	case types.IsArray(t):
		e, _, _ := types.ArrayElem(t)
		return l.ty(e)
	case types.IsStrType(t):
		return "u8"
	}
	return "i32"
}

// scopeStmt：`scope { }` = 任务域（不是内存 region）：RegionEnter/Exit 带 Scope 标记，
// 退出时 join 全部 spawn 的任务（L3）。块内 spawn 各发一条 Spawn 指令。
func (l *lowerer) scopeStmt(v *parse.ScopeStmt) error {
	l.depth++
	l.cur.Insts = append(l.cur.Insts, &RegionEnter{TypeID: l.tmp("sc"), Scope: true, Loc: LocOf(v.Pos)})
	if err := l.block(v.Body); err != nil {
		return err
	}
	l.depth--
	l.cur.Insts = append(l.cur.Insts, &RegionExit{Scope: true, Loc: LocOf(v.Pos)})
	return nil
}

// spawnStmt：任务体 = 被调函数（实参在 spawn 点求值）。
// L3 真并发：**不内联执行**，发 Spawn 指令（后端 = 上下文块 + thunk + 调度入口）。
// T1 串行期这里直接发 call —— 那个形态会让两个互相等待的任务"看起来"跑完，
// 死锁永不触发（773 实测）。
func (l *lowerer) spawnStmt(v *parse.SpawnStmt) error {
	call, ok := v.Call.(*parse.Call)
	if !ok {
		return fmt.Errorf("air: spawn needs a call (line %d)", v.Pos.Line)
	}
	sym, ta := l.callee(call)
	if sym == "" || sym == "unknown" {
		return fmt.Errorf("air: cannot resolve the spawned call target (line %d)", v.Pos.Line)
	}
	vals := []string{}
	// 方法形态：接收者是首实参（与直调 / defer 同一条规则）。
	if f, isField := call.Fn.(*parse.Field); isField && l.methodRecv(f.X) {
		rv, err := l.value(f.X)
		if err != nil {
			return err
		}
		vals = append(vals, rv)
	}
	for _, a := range call.Args {
		val, err := l.deferVal(a)
		if err != nil {
			return err
		}
		vals = append(vals, val)
	}
	l.cur.Insts = append(l.cur.Insts, &Spawn{Callee: sym, TypeArgs: ta, Vals: vals, Loc: LocOf(v.Pos)})
	return nil
}

// multiBind：`var a, err = f()` → call + 逐位 multi.extract + var 绑定。
func (l *lowerer) multiBind(v *parse.VarDecl) error {
	res, err := l.value(v.Init)
	if err != nil {
		return err
	}
	mt := types.MultiElems(l.typeOf(v.Init))
	for i, tgt := range v.Targets {
		ty := "i32"
		if mt != nil && i < len(mt) {
			ty = l.ty(mt[i])
		}
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: ty,
			Rhs: &MultiExtract{Val: res, Idx: i}, Loc: LocOf(v.Pos)})
		if tgt.Name == "_" || tgt.Blank {
			continue
		}
		l.cur.Insts = append(l.cur.Insts, &Var{Name: tgt.Name, Ty: ty, Init: t, Loc: LocOf(tgt.Pos)})
		l.noteBlockVar(tgt.Name)
	}
	return nil
}
