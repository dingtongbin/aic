package emit

import (
	"fmt"
	"strings"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// for 三形态 (核心设计 §三)、match 语句形、下标存 (§二.2)、多目标赋值 (§六)。
// ---------------------------------------------------------------------------

// emitFor 发射 for: for cond / for init; cond; post / for-in (§三)。
func (c *Ctx) emitFor(v *parse.ForStmt, fnHasErr bool) error {
	c.srcLine(v.Pos)
	c.loopDepth++
	defer func() { c.loopDepth-- }()

	if v.IsRange {
		return c.emitForRange(v, fnHasErr)
	}
	// F5：条件/三式循环体内的容器访问也走缓存形态。
	if used := c.probeIndexUsed(v.Body, fnHasErr); len(used) > 0 {
		c.indexUsed = used
		c.emitCacheDecls(v.Body)
		defer c.endCache()
	}
	// C 形与 cond 形统一走 C 的 for 头 (语义一致: AIC 无 while, 用 for cond)。
	init := ""
	if v.Init != nil {
		s, err := c.forClause(v.Init)
		if err != nil {
			return err
		}
		init = s
	}
	cond := ""
	if v.Cond != nil {
		// F4：`for m <= limit` 的条件给出体内 m 的上界事实（栈式）。
		savedBound := c.loopCondBound
		c.loopCondBound = condBoundOf(v.Cond)
		defer func() { c.loopCondBound = savedBound }()
		e, err := c.expr(v.Cond, types.TBool)
		if err != nil {
			return err
		}
		cond = e
	}
	post := ""
	if v.Post != nil {
		s, err := c.forClause(v.Post)
		if err != nil {
			return err
		}
		post = s
	}
	c.line("for (%s; %s; %s) {", init, cond, post)
	if err := c.emitBody(v.Body, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	return nil
}

// forClause 把 for 的 init/post 子句发射成不带分号的 C 片段。
func (c *Ctx) forClause(s parse.Stmt) (string, error) {
	switch v := s.(type) {
	case *parse.VarDecl:
		if len(v.Targets) != 1 {
			return "", fmt.Errorf("emit: a multi-target declaration is not supported in a for header (line %d)", v.Pos.Line)
		}
		t := c.declType(v)
		tgt := v.Targets[0]
		if v.Init == nil {
			return fmt.Sprintf("%s %s = %s", c.cTypeName(t), tgt.Name, c.zeroValue(t)), nil
		}
		e, err := c.expr(v.Init, t)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s %s = %s", c.cTypeName(t), tgt.Name, e), nil
	case *parse.ExprStmt:
		switch x := v.X.(type) {
		case *parse.Assign:
			t := c.ti(x.LHS)
			rhs, err := c.expr(x.Right, t)
			if err != nil {
				return "", err
			}
			id, ok := x.LHS.(*parse.Ident)
			if !ok {
				return "", fmt.Errorf("emit: a for-header assignment target must be a variable name (line %d)", v.Pos.Line)
			}
			return fmt.Sprintf("%s %s %s", id.Name, x.Op, rhs), nil
		case *parse.Call:
			e, err := c.expr(x, nil)
			if err != nil {
				return "", err
			}
			return e, nil
		}
	}
	return "", fmt.Errorf("emit: unsupported statement form in a for header (line %d)", nodeStmtLine(s))
}

// emitForRange 发射 for-in 三形态 (§二.5):
//
//	for x in xs / for i, x in xs / for i, b in s / for k, v in m / for i in a..b
//
// 迭代序 = 容器的插入序 (§二.2: map/set 恒插入序遍历, H3 确定性)。
func (c *Ctx) emitForRange(v *parse.ForStmt, fnHasErr bool) error {

	// F3/F4 的探测遍（每次区间循环都走一遍）：
	//   F3 = `for i in 0..xs.len()` 且体内不改 xs 的长度/数据指针 → 检查恒真
	//   F4 = 体内每轮恰好一次 append 且无其他变更 → 循环结束后 len(xs) += (hi − lo)
	savedLenBound := c.loopLenBound
	c.loopLenBound = nil
	if len(v.Names) == 1 {
		mutated, appends, perr := c.probeBody(v.Body, fnHasErr)
		if perr != nil {
			return perr
		}
		if xs := lenCallTargetOf(v.RangeEnd); xs != "" {
			c.loopLenBound = &lenBoundFact{induct: v.Names[0].Name, container: xs, clean: !mutated[xs]}
		}
		if v.RangeEnd != nil {
			if iters, ok := loopItersOf(v.RangeX, v.RangeEnd); ok {
				for name, f := range appends {
					if f.appends == 1 && !f.other {
						c.updateLenSymAfterLoop(name, iters)
					}
				}
			}
		}
	}
	defer func() { c.loopLenBound = savedLenBound }()

	// F5（§10.4 第 2 条）：循环体内容器访问的缓存形态（先声明，访问点见访问器分支）。
	if used := c.probeIndexUsed(v.Body, fnHasErr); len(used) > 0 {
		c.indexUsed = used
		c.emitCacheDecls(v.Body)
		defer c.endCache()
	}

	// 归纳变量事实（§10.4 第 12 条②）：仅当上界是字面量时可用于证明。
	savedInduct := c.loopInduct
	c.loopInduct = &inductFact{name: v.Names[0].Name}
	if n, ok := litInt(v.RangeEnd); ok {
		c.loopInduct.hiConst, c.loopInduct.hasHi = n, true
	}
	defer func() { c.loopInduct = savedInduct }()
	// 区间形态: for i in a..b (半开 [a,b); b ≤ a = 零次迭代)
	if v.RangeEnd != nil {
		return c.emitForInterval(v, fnHasErr)
	}
	xt := c.ti(v.RangeX)
	if xt == nil {
		return fmt.Errorf("emit: the iterated type of for-in is unknown (line %d)", v.Pos.Line)
	}
	switch {
	case types.IsSlice(xt):
		return c.emitForSlice(v, xt, fnHasErr)
	case types.IsSet(xt):
		return c.emitForSet(v, xt, fnHasErr)
	case types.IsMap(xt):
		return c.emitForMap(v, xt, fnHasErr)
	case types.IsStrType(xt):
		return c.emitForStr(v, fnHasErr)
	case types.IsArray(xt):
		return c.emitForArray(v, xt, fnHasErr)
	}
	return fmt.Errorf("emit: for-in does not support %s (line %d)", xt.String(), v.Pos.Line)
}

// containerBindings 容器迭代的 (键/下标, 值) 绑定 (§二.5)：
// 单变量 = 值本身（不是下标）；双变量 = (下标/键, 值)。
// 区间形态 `for i in a..b` 的语义不同（i 是计数器），故不走这里 —— 见
// emitForInterval；两种语义混进一个函数就会写出「单变量绑成下标」这类错。
func containerBindings(v *parse.ForStmt) (string, string) {
	names := make([]string, 0, 2)
	for _, n := range v.Names {
		if n.Blank {
			names = append(names, "")
			continue
		}
		names = append(names, n.Name)
	}
	switch len(names) {
	case 0:
		return "", ""
	case 1:
		return "", names[0] // 单变量 = 元素/值
	default:
		return names[0], names[1]
	}
}

// singleName 返回单绑定形态的名字（双绑定 / 无名 / 弃位返回 ""）。
func singleName(v *parse.ForStmt) string {
	if len(v.Names) != 1 || v.Names[0].Blank {
		return ""
	}
	return v.Names[0].Name
}

// intervalName 取区间形态 `for i in a..b` 的计数器名（`_` = 无名计数器）。
func intervalName(v *parse.ForStmt) string {
	for _, n := range v.Names {
		if n.Blank {
			return ""
		}
		return n.Name
	}
	return ""
}

func (c *Ctx) emitForSlice(v *parse.ForStmt, xt types.Type, fnHasErr bool) error {
	elem := types.SliceElem(xt)
	suf, ok := c.containerSuffix(elem)
	if !ok {
		return fmt.Errorf("emit: list element type %s is not instantiated (line %d)", elem.String(), v.Pos.Line)
	}
	recv, err := c.expr(v.RangeX, xt)
	if err != nil {
		return err
	}
	// 被迭代对象先求值一次 (求值顺序: 左到右, §六), 再按索引遍历 = 插入序。
	seq, idx, val := c.tmp("seq"), c.tmp("i"), c.tmp("v")
	c.line("%s *%s = %s;", "aic_list_"+suf, seq, recv)
	c.line("aic_usize %s = 0;", idx)
	k, x := containerBindings(v)
	c.line("for (; %s < aic_list_len_%s(%s); %s++) {", idx, suf, seq, idx)
	c.line("    %s %s = aic_list_get_%s(%s, %s, %s, %d);",
		c.cTypeName(elem), val, suf, seq, idx, cstr(c.Path), v.Pos.Line)
	if k != "" {
		c.line("    aic_usize %s = %s;", k, idx)
	}
	if x != "" {
		c.line("    %s %s = %s;", c.cTypeName(elem), x, val)
	}
	if err := c.emitBody(v.Body, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	return nil
}

func (c *Ctx) emitForSet(v *parse.ForStmt, xt types.Type, fnHasErr bool) error {
	elem := types.SetElem(xt)
	suf, ok := c.containerSuffix(elem)
	if !ok {
		return fmt.Errorf("emit: set element type %s is not instantiated (line %d)", elem.String(), v.Pos.Line)
	}
	recv, err := c.expr(v.RangeX, xt)
	if err != nil {
		return err
	}
	seq, idx := c.tmp("seq"), c.tmp("i")
	c.line("aic_set_%s *%s = %s;", suf, seq, recv)
	c.line("aic_usize %s = 0;", idx)
	k, x := containerBindings(v)
	c.line("for (; %s < aic_set_len_%s(%s); %s++) {", idx, suf, seq, idx)
	if k != "" {
		c.line("    aic_usize %s = %s;", k, idx)
	}
	if x != "" {
		c.line("    %s %s = aic_set_at_%s(%s, %s, %s, %d);",
			c.cTypeName(elem), x, suf, seq, idx, cstr(c.Path), v.Pos.Line)
	}
	if err := c.emitBody(v.Body, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	return nil
}

func (c *Ctx) emitForMap(v *parse.ForStmt, xt types.Type, fnHasErr bool) error {
	key, val, ok := types.MapParts(xt)
	if !ok {
		return fmt.Errorf("emit: the map type shape is unknown (line %d)", v.Pos.Line)
	}
	ks, kok := c.mapKeySuffix(key)
	vs, vok := c.mapValueSuffix(val)
	if !kok || !vok {
		return fmt.Errorf("emit: map[%s]%s is not instantiated (line %d)", key.String(), val.String(), v.Pos.Line)
	}
	recv, err := c.expr(v.RangeX, xt)
	if err != nil {
		return err
	}
	sym := ks + "_" + vs
	seq, idx := c.tmp("seq"), c.tmp("i")
	c.line("aic_map_%s *%s = %s;", sym, seq, recv)
	c.line("aic_usize %s = 0;", idx)
	k, val2 := containerBindings(v)
	if single := singleName(v); single != "" {
		// 单变量 = **键**（语料 031/700 冻结：for k in m 出键）
		k, val2 = single, ""
	}
	c.line("for (; %s < aic_map_len_%s(%s); %s++) {", idx, sym, seq, idx)
	if k != "" {
		c.line("    %s %s = aic_map_key_at_%s(%s, %s, %s, %d);",
			c.cTypeName(key), k, sym, seq, idx, cstr(c.Path), v.Pos.Line)
	}
	if val2 != "" {
		c.line("    %s %s = aic_map_val_at_%s(%s, %s, %s, %d);",
			c.cTypeName(val), val2, sym, seq, idx, cstr(c.Path), v.Pos.Line)
	}
	if err := c.emitBody(v.Body, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	return nil
}

func (c *Ctx) emitForStr(v *parse.ForStmt, fnHasErr bool) error {
	recv, err := c.expr(v.RangeX, types.TStr)
	if err != nil {
		return err
	}
	seq, idx := c.tmp("seq"), c.tmp("i")
	c.line("aic_str %s = %s;", seq, recv)
	c.line("aic_usize %s = 0;", idx)
	k, x := containerBindings(v)
	one := singleName(v)
	c.line("for (; %s < (%s).len; %s++) {", idx, seq, idx)
	switch {
	case one != "":
		// 单变量 = 单字节视图（语料 693 冻结：for ch in "hey" 打印字符）
		c.line("    aic_str %s = (aic_str){ (%s).p + %s, 1 };", one, seq, idx)
	default:
		if k != "" {
			c.line("    aic_usize %s = %s;", k, idx)
		}
		if x != "" {
			// 双变量：第二位是 u8 字节 (§二.5)
			c.line("    aic_u8 %s = (aic_u8)(%s).p[%s];", x, seq, idx)
		}
	}
	if err := c.emitBody(v.Body, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	return nil
}

func (c *Ctx) emitForArray(v *parse.ForStmt, xt types.Type, fnHasErr bool) error {
	elem, n, _ := types.ArrayElem(xt)
	recv, err := c.expr(v.RangeX, xt)
	if err != nil {
		return err
	}
	seq, idx := c.tmp("seq"), c.tmp("i")
	c.line("%s %s = %s;", c.cTypeName(xt), seq, recv)
	c.line("aic_usize %s = 0;", idx)
	k, x := containerBindings(v)
	c.line("for (; %s < %du; %s++) {", idx, n, idx)
	if k != "" {
		c.line("    aic_usize %s = %s;", k, idx)
	}
	if x != "" {
		c.line("    %s %s = %s[%s];", c.cTypeName(elem), x, seq, idx)
	}
	if err := c.emitBody(v.Body, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	return nil
}

// emitForInterval 发射 for i in a..b (半开 [a,b); b ≤ a = 零次迭代, §二.5)。
func (c *Ctx) emitForInterval(v *parse.ForStmt, fnHasErr bool) error {
	lo, err := c.expr(v.RangeX, types.TI64)
	if err != nil {
		return err
	}
	hi, err := c.expr(v.RangeEnd, types.TI64)
	if err != nil {
		return err
	}
	idx, bound := c.tmp("lo"), c.tmp("hi")
	c.line("aic_i64 %s = (aic_i64)(%s);", idx, lo)
	c.line("aic_i64 %s = (aic_i64)(%s);", bound, hi)
	name := intervalName(v)
	if name == "" {
		name = c.tmp("unused")
	}
	c.line("for (aic_i64 %s = %s; %s < %s; %s++) {", name, idx, name, bound, name)
	if err := c.emitBody(v.Body, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	return nil
}

// emitMatchStmt 发射 match 语句形 (强制穷尽、无通配符; 分支可为块, §三)。
func (c *Ctx) emitMatchStmt(v *parse.MatchStmt, fnHasErr bool) error {
	subj, err := c.expr(v.Subject, nil)
	if err != nil {
		return err
	}
	st := c.ti(v.Subject)
	en, hasData := c.enumOf(st)
	if en == nil {
		return fmt.Errorf("emit: the subject of match must be an enum (line %d)", v.Pos.Line)
	}
	tmp := c.tmp("subj")
	c.line("%s %s = %s;", c.cTypeName(st), tmp, subj)
	c.srcLine(v.Pos)
	c.line("switch (%s) {", matchSwitchExpr(tmp, hasData))
	for _, arm := range v.Arms {
		if len(arm.Patterns) == 0 {
			continue
		}
		pat := arm.Patterns[0]
		// 模式可带限定（Color.Red / pkg.Color.Red）：变体名取最后一段
		patName := pat.Name
		if i := strings.LastIndex(patName, "."); i >= 0 {
			patName = patName[i+1:]
		}
		idx, ok := en.VariantIndex(patName)
		if !ok {
			return fmt.Errorf("emit: unknown variant %s (line %d)", pat.Name, arm.Pos.Line)
		}
		c.srcLine(arm.Pos)
		c.line("case %d: {", idx)
		if hasData && pat.Kind == "payload" && pat.Binding != "" && pat.Binding != "_" {
			payload, _ := en.VariantPayload(patName)
			if payload != nil {
				// 泛型实例（Option[i32]）：负载里的 T 按实例实参代换。
				if inst, isInst := st.(*types.Instance); isInst {
					payload = types.Subst(payload, en.TypeParams, inst.Args)
				}
				c.line("    %s %s = %s.u.%s;", c.cTypeName(payload), pat.Binding, tmp, patName)
			}
		}
		if arm.Block != nil {
			if err := c.emitBody(arm.Block, fnHasErr); err != nil {
				return err
			}
		} else if arm.Value != nil {
			e, err := c.expr(arm.Value, nil)
			if err != nil {
				return err
			}
			c.line("    (void)(%s);", e)
		}
		c.line("    break;")
		c.line("}")
	}
	c.line("}")
	return nil
}

// matchSwitchExpr 取 switch 的判别表达式 (带数据 enum 用 .tag)。
func matchSwitchExpr(subj string, hasData bool) string {
	if hasData {
		return subj + ".tag"
	}
	return subj
}

// enumOf 取类型的枚举与其是否带数据。
func (c *Ctx) enumOf(t types.Type) (*types.Enum, bool) {
	if t == nil {
		return nil, false
	}
	if en, ok := types.IsEnum(t); ok {
		return en, en.HasData
	}
	// 泛型实例 (Option[i32]) 的基类型是 enum
	if base, _, ok := types.InstanceParts(t); ok {
		if en, isEnum := types.IsEnum(base); isEnum {
			return en, en.HasData
		}
	}
	return nil, false
}

// emitIndexStore 发射下标存 (存储点 ②; 下标写 i ≤ len = 追加扩容, §二.2)。
func (c *Ctx) emitIndexStore(v *parse.Assign, lhs *parse.Index) error {
	xt := c.ti(lhs.X)
	recv, err := c.expr(lhs.X, xt)
	if err != nil {
		return err
	}
	idx, err := c.expr(lhs.Index, types.TUsize)
	if err != nil {
		return err
	}
	elem := indexElemType(xt)
	rhs, err := c.expr(v.Right, elem)
	if err != nil {
		return err
	}
	// 复合赋值（`xs[i] += v`）：**必须先读回当前元素再算**。
	// 此前直接存 rhs ⇒ `xs[0] += 5` 变成 `xs[0] = 5`（静默算错；AIR 路径是对的，
	// 直译路径漏了这一步 —— 属"静默错误"级缺陷，锚点 ok/776 钉住）。
	if v.Op != "=" && v.Op != "" {
		cur, err := c.indexLoadExpr(xt, recv, idx, elem, v.Pos)
		if err != nil {
			return err
		}
		op := strings.TrimSuffix(v.Op, "=")
		tmp := c.tmp("cmpd")
		c.line("%s %s = (%s %s %s);", c.cTypeName(elem), tmp, cur, op, rhs)
		rhs = tmp
	}
	// 下标写的「已证在界内」判定所需的名字（拿不到名字 → 不证明，保守发带检查形态）
	recvName, idxName := indexNamesOf(v.LHS)
	c.noteIndexAccess(recvName)
	switch {
	case types.IsSlice(xt):
		suf, ok := c.containerSuffix(elem)
		if !ok {
			return fmt.Errorf("emit: list element type %s is not instantiated (line %d)", elem.String(), v.Pos.Line)
		}
		if cache, ok := c.cached[recvName]; ok {
			// F5：缓存形态（热路径只比较 + 直接写；越界走慢路径并刷新缓存）
			c.line("AIC_LIST_SET_CACHED(%s, %s, %s, %s, %s, %d);",
				suf, cache, idx, rhs, cstr(c.Path), v.Pos.Line)
		} else if c.provedInBoundsByName(recvName, idxName) {
			// §10.4 第 12 条②：已证在界内 → unchecked 写
			c.unchecked++
			c.line("aic_list_set_unchecked_%s(%s, %s, %s);", suf, recv, idx, rhs)
		} else {
			c.line("aic_list_set_%s(%s, %s, %s, %s, %d);", suf, recv, idx, rhs, cstr(c.Path), v.Pos.Line)
		}
		return c.afterStore(v.Right, "indexed element", elem, recv)
	case types.IsStrType(xt):
		return fmt.Errorf("emit: str does not support indexed writes (str is immutable, core design §2.3, line %d)", v.Pos.Line)
	default:
		c.line("%s[%s] = %s;", paren("", recv), idx, rhs)
		return c.afterStore(v.Right, "fixed-size array element", elem, "")
	}
}

// indexLoadExpr 发射下标**读**的 C 表达式（复合赋值要先读回当前元素）。
// 与 expr.go 的下标读同源：切片走带检查的 get（越界 = trap），数组直接下标。
func (c *Ctx) indexLoadExpr(xt types.Type, recv, idx string, elem types.Type, pos parse.Pos) (string, error) {
	switch {
	case types.IsSlice(xt):
		suf, ok := c.containerSuffix(elem)
		if !ok {
			return "", fmt.Errorf("emit: list element type %s is not instantiated (line %d)", elem.String(), pos.Line)
		}
		return fmt.Sprintf("aic_list_get_%s(%s, %s, %s, %d)", suf, recv, idx, cstr(c.Path), pos.Line), nil
	case types.IsStrType(xt):
		return "", fmt.Errorf("emit: str does not support indexed writes (str is immutable, core design §2.3, line %d)", pos.Line)
	default:
		return fmt.Sprintf("%s[%s]", paren("", recv), idx), nil
	}
}

// indexElemType 取下标读写的元素类型。
func indexElemType(t types.Type) types.Type {
	if types.IsSlice(t) {
		return types.SliceElem(t)
	}
	if types.IsArray(t) {
		elem, _, _ := types.ArrayElem(t)
		return elem
	}
	if types.IsStrType(t) {
		return types.TU8
	}
	return nil
}

// emitMultiAssign 发射多目标赋值 a, err = f() (§六 绑定规则: 名字必须全部已存在)。
func (c *Ctx) emitMultiAssign(v *parse.MultiAssign) error {
	c.srcLine(v.Pos)
	call, ok := v.Right.(*parse.Call)
	if !ok {
		return fmt.Errorf("emit: the right side of a multi-target assignment must be a call (line %d)", v.Pos.Line)
	}
	mt := types.MultiElems(c.ti(v.Right))
	if mt == nil {
		return fmt.Errorf("emit: the call result shape is unknown (line %d)", v.Pos.Line)
	}
	tmp := c.tmp("multi")
	c.line("%s %s = %s;", c.retStructNameFor(mt), tmp, mustExpr(c.expr(call, nil)))
	for i, lhs := range v.LHS {
		id, ok := lhs.(*parse.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		c.line("%s = %s._%d;", id.Name, tmp, i)
		// 存储点 ⑤（§五 R3）：多返回解构接收 —— 守卫标记挂在**目标名**节点上
		// （检查器 markVarStoreAt），这里按名取用；limit = 当前深度 − Delta。
		if g, marked := c.guardOf(lhs); marked {
			limit := "aic_depth"
			if g.Delta > 0 {
				limit = fmt.Sprintf("(aic_depth - %du)", g.Delta)
			}
			c.line("AIC_GUARD(%s, %s, %s, %d); /* 存储点 ⑤ 多返回接收 */",
				id.Name, limit, cstr(c.Path), v.Pos.Line)
			c.guards.Kept++
			c.guards.Store++
		}
	}
	return nil
}

// nodeStmtLine 取语句所在行。
func nodeStmtLine(s parse.Stmt) int {
	switch v := s.(type) {
	case *parse.VarDecl:
		return v.Pos.Line
	case *parse.ExprStmt:
		return v.Pos.Line
	}
	return 0
}
