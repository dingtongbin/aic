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

// checkExpr：`check f()` → 取多返回的末位 Err，判 code ≠ 0 则 checkfail。
func (l *lowerer) checkExpr(v *parse.CheckExpr) (string, error) {
	call, ok := v.X.(*parse.Call)
	if !ok {
		return "", fmt.Errorf("air: the check operand must be a call (line %d)", v.Pos.Line)
	}
	res, err := l.value(call)
	if err != nil {
		return "", err
	}
	mt := types.MultiElems(l.typeOf(call))
	if mt == nil {
		return "", fmt.Errorf("air: the check operand does not return multiple values (line %d)", v.Pos.Line)
	}
	last := len(mt) - 1
	// 前置返回位（check 表达式的值 = 末位之前那一项）。仅 Err 的调用没有前置位
	// （`check f()` 只传播、无值），此时给一个字面量占位，避免抽出 Err 自身。
	valT := "const 0"
	if len(mt) > 1 {
		valT = l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: valT, Ty: l.ty(mt[0]),
			Rhs: &MultiExtract{Val: res, Idx: 0}, Loc: LocOf(v.Pos)})
	}
	errT := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: errT, Ty: l.ty(mt[last]),
		Rhs: &MultiExtract{Val: res, Idx: last}, Loc: LocOf(v.Pos)})
	code := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: code, Ty: "i32",
		Rhs: &FieldRHS{Place: &VarPlace{Name: errT}, Idx: 0}, Loc: LocOf(v.Pos)})
	bad := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: bad, Ty: "bool",
		Rhs: &Cmp{Op: "ne", A: code, B: "const 0"}, Loc: LocOf(v.Pos)})

	fail := l.newBlock("checkfail", v.Pos)
	okB := l.newBlock("checkok", v.Pos)
	l.cur.Term = &Cbr{Cond: bad, Then: fail.Label, Else: okB.Label, Loc: LocOf(v.Pos)}

	// 失败路径：传播值 = 前置返回位零值 + 该 err（§六），由收尾块统一 ret。
	l.cur = fail
	zero := "const 0"
	if len(mt) > 1 {
		zero = l.tmp("t")
		fail.Insts = append(fail.Insts, &Let{Tmp: zero, Ty: l.ty(mt[0]),
			Rhs: &Nil{Ty: l.ty(mt[0])}, Loc: LocOf(v.Pos)})
	}
	fail.Term = &CheckFail{Err: errT, Tmp: zero, Loc: LocOf(v.Pos)}

	l.cur = okB
	return valT, nil
}

// matchStmt：主体枚举 → enum.tag + switch + 每臂绑定。
func (l *lowerer) matchStmt(v *parse.MatchStmt) error {
	subj, err := l.value(v.Subject)
	if err != nil {
		return err
	}
	subjTy := l.typeOf(v.Subject)
	en, inst := enumOf(subjTy)
	_ = inst
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
			return err
		}
		switch {
		case arm.Block != nil:
			if err := l.block(arm.Block); err != nil {
				return err
			}
		case arm.Value != nil:
			// **表达式形分支**（`Red => print.println("r")`）：语句位 match 的两种分支
			// 形态（块形 / 表达式形）都必须折进来。曾经只认 Block，表达式形被**整条丢掉**
			// —— 求值有副作用（打印/调用），丢掉就是静默少执行（719 实测：少打一行 "b"）。
			if _, err := l.value(arm.Value); err != nil {
				return err
			}
		}
		if l.cur.Term == nil {
			l.cur.Term = &Br{Label: join.Label, Loc: LocOf(arm.Pos)}
		}
	}
	l.cur = join
	return nil
}

// bindArm 发射一个 match 臂的负载绑定（payload 形态 → enum.payload）。
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

func (l *lowerer) bindArm(en *types.Enum, inst *types.Instance, subj string, arm parse.MatchArm) error {
	for _, p := range arm.Patterns {
		if p.Kind != "payload" || p.Binding == "" || p.Binding == "_" {
			continue
		}
		// 变体名**必须去掉 `Enum.` 限定前缀**（与检查器同一套规则，types.VariantByName）。
		// 曾经直接用 p.Name 查表：`Shape.Circle(r)` 的 `"Shape.Circle"` 查不到 ⇒ **静默跳过
		// 绑定** ⇒ 臂体里引用 r 成了"先用后定"（verifier V2.4 报编译器缺陷；直译路径因为
		// 走检查器解析过的表而正常，于是这个洞只在 IR 路径暴露）。
		idx, va, ok := types.VariantByName(en, p.Name)
		if !ok {
			return fmt.Errorf("air: pattern %s does not name a variant of the matched enum (line %d)", p.Name, p.Pos.Line)
		}
		pl := va.Payload
		if pl == nil {
			// 无负载变体带绑定 = 检查期已拒；走到这里说明类型表与模式不一致。
			return fmt.Errorf("air: pattern %s binds a payload-free variant (line %d)", p.Name, p.Pos.Line)
		}
		if inst != nil && en != nil {
			pl = types.Subst(pl, en.TypeParams, inst.Args) // Option[i32] 的负载 = i32
		}
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.ty(pl),
			Rhs: &EnumPayload{Val: subj, Idx: idx}, Loc: LocOf(p.Pos)})
		l.cur.Insts = append(l.cur.Insts, &Var{Name: p.Binding, Ty: l.ty(pl), Init: t, Loc: LocOf(p.Pos)})
	}
	return nil
}

func (l *lowerer) payloadType(en *types.Enum, variant string) (types.Type, bool) {
	if en == nil {
		return nil, false
	}
	return en.VariantPayload(variant)
}

// rangeForContainer：`for x in xs` / `for i, x in xs` / `for k, v in m` → 下标循环。
func (l *lowerer) rangeForContainer(v *parse.ForStmt) error {
	xs, err := l.value(v.RangeX)
	if err != nil {
		return err
	}
	xt := l.typeOf(v.RangeX)
	if xt == nil {
		// 未定型字面量主体：str 字面量按 str 定（§一 最窄可容纳）
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
	// **map 迭代的名字位不是下标**（§2.5：单名 = 键，两名 = (键, 值)），故内部
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
		// **map 迭代：单名 = 键，两名 = (键, 值)**（§2.5）。键/值按插入序下标取
		// （runtime 的 key_at/val_at）。曾经走通用的 elem 读 ⇒ "element read on
		// the host type map[str]i32 is not covered"（031/700 实测）。
		if isMap {
			if err := l.bindMapIter(xt, xs, idxName, v); err != nil {
				return err
			}
		} else if types.IsStrType(xt) && len(v.Names) == 1 {
			// **str 单名迭代 = 单字节视图（str）**（§2.5：`for ch in s` 打印该字符）；
			// 只有 `for i, b in s` 的两名形态才出 u8 字节。曾经两种都发 u8，
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

// spawnStmt：任务体 = 被调函数（实参在 spawn 点求值，§七）。
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
