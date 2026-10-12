// 去糖 pass：IR-1 自有原语 → IR-2 低层原语（《AIC 语言规范》§13.3）。
//
// 位置（§13.1 的唯一管线）：
//
//	IR-1（ir_prim.go 的原语）→ 单态化（mono.go）→ **本 pass** → IR-2（ir.go）
//
// 为什么在单态化之后：原语节点里的类型文本（MatchPattern.BindTy、
// CheckRHS.Mt、InterpRHS.ValueTys）含类型形参。单态化在 IR-1 上做文本替换，
// 本 pass 才机械地按已实参化的类型发低层指令。
//
// ## IR-1 原语不变式（lowering 与 desugar 的契约）
//
// 每个含控制流的原语都带 `After` = **lowering 续写块的标签**：
//
//	pre:    <原语节点>            → desugar 把这里换成分发链
//	body…:  臂体 / 处理块          → 其 Term 已由 lowering 设为 Br After
//	after:  lowering 续写的代码   → 执行流汇聚点
//
// desugar 只做三件事：在 pre 里发 tag 取值与分发、把臂 prologue（payload 绑定）
// 插在臂体前、把 ok 分支指向 After。它**从不改写别的块的 Term**，也从不分裂
// 当前块 —— 续写点已由 After 显式给出。这消掉了"块分裂"这一整类错误（分裂要
// 移动 Term 与指令，是 IR pass 里最容易错的地方）。
//
// ## 设计纪律（§13.2 / §13.5：去糖与发射零语义判断）
//
//   - 不读 AST、不读 types.Info：tag 下标、绑定类型、多返回表已由 IR-1 内联；
//   - 每个 IR-2 节点形态由输入唯一决定；
//   - 不改变可观测行为：输入输出确定（§13.4 / H2）。

package air

import "fmt"

// Desugar 把模块里的 IR-1 原语全部编译成 IR-2 低层形态。
// 返回的模块**不含任何 IR-1 原语**（调用方可用 HasPrim 断言）。
func Desugar(m *Module) (*Module, error) {
	out := &Module{Pkg: m.Pkg, Imports: append([]string{}, m.Imports...), Types: m.Types}
	byTy := map[string]*TypeDecl{}
	for _, td := range m.Types {
		byTy[td.Sym] = td
	}
	for _, f := range m.Funcs {
		nf, err := desugarFunc(f, byTy)
		if err != nil {
			return nil, err
		}
		out.Funcs = append(out.Funcs, nf)
	}
	return out, nil
}

// desugarFunc 去糖一个函数体。byTy = 模块的具名类型表（键 = IR 类型文本，
// 如 `ok_Level`）—— 插值降级要按它判"这个值怎么变成 str"（无载荷 enum 走变体名，
// derive ToString 的 struct 走逐字段 ToString；判据只能是类型声明本身）。
func desugarFunc(f *Func, byTy map[string]*TypeDecl) (*Func, error) {
	d := &desugarer{labels: map[string]bool{}, byTy: byTy}
	// 预登记 IR-1 已有的全部块标签：desugar 新建块不得与它们撞名
	//（撞名会让跳转落到错误的块上 —— 这是 IR pass 的静默错误源）。
	for _, b := range f.Blocks {
		if _, dup := d.labels[b.Label]; dup {
			return nil, fmt.Errorf("air: IR-1 has two blocks labelled %q (line %d)", b.Label, b.Loc.Line)
		}
		d.labels[b.Label] = true
	}
	nf := &Func{Sym: f.Sym, Params: f.Params, Rets: f.Rets, Flags: f.Flags, Loc: f.Loc}
	for _, b := range f.Blocks {
		nb, err := d.desugarBlock(b)
		if err != nil {
			return nil, err
		}
		nf.Blocks = append(nf.Blocks, nb...)
	}
	return nf, nil
}

// desugarer 持有去糖期的命名器。
//
// 与 lowerer 的分界：lowerer 面向 AST（typeOf / value 求值），desugarer 只
// 面向已定型 IR（有类型文本）。它**不推断类型**，但按类型文本查类型表是允许的 ——
// 插值降级就需要它（无载荷 enum → 变体名、derive ToString 的 struct → 逐字段
// ToString，判据只能是类型声明本身）。
type desugarer struct {
	tmpN int
	blkN int
	// byTy 是本模块的具名类型表（键 = IR 类型文本，如 `ok_Level`）。
	byTy map[string]*TypeDecl
	// pending 是本条原语展开过程中新建、但**尚未编入输出序列**的块。
	// 为什么需要它：原语展开会建多个块（match 的每臂 prologue、check 的
	// fail/ok…），而 desugarBlock 只拿到原语所在的 pre 块。若 newBlock 不
	// 把块交出来，Switch 的标签会指向**不存在的块** —— 实测 CIR 报
	// "switch arm arm11 not found"，正是这个漏登记。
	pending []*Block
	labels  map[string]bool
}

// newBlock 建一个新块并登记到 pending（由 desugarBlock 编入输出序列）。
// 块名在本函数内唯一（IR-1 已有标签全部预登记）。
func (d *desugarer) newBlock(prefix string, at Loc) *Block {
	if d.labels == nil {
		d.labels = map[string]bool{}
	}
	for {
		d.blkN++
		name := fmt.Sprintf("%s%d", prefix, d.blkN)
		if !d.labels[name] {
			d.labels[name] = true
			b := &Block{Label: name, Loc: at}
			d.pending = append(d.pending, b)
			return b
		}
	}
}

// takePending 取走并清空 pending（调用方把它们编入函数块表）。
func (d *desugarer) takePending() []*Block {
	out := d.pending
	d.pending = nil
	return out
}

func (d *desugarer) tmp(prefix string) string {
	d.tmpN++
	return fmt.Sprintf("%s%d", prefix, d.tmpN)
}

func (d *desugarer) emit(c *Block, in Inst) { c.Insts = append(c.Insts, in) }

// let 发一条 let 并返回临时量名。
//
// 前缀是 `d` 而不是 `t`：lowering 的 tmp 也用 `t<N>` 且**每函数从 1 起**，
// desugarer 若同样发 `t1`，两个序列必然撞名 —— 同一函数里两个不同类型同名的
// let 在 C 层就是 "conflicting types for 't1'"。这是 012/028 实测出来的：
// 撞名的直接后果是 C 编译失败，而 IR 层看不出任何异常。
func (d *desugarer) let(c *Block, ty string, rhs RHS, at Loc) string {
	t := d.tmp("d")
	d.emit(c, &Let{Tmp: t, Ty: ty, Rhs: rhs, Loc: at})
	return t
}

// PrimAfter 返回块尾 IR-1 原语的 After（lowering 续写块标签）；块不以原语
// 收尾时返回 ""。
//
// 这是 desugar 与 verifier 共用的**不变式读取点**（§13.1）：原语的控制流
// 目标只有这一处解析，禁第二份（否则两处迟早分叉，而分叉的表现是可达性
// 误判或跳转漏译 —— 都是静默错误）。
func PrimAfter(b *Block) string {
	if IsPrimTerm(b.Term) {
		if t, ok := b.Term.(*MatchTerm); ok {
			return t.After
		}
	}
	if len(b.Insts) == 0 {
		return ""
	}
	last := b.Insts[len(b.Insts)-1]
	if l, ok := last.(*Let); ok {
		switch r := l.Rhs.(type) {
		case *CheckRHS:
			return r.After
		case *CatchRHS:
			return r.After
		case *MatchExprRHS:
			return r.After
		}
		return ""
	}
	switch in := last.(type) {
	case *PropagateInst:
		return in.After
	case *ForInInst:
		return in.After
	case *ScopeInst:
		return in.After
	case *SelectInst:
		return in.After
	}
	return ""
}

// PrimTargets 返回块尾 IR-1 原语的**全部控制流后继**。
//
// 为什么不能只给 After：match 的臂体块在 lowering 期只被 MatchTerm.Arms
// 引用，而 desugar 建的 Switch 那时还不存在。若按 Term 单独走可达性，臂块
// 会被判成不可达而**被剪掉**（实测：`area` 的三个臂全体消失，CIR 报
// "block mafter1 was never emitted"）。所以 IR-1 的后继 = After ∪ 各臂体。
//
// lowerer 的 pruneUnreachable 与 verifier 的 V2.3 共用本函数（单一来源）。
func PrimTargets(b *Block) []string {
	out := []string{}
	if IsPrimTerm(b.Term) {
		if t, ok := b.Term.(*MatchTerm); ok {
			out = append(out, t.After)
			for _, a := range t.Arms {
				out = append(out, a.Body)
			}
			return out
		}
	}
	if len(b.Insts) == 0 {
		return out
	}
	last := b.Insts[len(b.Insts)-1]
	if l, ok := last.(*Let); ok {
		switch r := l.Rhs.(type) {
		case *CheckRHS:
			return []string{r.After}
		case *CatchRHS:
			return []string{r.Body, r.After}
		case *MatchExprRHS:
			out = append(out, r.After)
			for _, a := range r.Arms {
				out = append(out, a.Body)
			}
			return out
		}
		return out
	}
	switch in := last.(type) {
	case *PropagateInst:
		return []string{in.After}
	case *ForInInst:
		return []string{in.Body, in.After}
	case *ScopeInst:
		return []string{in.Body, in.After}
	case *SelectInst:
		out = append(out, in.After)
		for _, a := range in.Arms {
			out = append(out, a.Body)
		}
		return out
	}
	return out
}

// ---------------------------------------------------------------------------
// 入口：逐块扫描
// ---------------------------------------------------------------------------

func (d *desugarer) desugarBlock(b *Block) ([]*Block, error) {
	prim := false
	for _, in := range b.Insts {
		if l, ok := in.(*Let); ok && IsPrimRHS(l.Rhs) {
			prim = true
			break
		}
		if IsPrimInst(in) {
			prim = true
			break
		}
	}
	if IsPrimTerm(b.Term) {
		prim = true
	}
	if !prim {
		return []*Block{b}, nil // 零开销路径：绝大多数块走这条
	}

	cur := &Block{Label: b.Label, Loc: b.Loc}
	out := []*Block{cur}
	setTerm := func(t Term) {
		if cur.Term == nil {
			cur.Term = t
		}
	}
	for _, in := range b.Insts {
		if l, ok := in.(*Let); ok && IsPrimRHS(l.Rhs) {
			// 原语 let **自身也要发**：desugar 只把它的 Rhs 换成低层形态（TmpRef /
			// VarRef / Const），槽位声明（`let t1 : str = …`）仍由这一条承担。
			// 少发它 = 结果临时量从未声明，C 层报 "'t1' undeclared"（interp/check/
			// catch/assert/match 五族全中，002/028 实测）。
			if err := d.letPrim(cur, l, setTerm); err != nil {
				return nil, err
			}
			d.emit(cur, l)
			out = append(out, d.takePending()...)
			continue
		}
		if IsPrimInst(in) {
			if err := d.instPrim(cur, in, setTerm); err != nil {
				return nil, err
			}
			out = append(out, d.takePending()...)
			continue
		}
		d.emit(cur, in)
	}
	if IsPrimTerm(b.Term) {
		if err := d.termPrim(cur, b.Term, setTerm); err != nil {
			return nil, err
		}
		out = append(out, d.takePending()...)
		return out, nil
	}
	setTerm(b.Term) // 原样搬终结符（目标标签不变）
	return out, nil
}

func (d *desugarer) letPrim(cur *Block, l *Let, setTerm func(Term)) error {
	switch r := l.Rhs.(type) {
	case *AssertRHS:
		return d.desugarAssert(cur, l, r)
	case *CheckRHS:
		return d.desugarCheck(cur, l, r, setTerm)
	case *CatchRHS:
		return d.desugarCatch(cur, l, r, setTerm)
	case *InterpRHS:
		return d.desugarInterp(cur, l, r)
	case *MatchExprRHS:
		return d.desugarMatchExpr(cur, l, r, setTerm)
	}
	return fmt.Errorf("air: desugar sees an unhandled let primitive %T (line %d)", l.Rhs, l.Loc.Line)
}

func (d *desugarer) instPrim(cur *Block, in Inst, setTerm func(Term)) error {
	switch r := in.(type) {
	case *PropagateInst:
		return d.desugarPropagate(cur, r, setTerm)
	}
	return fmt.Errorf("air: desugar sees an unhandled instruction primitive %T (line %d)", in, InstLoc(in).Line)
}

func (d *desugarer) termPrim(cur *Block, t Term, setTerm func(Term)) error {
	switch r := t.(type) {
	case *MatchTerm:
		return d.desugarMatchTerm(cur, r, setTerm)
	}
	return fmt.Errorf("air: desugar sees an unhandled term primitive %T", t)
}

// ---------------------------------------------------------------------------
// assert：expr! → TrapIfErr（§13.3）
//
// TrapIfErr 在 IR-2 是**块内指令**（断言之后控制流继续），不是终结符 ——
// 与 lowering 的 assertExpr 同形。
// ---------------------------------------------------------------------------

func (d *desugarer) desugarAssert(cur *Block, l *Let, r *AssertRHS) error {
	last := len(r.Mt) - 1
	errVal := d.let(cur, r.Mt[last], &MultiExtract{Val: r.Operand, Idx: last}, l.Loc)
	d.emit(cur, &TrapIfErr{Err: errVal, Line: l.Loc.Line, Loc: l.Loc})
	if r.Payload == "" {
		l.Rhs = &Const{Lit: "0"}
	} else {
		l.Rhs = &TmpRef{Name: r.Payload}
	}
	return nil
}

// ---------------------------------------------------------------------------
// propagate：语句位 / return 位自动传播（§5.6）
//
// 与 check 同构：失败 = 前置位零值 + 该 Err，由收尾块统一 ret。
// ---------------------------------------------------------------------------

func (d *desugarer) desugarPropagate(cur *Block, r *PropagateInst, setTerm func(Term)) error {
	last := len(r.Mt) - 1
	errVal := d.let(cur, r.Mt[last], &MultiExtract{Val: r.Operand, Idx: last}, r.Loc)
	code := d.let(cur, "i32", &FieldRHS{Place: &VarPlace{Name: errVal}, Idx: 0}, r.Loc)
	bad := d.let(cur, "bool", &Cmp{Op: "ne", A: code, B: "const 0"}, r.Loc)

	fail := d.newBlock("propfail", r.Loc)
	ok := d.newBlock("propok", r.Loc)
	setTerm(&Cbr{Cond: bad, Then: fail.Label, Else: ok.Label, Loc: r.Loc})

	var zero string
	if len(r.Mt) > 1 {
		zero = d.let(fail, r.Mt[0], &Nil{Ty: r.Mt[0]}, r.Loc)
	} else {
		zero = "const 0"
	}
	fail.Term = &CheckFail{Err: errVal, Tmp: zero, Loc: r.Loc}
	ok.Term = &Br{Label: r.After, Loc: r.Loc}
	return nil
}

// ---------------------------------------------------------------------------
// check：check f() → 条件分支 + 错误返回（§13.3）
// ---------------------------------------------------------------------------

func (d *desugarer) desugarCheck(cur *Block, l *Let, r *CheckRHS, setTerm func(Term)) error {
	last := len(r.Mt) - 1
	payload := r.Payload
	if payload == "" && len(r.Mt) > 1 {
		payload = d.let(cur, r.Mt[0], &MultiExtract{Val: r.Operand, Idx: 0}, l.Loc)
	}
	errVal := d.let(cur, r.Mt[last], &MultiExtract{Val: r.Operand, Idx: last}, l.Loc)
	code := d.let(cur, "i32", &FieldRHS{Place: &VarPlace{Name: errVal}, Idx: 0}, l.Loc)
	bad := d.let(cur, "bool", &Cmp{Op: "ne", A: code, B: "const 0"}, l.Loc)

	fail := d.newBlock("checkfail", l.Loc)
	ok := d.newBlock("checkok", l.Loc)
	setTerm(&Cbr{Cond: bad, Then: fail.Label, Else: ok.Label, Loc: l.Loc})

	var zero string
	if len(r.Mt) > 1 {
		zero = d.let(fail, r.Mt[0], &Nil{Ty: r.Mt[0]}, l.Loc)
	} else {
		zero = "const 0"
	}
	fail.Term = &CheckFail{Err: errVal, Tmp: zero, Loc: l.Loc}
	ok.Term = &Br{Label: r.After, Loc: l.Loc}

	if payload == "" {
		l.Rhs = &Const{Lit: "0"}
	} else {
		l.Rhs = &TmpRef{Name: payload}
	}
	return nil
}

// ---------------------------------------------------------------------------
// catch：f() catch e { } → 条件分支 + 处理块（§13.3）
//
// 结果槽两分支各写一次：ok 写负载，Body 处理块写处理结果，After 合流。
// 无 phi 也满足 V2.5（与返回槽同源）。
// ---------------------------------------------------------------------------

func (d *desugarer) desugarCatch(cur *Block, l *Let, r *CatchRHS, setTerm func(Term)) error {
	last := len(r.Mt) - 1
	payload := d.let(cur, r.Mt[0], &MultiExtract{Val: r.Operand, Idx: 0}, l.Loc)
	errVal := d.let(cur, r.Mt[last], &MultiExtract{Val: r.Operand, Idx: last}, l.Loc)
	code := d.let(cur, "i32", &FieldRHS{Place: &VarPlace{Name: errVal}, Idx: 0}, l.Loc)
	bad := d.let(cur, "bool", &Cmp{Op: "ne", A: code, B: "const 0"}, l.Loc)

	catchB := d.newBlock("catch", l.Loc)
	ok := d.newBlock("catchok", l.Loc)
	setTerm(&Cbr{Cond: bad, Then: catchB.Label, Else: ok.Label, Loc: l.Loc})

	// 处理入口：绑定 Err 后进用户处理块（Body 的 Term 已由 lowering 设成 Br After）。
	if r.Bind != "" && r.Bind != "_" {
		d.emit(catchB, &Var{Name: r.Bind, Ty: r.Mt[last], Init: errVal, Loc: l.Loc})
	}
	catchB.Term = &Br{Label: r.Body, Loc: l.Loc}

	d.emit(ok, &Store{Place: &VarPlace{Name: r.Slot}, Val: payload, Loc: l.Loc})
	ok.Term = &Br{Label: r.After, Loc: l.Loc}
	l.Rhs = &VarRef{Name: r.Slot}
	return nil
}

// ---------------------------------------------------------------------------
// interp：插值 → 拼接 + 逐分量格式化（§13.3）
// ---------------------------------------------------------------------------

func (d *desugarer) desugarInterp(cur *Block, l *Let, r *InterpRHS) error {
	strConst := func(s string) string {
		return d.let(cur, "str", &Const{Lit: "\"" + s + "\""}, l.Loc)
	}
	acc := ""
	append_ := func(part string) {
		if acc == "" {
			acc = part
			return
		}
		acc = d.let(cur, "str", &Binop{Op: "+", A: acc, B: part}, l.Loc)
	}
	if len(r.Parts) > 0 && r.Parts[0] != "" {
		append_(strConst(r.Parts[0]))
	}
	for i, v := range r.Values {
		ty := ""
		if i < len(r.ValueTys) {
			ty = r.ValueTys[i]
		}
		f, err := d.interpFormatOne(cur, v, ty, l.Loc)
		if err != nil {
			return err
		}
		append_(f)
		if i+1 < len(r.Parts) && r.Parts[i+1] != "" {
			append_(strConst(r.Parts[i+1]))
		}
	}
	if acc == "" {
		acc = strConst("")
	}
	l.Rhs = &TmpRef{Name: acc}
	return nil
}

// interpFormatOne 按**类型文本**机械选格式化器（去糖零语义判断）。
func (d *desugarer) interpFormatOne(cur *Block, val, ty string, at Loc) (string, error) {
	switch ty {
	case "str", "":
		return val, nil
	case "bool":
		return d.let(cur, "str", &Call{Sym: "str_fromBool", Args: []string{val}}, at), nil
	case "f64":
		return d.let(cur, "str", &Call{Sym: "str_fromF64", Args: []string{val}}, at), nil
	case "f32":
		w := d.let(cur, "f64", &Conv{Kind: "fpext", A: val}, at)
		return d.let(cur, "str", &Call{Sym: "str_fromF64", Args: []string{w}}, at), nil
	case "i8", "i16", "i32":
		w := d.let(cur, "i64", &Conv{Kind: "sext", A: val}, at)
		return d.let(cur, "str", &Call{Sym: "str_fromI64", Args: []string{w}}, at), nil
	case "i64":
		return d.let(cur, "str", &Call{Sym: "str_fromI64", Args: []string{val}}, at), nil
	case "u8", "u16", "u32":
		w := d.let(cur, "u64", &Conv{Kind: "zext", A: val}, at)
		return d.let(cur, "str", &Call{Sym: "str_fromU64", Args: []string{w}}, at), nil
	case "u64", "usize":
		return d.let(cur, "str", &Call{Sym: "str_fromU64", Args: []string{val}}, at), nil
	case "rune":
		// rune 是独立基本类型（Unicode 标量值）：先按数值定宽，再走 str 的编码
		// 构造（越界在运行期 trap，见 aic_std_str_from_rune）。不能当 u32 插值 ——
		// 那会把中文字符打成一串十进制数字。
		return d.let(cur, "str", &Call{Sym: "str_fromRune", Args: []string{val}}, at), nil
	}
	// 非基本类型（规范 §1.4 的可插值集合后两类）：
	//   - **无载荷 enum**：值就是 tag，文本 = 变体名（`Level.High` → "High"）；
	//   - **derive ToString 的 struct**：文本 = 逐字段拼出的 ToString。
	// 两者的判据都是 IR 类型表的声明（Derives / Variants），不猜。
	if td, ok := d.byTy[ty]; ok {
		if td.Kind == "enum" && payloadFreeEnum(td) {
			return d.let(cur, "str", &Call{Sym: "str_enumName", Args: []string{val}}, at), nil
		}
		if td.Kind == "struct" && deriveHas(td.Derives, "ToString") {
			return d.let(cur, "str", &Call{Sym: "str_ofStruct", Args: []string{val}}, at), nil
		}
	}
	return "", fmt.Errorf("air: interpolation of %s is not mechanisable (line %d): "+
		"a placeholder value must reach IR-1 narrowed to a basic type, a payload-free enum "+
		"or a derive ToString struct (AIC language spec §1.4 / standard library spec §2.1)", ty, at.Line)
}

// payloadFreeEnum 报告一个枚举声明是否无载荷（每个变体的 Payload 都是空）。
func payloadFreeEnum(td *TypeDecl) bool {
	for _, v := range td.Variants {
		if v.Payload != "" {
			return false
		}
	}
	return true
}

// deriveHas 报告 derive 名单里有没有某一项（封闭能力集，判据唯一）。
func deriveHas(list []string, what string) bool {
	for _, d := range list {
		if d == what {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// match：tag 取值 + 分支 + 每臂 payload 绑定（§13.3）
//
// 形状：Arm prologue = payload 绑定 → Br 臂体；Switch 按 tag 分发到 prologue。
// 臂体与 After 的 Term 都由 lowering 设定，desugar 只插分发链。
// ---------------------------------------------------------------------------

func (d *desugarer) desugarMatchTerm(cur *Block, r *MatchTerm, setTerm func(Term)) error {
	tag := d.let(cur, "i32", &EnumTag{Val: r.Subject}, r.Loc)
	pros := d.matchPrologues(r.Arms, r.Subject)
	setTerm(&Switch{Val: tag, Labels: labelsOf(pros), Loc: r.Loc})
	return nil
}

func (d *desugarer) desugarMatchExpr(cur *Block, l *Let, r *MatchExprRHS, setTerm func(Term)) error {
	tag := d.let(cur, "i32", &EnumTag{Val: r.Subject}, r.Loc)
	pros := d.matchPrologues(r.Arms, r.Subject)
	setTerm(&Switch{Val: tag, Labels: labelsOf(pros), Loc: r.Loc})
	l.Rhs = &VarRef{Name: r.Slot}
	return nil
}

// matchPrologues 为每个臂建 prologue 块（payload 绑定 + Br 臂体）。
func (d *desugarer) matchPrologues(arms []MatchArm, subject string) []*Block {
	pros := make([]*Block, len(arms))
	for i, arm := range arms {
		p := d.newBlock(fmt.Sprintf("arm%d", i+1), arm.Loc)
		pros[i] = p
		for _, pat := range arm.Patterns {
			if !pat.HasPay || pat.Binding == "" || pat.Binding == "_" {
				continue // 无绑定：prologue 空，直接穿到臂体
			}
			t := d.let(p, pat.BindTy, &EnumPayload{Val: subject, Idx: pat.Tag}, pat.Loc)
			d.emit(p, &Var{Name: pat.Binding, Ty: pat.BindTy, Init: t, Loc: pat.Loc})
		}
		p.Term = &Br{Label: arm.Body, Loc: arm.Loc}
	}
	return pros
}

func labelsOf(bs []*Block) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Label)
	}
	return out
}
