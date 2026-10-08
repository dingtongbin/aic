package air

import (
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// N6 `select` 与 `scope.timeout` 的去糖（核心设计 §16 N6；L3 起为**真等待**）。
//
// select 的去糖 = 一条 `select.wait` 指令 + 按下标分派：
//
//	t30 : i32 = select.wait [recv ch1, send ch2] deadline t12   // 运行期 aic_select_wait
//	cbr (t30 == 0), arm0, sel1
//	sel1: cbr (t30 == 1), arm1, sel2
//	sel2: cbr (t30 == -1), armTime, selnone      // -1 = 时间分支到点
//	selnone:                                      // -2 = 被取消：不执行任何分支
//	armI: <绑定 + 块体> → join
//	join: …
//
// 运行期语义（runtime/aic_l2.c 的 aic_select_wait）：
//   - 有臂就绪 → 返回该臂下标（生成代码随即做一次**不会阻塞**的收发）；
//   - 时间臂到点 → -1；
//   - 被取消（scope.timeout / Ctx）→ -2；
//   - 无就绪、无截止点、无取消且**无人能推进** → trap 死锁（位置 = select 点）；
//   - 存在 recv 臂且**已无别的存活任务** → 返回该 recv 臂（其 recv 得 (零值,false)，
//     即 §七【O2】）。
//
// scope.timeout：与 scope 同形（region.enter/exit + 体），额外发一次
// `scope_timeout(ms)` 登记截止点（本任务及其后 spawn 的子任务继承；取消点只在
// AIC 侧等待原语上，C 调用不可取消 —— §15.8 I9），体结束发 `scope_timeout_clear`。
// ---------------------------------------------------------------------------

// selectStmt 把 select 降级成 select.wait + 下标分派。
func (l *lowerer) selectStmt(v *parse.SelectStmt) error {
	if len(v.Arms) == 0 {
		return nil
	}
	join := l.newBlock("seljoin", v.Pos)
	armBlocks := make([]*Block, 0, len(v.Arms))
	for i := range v.Arms {
		armBlocks = append(armBlocks, l.newBlock("selarm", v.Arms[i].Pos))
	}
	deadline := ""
	chans := make([]string, 0, len(v.Arms))
	sends := make([]bool, 0, len(v.Arms))
	idxOf := make([]int, len(v.Arms)) // 源码臂 → 描述符下标（时间臂不占描述符）
	for i := range v.Arms {
		arm := &v.Arms[i]
		switch arm.Kind {
		case "recv", "send":
			call, ok := arm.Expr.(*parse.Call)
			if !ok {
				return errSelectArm
			}
			field, ok := call.Fn.(*parse.Field)
			if !ok {
				return errSelectArm
			}
			chVal, err := l.value(field.X)
			if err != nil {
				return err
			}
			idxOf[i] = len(chans)
			chans = append(chans, chVal)
			sends = append(sends, arm.Kind == "send")
		case "time":
			call, ok := arm.Expr.(*parse.Call)
			if !ok {
				return errSelectArm
			}
			ms, err := l.value(call.Args[0])
			if err != nil {
				return err
			}
			dl := l.tmp("t")
			l.cur.Insts = append(l.cur.Insts, &Let{Tmp: dl, Ty: "i64",
				Rhs: &Call{Sym: "time_after", Args: []string{ms}}, Loc: LocOf(arm.Pos)})
			deadline = dl
		default:
			return errSelectArm
		}
	}

	res := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &SelWait{
		Tmp: res, Chans: chans, Sends: sends, Deadline: deadline, Line: v.Pos.Line, Loc: LocOf(v.Pos),
	})

	// 下标分派：`t30 == k` 逐臂比较（k = 描述符下标；时间臂比 -1；都不中 = 被取消）。
	none := l.newBlock("selnone", v.Pos)
	for i := range v.Arms {
		want := idxOf[i]
		if v.Arms[i].Kind == "time" {
			want = -1
		}
		cmp := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: cmp, Ty: "bool",
			Rhs: &Cmp{Op: "==", A: res, B: "const " + itoa(want)}, Loc: LocOf(v.Arms[i].Pos)})
		next := none.Label
		if i+1 < len(v.Arms) {
			next = l.newBlock("selnext", v.Arms[i].Pos).Label
		}
		l.cur.Term = &Cbr{Cond: cmp, Then: armBlocks[i].Label, Else: next, Loc: LocOf(v.Arms[i].Pos)}
		if i+1 < len(v.Arms) {
			l.cur = l.blockByLabel(next)
		}
	}
	// 都不中 = -2（被取消）：不执行任何分支，直接落到 select 之后。
	l.cur = none
	l.cur.Term = &Br{Label: join.Label, Loc: LocOf(v.Pos)}

	for i := range v.Arms {
		arm := &v.Arms[i]
		l.cur = armBlocks[i]
		if err := l.selectBind(arm); err != nil {
			return err
		}
		if err := l.block(arm.Block); err != nil {
			return err
		}
		if l.cur.Term == nil {
			l.cur.Term = &Br{Label: join.Label, Loc: LocOf(arm.Pos)}
		}
	}
	l.cur = join
	return nil
}

// itoa 是给 IR 文本用的最小整数渲染。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// selectBind 发射一条 recv 分支的绑定（值 / (值, ok)）。
func (l *lowerer) selectBind(arm *parse.SelectArm) error {
	if arm.Kind != "recv" || len(arm.Targets) == 0 {
		return nil
	}
	call, _ := arm.Expr.(*parse.Call)
	field, _ := call.Fn.(*parse.Field)
	chVal, err := l.value(field.X)
	if err != nil {
		return err
	}
	ct := l.typeOf(field.X)
	ch, isCh := ct.(*types.ChanT)
	if !isCh {
		return errSelectArm
	}
	suf, ok := types.RuntimeSuffix(ch.Elem)
	if !ok {
		return errSelectArm
	}
	// recv 的结果是 {_0, _1} 两字结构（与 chan_recv 同源）。
	res := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: res, Ty: "(" + l.ty(ch.Elem) + ", bool)",
		Rhs: &Call{Sym: "chan_recv_" + suf, Args: []string{chVal}}, Loc: LocOf(arm.Pos)})
	for i, tgt := range arm.Targets {
		if tgt.Name == "_" || tgt.Blank {
			continue
		}
		ty := l.ty(ch.Elem)
		if i == 1 {
			ty = "bool"
		}
		v := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: v, Ty: ty,
			Rhs: &MultiExtract{Val: res, Idx: i}, Loc: LocOf(tgt.Pos)})
		l.cur.Insts = append(l.cur.Insts, &Var{Name: tgt.Name, Ty: ty, Init: v, Loc: LocOf(tgt.Pos)})
	}
	return nil
}

// scopeTimeoutStmt 把 `scope.timeout(ms) { … }` 降级成 scope + 截止点登记。
func (l *lowerer) scopeTimeoutStmt(v *parse.ScopeStmt) error {
	ms := "const 0"
	if v.Timeout != nil {
		val, err := l.value(v.Timeout)
		if err != nil {
			return err
		}
		ms = val
	}
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: "void",
		Rhs: &Call{Sym: "scope_timeout", Args: []string{ms}}, Loc: LocOf(v.Pos)})
	return nil
}

// firstRecvArm 取第一条 recv 分支的下标（没有 = -1）。
func firstRecvArm(arms []parse.SelectArm) int {
	for i := range arms {
		if arms[i].Kind == "recv" {
			return i
		}
	}
	return -1
}

// blockByLabel 取已登记块的指针（就绪链要回到"下一臂"的块上继续发指令）。
func (l *lowerer) blockByLabel(label string) *Block {
	for _, b := range l.fn.Blocks {
		if b.Label == label {
			return b
		}
	}
	return l.cur
}
