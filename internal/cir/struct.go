package cir

import (
	"fmt"
	"strings"

	"aic/internal/air"
)

// ---------------------------------------------------------------------------
// air (mair) → cir：把 CFG 折成结构化语句，**折不出来就发 goto，绝不丢边**。
//
// 本轮（P1，2026-10）重定纪律。用户直令：**IR 先无损装下全部语法，"理想 C"是
// P3 的事**。据此本层只有一条硬要求：
//
//	CFG 的每一条边都必须在产物里被**物化**成五者之一：
//	  fallthrough（文字顺序上紧接着）｜goto｜return｜break｜continue。
//	少一条 = **编译失败**（红线 23），不是"发个错的先跑起来"。
//
// 为什么这条要求是决定性的：旧的折叠器只做"数数自检"（cbr 数 == if 数、
// ret 数 == Return 数），于是**边被丢掉也照样通过**。791 实测：`ok_load` 的
// `join6 → br exit` 被丢（`exit` 已被第一个到达它的分支折走），生成的多返回
// 函数在 else 路径上**没有 return**，C 返回栈上垃圾 —— 语料判据（.out）看到的是
// 2.8 MB 空白输出。701 的 `classify` 同样缺 `ret ret0`。
//
// 形状仍然按实测折（六类，见下），但**只折"能证明落地位置正确"的那些**：
//
//	entry / exit                       函数出入口
//	forhead → forbody → forpost → fordone   唯一的循环形态
//	then / else / join                 条件分支
//	arm / mjoin                        match 的变体分派
//	selarm / seljoin / selnone         select 的就绪分派
//	checkfail / checkok                check（`?`）的传播
//
// 汇合点的处理（本层唯一的新机制）：构造在折自己的分支前**reserve** 自己的汇合点，
// 于是分支折到那里就停住、把边交给构造收口 —— 第一个到达它的分支再也不能把它
// "顺手折走"。出口块（exit）只在**多前驱**时才 reserve：
//
//	单前驱（242/266）⇒ 照旧折进那条路，零 goto；
//	多前驱 + 只剩 ret（258/266 的 exit 块指令数是 0）⇒ 把 ret **内联**到各分支
//	  （产物就是干净的多 return C，正是 C1–C17 想要的样子）；
//	多前驱 + 自带指令（8/266，如 defer.run / region.exit）⇒ 发 `label:` + `goto`，
//	  保证那些指令**只跑一次**。
//
// 把 goto 再收成 if/for（真正的"无 goto 汤"）是 **P3** 的工作，那时有产物级断言
// 兜底；本层不许为了好看牺牲保真。
// ---------------------------------------------------------------------------

// succs 取一个终结符的出边（ret/checkfail 无出边）。
func succs(t air.Term) []string {
	switch v := t.(type) {
	case *air.Br:
		return []string{v.Label}
	case *air.Cbr:
		return []string{v.Then, v.Else}
	case *air.Switch:
		return append([]string{}, v.Labels...)
	}
	return nil
}

// Build 把 mair 折叠成 C 形态程序并自检（自检 = 逐边物化核对）。
func Build(m *air.Module) (*Prog, error) {
	p := &Prog{Pkg: m.Pkg, Imports: append([]string{}, m.Imports...), Types: append([]*air.TypeDecl{}, m.Types...)}
	for _, f := range m.Funcs {
		nf, err := buildFunc(f)
		if err != nil {
			return nil, err
		}
		p.Funcs = append(p.Funcs, nf)
	}
	if err := Verify(p); err != nil {
		return nil, err
	}
	return p, nil
}

type folder struct {
	sym     string
	byLabel map[string]*air.Block
	order   []string // 声明序（剩余块/标签块的确定性发射序）
	// links 记录"某层 if 的两支已close 进它、线性链应当继续穿过的汇合点"
	// （多前驱块默认不许内联进链 —— 见 fold 的停步规则）。
	links      map[string]bool
	preds      map[string]map[string]bool
	done    map[string]bool
	labeled map[string]bool // 被 goto 引用的块（要发 label）
	inlined map[string]bool // 内容被内联到各分支的块（只剩 ret 的 exit）
	// emittedLabel 记录"带标签发过"的块（自检 ③：goto 不许悬空）。
	emittedLabel map[string]bool
	reserve      map[string]int // 折叠期间"原地发"的块（计数：嵌套构造可重叠）
	loops        []loopCtx
	exit         string
	edges        map[[2]string]bool // 已物化的边
	// regOpen 跨块 region 的开放栈：enter 在这块、exit 在后续块时，平铺的
	// RegionOpen/RegionClose 靠它找回配对（air V3.1 保证栈纪律）。
	regOpen []regSpan
	regSeq  int // scope 域的 aic_scope 变量名序号（确定性）
}

// regSpan 是一个跨块 region 的开放记录。
type regSpan struct {
	name  string // 非空 = scope 域的 aic_scope 变量名
	scope bool
}

type loopCtx struct{ head, post, done string }

func buildFunc(f *air.Func) (*Func, error) {
	fl := &folder{
		sym: f.Sym, byLabel: map[string]*air.Block{},
		preds: map[string]map[string]bool{}, done: map[string]bool{},
		labeled: map[string]bool{}, inlined: map[string]bool{}, emittedLabel: map[string]bool{},
		reserve: map[string]int{}, edges: map[[2]string]bool{},
		links:   map[string]bool{},
	}
	for _, b := range f.Blocks {
		fl.byLabel[b.Label] = b
		fl.order = append(fl.order, b.Label)
	}
	for _, b := range f.Blocks {
		for _, s := range succs(b.Term) {
			if fl.preds[s] == nil {
				fl.preds[s] = map[string]bool{}
			}
			fl.preds[s][b.Label] = true
		}
	}
	out := &Func{Sym: f.Sym, Params: append([]air.Param{}, f.Params...), Rets: append([]string{}, f.Rets...),
		Flags: append([]string{}, f.Flags...), Loc: f.Loc, SrcHash: air.HashFunc(f)}
	if len(f.Blocks) == 0 {
		return out, nil
	}
	if _, ok := fl.byLabel["exit"]; ok {
		fl.exit = "exit"
		if len(fl.preds["exit"]) > 1 {
			// 出口块多前驱：只能发一次 ⇒ 常驻 reserve，由收口处决定内联还是 goto。
			fl.reserve["exit"] = 1 << 20
		}
	}
	entry := f.Blocks[0].Label
	body, stop, from, err := fl.fold(entry)
	if err != nil {
		return nil, fmt.Errorf("cir: %s: %w", f.Sym, err)
	}
	if body, err = fl.close(body, stop, from, "", f.Loc); err != nil {
		return nil, fmt.Errorf("cir: %s: %w", f.Sym, err)
	}
	// 剩余块：只可能由 goto 到达（否则就是丢边，下面的逐边核对会报）。
	for {
		progressed := false
		for _, lbl := range fl.order {
			if fl.done[lbl] || fl.inlined[lbl] || !fl.labeled[lbl] {
				continue
			}
			unit := []Stmt{&Label{Name: lbl, Loc: fl.byLabel[lbl].Loc}}
			fl.emittedLabel[lbl] = true
			// 这里要**真的把这块折出来**：它可能还被 reserve 着（多前驱的 exit），
			// 不放开就是空折 —— 而且空折会让下面这个 for 永远转下去。
			saved := fl.reserve[lbl]
			fl.reserve[lbl] = 0
			stmts, st, fr, err := fl.fold(lbl)
			fl.reserve[lbl] = saved
			if err != nil {
				return nil, fmt.Errorf("cir: %s: %w", f.Sym, err)
			}
			unit = append(unit, stmts...)
			if unit, err = fl.close(unit, st, fr, "", fl.byLabel[lbl].Loc); err != nil {
				return nil, fmt.Errorf("cir: %s: %w", f.Sym, err)
			}
			body = append(body, unit...)
			progressed = true
		}
		if !progressed {
			break
		}
	}
	out.Body = body
	// 自检 ①：每个块都被发过一次（折走、或内联进分支）。
	for _, b := range f.Blocks {
		if !fl.done[b.Label] && !fl.inlined[b.Label] {
			return nil, fmt.Errorf("cir: %s: block %s was never emitted (compiler defect)", f.Sym, b.Label)
		}
	}
	// 自检 ②：**逐边核对** —— CFG 的每条边都必须被物化（这是本层的核心判据）。
	for _, b := range f.Blocks {
		for _, s := range succs(b.Term) {
			if !fl.edges[[2]string{b.Label, s}] {
				return nil, fmt.Errorf("cir: %s: CFG edge %s -> %s was dropped by structuring (compiler defect)", f.Sym, b.Label, s)
			}
		}
	}
	// 自检 ③：被 goto 引用的块必须**带标签发过**（发过但没标签 = goto 悬空）。
	for lbl := range fl.labeled {
		if fl.inlined[lbl] {
			continue
		}
		if fl.done[lbl] && !fl.emittedLabel[lbl] {
			return nil, fmt.Errorf("cir: %s: block %s is goto-referenced but was emitted without a label (compiler defect)", f.Sym, lbl)
		}
	}
	return out, nil
}

// fold 从 start 起直线折，直到遇到已折过 / 被本层 reserve 的块。
// 返回 (语句, 停在哪块, 停住的那条边从哪来)。stop == "" ⇒ 没有未物化的边。
func (fl *folder) fold(start string) ([]Stmt, string, string, error) {
	var out []Stmt
	cur := start
	pendFrom := ""
	for cur != "" {
		if fl.done[cur] || fl.reserve[cur] > 0 {
			return out, cur, pendFrom, nil
		}
		// **汇合点不内联进线性链**：多个前驱且自带指令的块是被多条路径共享的
		// 代码。折进第一条到达它的链 ⇒ 只有那条路径执行它，其余路径只剩 goto
		// 却没有标签（tool 实测：join3 被 then 支折走，else 支的 goto 悬空）。
		// 例外：① 本层 if 已把两支 close 进它（links 登记）；② 出口块（零指令，
		// 由 close 的 inlineExit 处理）；③ 链的起点（调用方刚折完 if 正续进来）。
		if cur != start && len(fl.preds[cur]) > 1 && len(fl.byLabel[cur].Insts) > 0 &&
			!fl.isLoopHead(cur) && !fl.isLoopPost(cur) &&
			!(fl.links[cur] && !fl.labeled[cur]) {
			return out, cur, pendFrom, nil
		}
		if pendFrom != "" {
			// 上一条 `br cur` 靠文字顺序落空 ⇒ 物化。
			fl.edges[[2]string{pendFrom, cur}] = true
			pendFrom = ""
		}
		b := fl.byLabel[cur]
		if b == nil {
			return nil, "", "", fmt.Errorf("block %s not found", cur)
		}
		fl.done[cur] = true
		// 经 links 豁免内联的多前驱汇合点：先发标签。后到的路径会 goto 到这里
		// （C 的标签可以位于块内，goto 跳进来合法；未使用的标签在无 -Wall 的
		// 构建下也无警告）。
		// **零指令多前驱块也要标签**（D24 实测）：它照样会被第二条路径 goto
		// （`len(Insts) > 0` 的老条件让它以"无标签的文字落空"被内联进第一条链
		// ⇒ 第二条路径的 goto 悬空，自检 ③ 报 "goto-referenced but was emitted
		// without a label"）。出口块（零指令）走 inlineExit 不受影响。
		if len(fl.preds[cur]) > 1 && !fl.emittedLabel[cur] {
			out = append(out, &Label{Name: cur, Loc: b.Loc})
			fl.emittedLabel[cur] = true
		}
		stmts, next, err := fl.foldBlock(b)
		if err != nil {
			return nil, "", "", err
		}
		out = append(out, stmts...)
		if br, ok := b.Term.(*air.Br); ok && br.Label == next {
			pendFrom = cur
		}
		cur = next
	}
	return out, "", "", nil
}

// close 收口一条折到 stop 停住的分支：把那条边物化成 goto / 内联 ret / 落空到 join。
func (fl *folder) close(stmts []Stmt, stop, from, join string, loc air.Loc) ([]Stmt, error) {
	if stop == "" {
		return stmts, nil
	}
	if stop == join {
		if from != "" {
			fl.edges[[2]string{from, stop}] = true // 落空到本层的下一句
		}
		return stmts, nil
	}
	if from != "" {
		fl.edges[[2]string{from, stop}] = true // 由下面这条 goto / 内联 ret 物化
	}
	if ep, ok := fl.inlineExit(stop, loc); ok {
		return append(stmts, ep...), nil
	}
	fl.labeled[stop] = true
	return append(stmts, &Goto{Name: stop, Loc: loc}), nil
}

// inlineExit 在"出口块只剩一个 ret"时把 ret 内联到调用点（产物 = 多 return 的干净 C）。
// 出口块自带指令（defer.run / region.exit 之类）时**不许**内联：那些指令只能跑一次。
func (fl *folder) inlineExit(block string, loc air.Loc) ([]Stmt, bool) {
	if fl.exit == "" || block != fl.exit {
		return nil, false
	}
	b := fl.byLabel[block]
	if b == nil || len(b.Insts) != 0 {
		return nil, false
	}
	ret, ok := b.Term.(*air.Ret)
	if !ok {
		return nil, false
	}
	fl.inlined[block] = true
	return []Stmt{&Return{Vals: append([]string{}, ret.Vals...), Loc: loc}}, true
}

// foldBlock 折一个块：块内指令（含 region 作用域）+ 终结符。
func (fl *folder) foldBlock(b *air.Block) ([]Stmt, string, error) {
	stmts, err := fl.foldInsts(b, b.Insts)
	if err != nil {
		return nil, "", err
	}
	// 循环头（forhead）特殊：块内指令**每轮都要跑**（它们算的就是循环条件），
	// 所以不能像普通块那样留在循环之前，而要进 For 的 Head 槽（循环体开头）。
	// 这是 012 语料实测出的语义错：留在外面 ⇒ 条件只算一次 ⇒ 死循环；而再往
	// Post 里补一份同名的 `let`，在 C 里是新作用域的**另一个**变量，改不到外层。
	if cbr, ok := b.Term.(*air.Cbr); ok && isForShape(cbr) {
		next, extra, err := fl.foldIfHead(b, cbr, stmts)
		if err != nil {
			return nil, "", err
		}
		return extra, next, nil
	}
	next, extra, err := fl.foldTerm(b)
	if err != nil {
		return nil, "", err
	}
	return append(stmts, extra...), next, nil
}

// isForShape 报告一个条件分支是否是降级器生成的循环头形态
// （then = forbody、else = fordone；六种标签形态之一，核心设计 §十）。
func isForShape(t *air.Cbr) bool {
	return strings.HasPrefix(t.Then, "forbody") && strings.HasPrefix(t.Else, "fordone")
}

// foldInsts 折块内指令。region 有两种折法：
//   - 同块内 enter/exit 成对（词法括号匹配）→ Region{Body} 结构化块（原形态）；
//   - 跨块（enter 在这块、exit 在降级后的后续块）→ 平铺 RegionOpen/RegionClose，
//     配对由 fl.regOpen 栈跨块记录。air 层 V3.1 已沿 CFG 边保证结构化配平，
//     故按遇到顺序平铺即保持执行序（goto 改 relabel 不改动态序）。
func (fl *folder) foldInsts(b *air.Block, insts []air.Inst) ([]Stmt, error) {
	var out []Stmt
	for i := 0; i < len(insts); i++ {
		in := insts[i]
		switch v := in.(type) {
		case *air.RegionEnter:
			// 找同一块内配对的 exit（词法嵌套 ⇒ 括号匹配）
			depth := 0
			end := -1
			for j := i; j < len(insts); j++ {
				switch insts[j].(type) {
				case *air.RegionEnter:
					depth++
				case *air.RegionExit:
					depth--
					if depth == 0 {
						end = j
					}
				}
				if end >= 0 {
					break
				}
			}
			if end >= 0 {
				inner, err := fl.foldInsts(b, insts[i+1:end])
				if err != nil {
					return nil, err
				}
				out = append(out, &Region{Body: inner, Scope: v.Scope, Loc: v.Loc})
				i = end
				continue
			}
			// 跨块：平铺。scope 域的 aic_scope 变量名在此定下（确定性：函数内序号）。
			fl.regSeq++
			name := ""
			if v.Scope {
				name = fmt.Sprintf("aic_t_region_%d", fl.regSeq)
			}
			fl.regOpen = append(fl.regOpen, regSpan{name: name, scope: v.Scope})
			out = append(out, &RegionOpen{Name: name, Scope: v.Scope, Loc: v.Loc})
		case *air.RegionExit:
			// 同块配对已在上面的 RegionEnter 分支被吃掉；走到这里 = 跨块的 exit。
			if len(fl.regOpen) == 0 {
				return nil, fmt.Errorf("region.exit without enter in block %s", b.Label)
			}
			top := fl.regOpen[len(fl.regOpen)-1]
			fl.regOpen = fl.regOpen[:len(fl.regOpen)-1]
			out = append(out, &RegionClose{Name: top.name, Scope: top.scope, Loc: v.Loc})
		default:
			out = append(out, &Leaf{In: in, Loc: air.InstLoc(in)})
		}
	}
	return out, nil
}

// foldTerm 折终结符；返回 (下一块标签, 额外语句)。
func (fl *folder) foldTerm(b *air.Block) (string, []Stmt, error) {
	switch t := b.Term.(type) {
	case nil:
		return "", nil, nil

	case *air.Ret:
		return "", []Stmt{&Return{Vals: append([]string{}, t.Vals...), Loc: t.Loc}}, nil

	case *air.CheckFail:
		return "", []Stmt{&CheckFailStmt{Err: t.Err, Tmp: t.Tmp, Loc: t.Loc}}, nil

	case *air.Br:
		if len(fl.loops) > 0 {
			top := fl.loops[len(fl.loops)-1]
			if t.Label == top.done {
				// 边当场物化成 break（跳到循环的出口块）。
				fl.edges[[2]string{b.Label, t.Label}] = true
				return "", []Stmt{&Break{Loc: t.Loc}}, nil
			}
			if t.Label == top.post || t.Label == top.head {
				fl.edges[[2]string{b.Label, t.Label}] = true
				return "", []Stmt{&Continue{Loc: t.Loc}}, nil
			}
		}
		// 词法块（源码的 `{ … }`）：air 里是 scopeN → … → scopeendM 一对。
		// 折成 Scope 而不是当直线延续 —— 块边界承载"同名遮蔽从哪儿起指谁"。
		if isScopeStart(t.Label) {
			if end := fl.scopeEnd(t.Label); end != "" {
				fl.edges[[2]string{b.Label, t.Label}] = true
				fl.reserve[end]++
				body, stop, from, err := fl.fold(t.Label)
				fl.reserve[end]--
				if err != nil {
					return "", nil, err
				}
				if body, err = fl.close(body, stop, from, end, t.Loc); err != nil {
					return "", nil, err
				}
				return end, []Stmt{&Scope{Body: body, Loc: t.Loc}}, nil
			}
		}
		// 未定：交给 fold 决定是"落空"还是"收口成 goto"（边在那里物化）。
		return t.Label, nil, nil

	case *air.Cbr:
		// cbr 的两条出边由 If/For 的结构物化（两支各自成为分支）。
		fl.edges[[2]string{b.Label, t.Then}] = true
		fl.edges[[2]string{b.Label, t.Else}] = true
		return fl.foldIf(b, t)

	case *air.Switch:
		for _, label := range t.Labels {
			fl.edges[[2]string{b.Label, label}] = true
		}
		return fl.foldSwitch(t)
	}
	return "", nil, fmt.Errorf("%s: unsupported terminator %T", b.Label, b.Term)
}


// --- 干跑探测（foldIfPlain 的汇合点判定）------------------------------------
//
// 为什么需要：if 的两支停在同一块时，那块就是本层的汇合点 —— 必须**在折之前**
// 知道它（reserve + links 登记），否则第一支会把汇合点内联掉，第二支只剩一个
// 没有标签的 goto（sieve 实测：then 支穿过整个内层循环才落到 join7，简单的
// brTarget 判据看不出来）。判定方法 = 干跑：快照全部可变状态 → 折一支 → 看停点
// → 恢复。fold 是确定性的，干跑与真折行为一致。

type folderSnap struct {
	done         map[string]bool
	labeled      map[string]bool
	inlined      map[string]bool
	emittedLabel map[string]bool
	links        map[string]bool
	reserve      map[string]int
	edges        map[[2]string]bool
	loops        []loopCtx
}

func (fl *folder) snapshot() *folderSnap {
	cp := func(m map[string]bool) map[string]bool {
		out := make(map[string]bool, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	cpI := func(m map[string]int) map[string]int {
		out := make(map[string]int, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	cpE := func(m map[[2]string]bool) map[[2]string]bool {
		out := make(map[[2]string]bool, len(m))
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	return &folderSnap{
		done: cp(fl.done), labeled: cp(fl.labeled), inlined: cp(fl.inlined),
		emittedLabel: cp(fl.emittedLabel), links: cp(fl.links),
		reserve: cpI(fl.reserve), edges: cpE(fl.edges),
		loops: append([]loopCtx{}, fl.loops...),
	}
}

func (fl *folder) restore(sp *folderSnap) {
	fl.done, fl.labeled, fl.inlined = sp.done, sp.labeled, sp.inlined
	fl.emittedLabel, fl.links, fl.reserve, fl.edges = sp.emittedLabel, sp.links, sp.reserve, sp.edges
	fl.loops = sp.loops
}

// dryStop 干跑折一条链，返回它停在哪（出错 = 空串；状态完全恢复）。
func (fl *folder) dryStop(start string) string {
	sp := fl.snapshot()
	_, stop, _, err := fl.fold(start)
	fl.restore(sp)
	if err != nil {
		return ""
	}
	return stop
}

// foldIf 折条件分支：识别 for（forhead 形状）、if/else、以及 if 无 else 三种形态。
// head = 条件块自身的标签（循环头就是它）。
func (fl *folder) foldIf(headBlk *air.Block, t *air.Cbr) (string, []Stmt, error) {
	if isForShape(t) {
		return fl.foldIfHead(headBlk, t, nil)
	}
	return fl.foldIfPlain(headBlk, t)
}

// foldIfHead 折循环头：headStmts = 循环头块的指令（每轮先跑，算条件）。
func (fl *folder) foldIfHead(headBlk *air.Block, t *air.Cbr, headStmts []Stmt) (string, []Stmt, error) {
	head := headBlk.Label
	// 循环头的两条出边由 For 的结构物化（体 = then；出口 = else）。
	// **必须在这里记**：forhead 形态在 foldBlock 里被短路了，走不到 foldTerm 的 Cbr 分支。
	fl.edges[[2]string{head, t.Then}] = true
	fl.edges[[2]string{head, t.Else}] = true
	post := fl.postOf(head)
	// 循环的出口块（fordone）在本层就地发：折体期间 reserve 住它，免得体内某个
	// 嵌套构造把它折进自己分支里（那样循环的出口就跑到循环体里去了）。
	fl.reserve[t.Else]++
	defer func() { fl.reserve[t.Else]-- }()
	// **先建循环上下文再折体**：体尾的 `br forpost`/`br forhead` 要折成 continue，
	// `br fordone` 要折成 break —— 顺序反了会把体折成"空"并丢掉这些边。
	fl.loops = append(fl.loops, loopCtx{head: head, post: post, done: t.Else})
	body, stop, from, err := fl.fold(t.Then)
	if err != nil {
		fl.loops = fl.loops[:len(fl.loops)-1]
		return "", nil, err
	}
	if body, err = fl.close(body, stop, from, t.Else, t.Loc); err != nil {
		fl.loops = fl.loops[:len(fl.loops)-1]
		return "", nil, err
	}
	postStmts := []Stmt{}
	if post != "" && !fl.done[post] {
		more, pstop, pfrom, err := fl.fold(post)
		if err != nil {
			fl.loops = fl.loops[:len(fl.loops)-1]
			return "", nil, err
		}
		if more, err = fl.close(more, pstop, pfrom, t.Else, t.Loc); err != nil {
			fl.loops = fl.loops[:len(fl.loops)-1]
			return "", nil, err
		}
		postStmts = append(postStmts, more...)
	}
	// post 块若只折出 "continue"（`br forhead`），它对 C 的 for 是多余的：C 的
	// post 槽本身就在每轮末尾跑。留着会出现连续两个 continue（可读性退化）。
	if n := len(postStmts); n > 0 {
		if _, isCont := postStmts[n-1].(*Continue); isCont {
			postStmts = postStmts[:n-1]
		}
	}
	if n := len(body); n > 0 {
		if _, isCont := body[n-1].(*Continue); isCont {
			body = body[:n-1]
		}
	}
	fl.loops = fl.loops[:len(fl.loops)-1]
	return t.Else, []Stmt{&For{Head: headStmts, Cond: t.Cond, Post: postStmts, Body: body, Loc: t.Loc}}, nil
}

// foldIfPlain 折非循环的条件分支：if/else 或 if 无 else。
func (fl *folder) foldIfPlain(headBlk *air.Block, t *air.Cbr) (string, []Stmt, error) {
	_ = headBlk
	// if/else：两支都 `br` 到同一 join ⇒ 那是本层的汇合点，跟在本层之后发。
	thenB, elseB := fl.byLabel[t.Then], fl.byLabel[t.Else]
	join := ""
	if thenB != nil && elseB != nil {
		if a, b := brTarget(thenB), brTarget(elseB); a != "" && a == b {
			join = a
		}
	}
	// brTarget 只认"两支的终结符都是简单 br 同一块"。一支穿过内层循环/嵌套 if
	// 才落到汇合点时它看不出来（sieve：then → 内层 for → join7，else → join7）
	// ⇒ 干跑探测两支的真实停点：相同 ⇒ 那就是汇合点。
	if join == "" {
		if ts, es := fl.dryStop(t.Then), fl.dryStop(t.Else); ts != "" && ts == es {
			join = ts
		}
	}
	if join != "" {
		// **折分支期间 reserve 汇合点**：否则第一个到达它的分支会把它折走，
		// 另一支的边就丢了（791/701 的实测形态）。
		fl.reserve[join]++
		defer func() { fl.reserve[join]-- }()
	}
	thenStmts, tstop, tfrom, err := fl.fold(t.Then)
	if err != nil {
		return "", nil, err
	}
	if thenStmts, err = fl.close(thenStmts, tstop, tfrom, join, t.Loc); err != nil {
		return "", nil, err
	}
	var elseStmts []Stmt
	if join == "" || t.Else != join {
		estmts, estop, efrom, err := fl.fold(t.Else)
		if err != nil {
			return "", nil, err
		}
		if elseStmts, err = fl.close(estmts, estop, efrom, join, t.Loc); err != nil {
			return "", nil, err
		}
	}
	if join != "" {
		fl.links[join] = true
	}
	return join, []Stmt{&If{Cond: t.Cond, Then: thenStmts, Else: elseStmts, Loc: t.Loc}}, nil
}

// isLoopHead 报告一个块是不是循环头（forhead 形状：then=forbody/else=fordone）。
// 循环头的多前驱是**结构性的**（入口边 + post 回边），不是待停下的汇合点 ——
// 线性链从块外走进循环头正是"进入循环"的合法形态。
func (fl *folder) isLoopHead(lbl string) bool {
	b := fl.byLabel[lbl]
	if b == nil {
		return false
	}
	cbr, ok := b.Term.(*air.Cbr)
	return ok && isForShape(cbr)
}

// isLoopPost 报告一个块是不是某个循环的 post（终结符是 `br <循环头>`）。
// post 的多前驱同样结构性（体尾 + 内层循环出口等），链走进去应由 foldTerm 折成
// continue（回边），不是 goto。
func (fl *folder) isLoopPost(lbl string) bool {
	b := fl.byLabel[lbl]
	if b == nil {
		return false
	}
	br, ok := b.Term.(*air.Br)
	return ok && fl.isLoopHead(br.Label)
}

// isScopeStart 报告一个标签是不是 air 的词法块起点（`scopeN`，不是 `scopeendM`）。
func isScopeStart(label string) bool {
	return strings.HasPrefix(label, "scope") && !strings.HasPrefix(label, "scopeend")
}

// scopeEnd 找词法块的配对终点：从 scopeN 起按声明序扫描，遇 `scope*` 深度 +1、
// 遇 `scopeend*` 深度 −1，深度归零的那块就是终点（嵌套块因此也能配对，014/722 实测）。
func (fl *folder) scopeEnd(start string) string {
	i := -1
	for k, lbl := range fl.order {
		if lbl == start {
			i = k
			break
		}
	}
	if i < 0 {
		return ""
	}
	depth := 1
	for _, lbl := range fl.order[i+1:] {
		switch {
		case isScopeStart(lbl):
			depth++
		case strings.HasPrefix(lbl, "scopeend"):
			depth--
			if depth == 0 {
				return lbl
			}
		}
	}
	return ""
}

// postOf 找循环头对应的 forpost 块。
//
// 两条路，**先按命名约定找**：AIR 降级器给一个循环发四块同号的
// forhead<N>/forbody<N+1>/forpost<N+2>/fordone<N+3>，而 post 块的终结符一定是
// `br forhead<N>`。结构式搜法（体块直接 br forpost）在"体块是 cbr"时不成立 ——
// `while c { if d { continue } … }` 的体块就是 cbr（014/703 实测踩到）。
func (fl *folder) postOf(head string) string {
	for _, lbl := range fl.order {
		if !strings.HasPrefix(lbl, "forpost") {
			continue
		}
		if b := fl.byLabel[lbl]; b != nil && brTarget(b) == head {
			return lbl
		}
	}
	// 兜底：体块直接 br forpost（老形态）。
	b := fl.byLabel[head]
	if b == nil {
		return ""
	}
	cbr, ok := b.Term.(*air.Cbr)
	if !ok {
		return ""
	}
	bodyB := fl.byLabel[cbr.Then]
	if bodyB == nil {
		return ""
	}
	if tgt := brTarget(bodyB); tgt != "" && strings.HasPrefix(tgt, "forpost") {
		return tgt
	}
	return ""
}

// foldSwitch 折多路分支（match 的变体分派 / select 的就绪分派）。
//
// 汇合点先**静态**看出来（各臂的首个直线后继相同 ⇒ 那就是 join），折臂之前
// reserve 住它，再把每臂收口进 join —— 与 if/else 同一条纪律。
func (fl *folder) foldSwitch(t *air.Switch) (string, []Stmt, error) {
	cases := make([]Case, 0, len(t.Labels))
	join := fl.switchJoin(t)
	if join != "" {
		fl.reserve[join]++
		defer func() { fl.reserve[join]-- }()
	}
	for i, label := range t.Labels {
		b := fl.byLabel[label]
		if b == nil {
			return "", nil, fmt.Errorf("switch arm %s not found", label)
		}
		if fl.done[label] || fl.reserve[label] > 0 {
			// 该臂已被折过（或由外层原地发）：这里只留一个空 case。
			cases = append(cases, Case{Label: fmt.Sprintf("%d", i)})
			continue
		}
		fl.done[label] = true
		stmts, next, err := fl.foldBlock(b)
		if err != nil {
			return "", nil, err
		}
		fr := ""
		if br, ok := b.Term.(*air.Br); ok && br.Label == next {
			fr = label
		}
		if stmts, err = fl.close(stmts, next, fr, join, t.Loc); err != nil {
			return "", nil, err
		}
		cases = append(cases, Case{Label: fmt.Sprintf("%d", i), Body: stmts})
	}
	return join, []Stmt{&Switch{Val: t.Val, Cases: cases, Loc: t.Loc}}, nil
}

// switchJoin 静态取各臂的公共直线后继（不同或为空 ⇒ 没有公共汇合点）。
func (fl *folder) switchJoin(t *air.Switch) string {
	join := ""
	for _, label := range t.Labels {
		b := fl.byLabel[label]
		if b == nil {
			return ""
		}
		tgt := brTarget(b)
		if tgt == "" {
			return ""
		}
		if join == "" {
			join = tgt
			continue
		}
		if join != tgt {
			return ""
		}
	}
	return join
}

func brTarget(b *air.Block) string {
	if br, ok := b.Term.(*air.Br); ok {
		return br.Label
	}
	return ""
}

// --- 回折自检 ---------------------------------------------------------------

// Flatten 把结构化语句折回 (块序列, 终结符序列)，用于与 CFG 比对规模。
func Flatten(f *Func) ([]string, []string) {
	s := &flattener{}
	s.stmts(f.Body)
	return s.blocks, s.terms
}

type flattener struct {
	blocks []string
	terms  []string
	seq    int
}

func (s *flattener) stmts(list []Stmt) {
	for _, st := range list {
		s.stmt(st)
	}
}

func (s *flattener) stmt(st Stmt) {
	s.seq++
	switch v := st.(type) {
	case *Leaf:
		s.terms = append(s.terms, "leaf")
	case *Guard:
		s.terms = append(s.terms, "guard:"+v.Limit)
	case *Return:
		s.terms = append(s.terms, "ret")
	case *Break:
		s.terms = append(s.terms, "break")
	case *Continue:
		s.terms = append(s.terms, "continue")
	case *Exit:
		s.terms = append(s.terms, "exit")
	case *If:
		s.blocks = append(s.blocks, fmt.Sprintf("if%d", s.seq))
		s.terms = append(s.terms, "cbr")
		s.stmts(v.Then)
		s.stmts(v.Else)
	case *For:
		s.blocks = append(s.blocks, fmt.Sprintf("for%d", s.seq))
		s.terms = append(s.terms, "cbr")
		s.stmts(v.Body)
	case *Switch:
		s.blocks = append(s.blocks, fmt.Sprintf("switch%d", s.seq))
		s.terms = append(s.terms, "switch")
		for _, c := range v.Cases {
			s.stmts(c.Body)
		}
	case *Region:
		s.blocks = append(s.blocks, fmt.Sprintf("region%d", s.seq))
		s.stmts(v.Body)
	case *Scope:
		s.blocks = append(s.blocks, fmt.Sprintf("scope%d", s.seq))
		s.stmts(v.Body)
	case *Label:
		s.terms = append(s.terms, "label:"+v.Name)
	case *Goto:
		s.terms = append(s.terms, "goto:"+v.Name)
	}
}

// --- verifier ---------------------------------------------------------------

// Verify 是 cir 的 verifier：结构化形态必须自洽。
func Verify(p *Prog) error {
	for _, f := range p.Funcs {
		if len(f.Body) == 0 && len(f.Rets) > 0 {
			return fmt.Errorf("cir: %s has results but an empty body", f.Sym)
		}
		if err := verifyStmts(f.Sym, f.Body, 0); err != nil {
			return err
		}
		// goto 必须有着落（每个 Goto 都要有同名 Label）：发不出去的 goto 会让 C
		// 直接编译失败，或更糟——跳到错误的标签（同名跨函数也不行）。
		labels := map[string]int{}
		gotos := map[string]int{}
		collectLabels(f.Body, labels, gotos)
		for name, n := range gotos {
			if labels[name] == 0 {
				return fmt.Errorf("cir: %s has %d goto(s) to %q with no label (compiler defect)", f.Sym, n, name)
			}
		}
		for name, n := range labels {
			if n > 1 {
				return fmt.Errorf("cir: %s emits the label %q %d times (a label must be unique per function)", f.Sym, name, n)
			}
		}
	}
	return nil
}

func collectLabels(list []Stmt, labels, gotos map[string]int) {
	for _, st := range list {
		switch v := st.(type) {
		case *Label:
			labels[v.Name]++
		case *Goto:
			gotos[v.Name]++
		case *If:
			collectLabels(v.Then, labels, gotos)
			collectLabels(v.Else, labels, gotos)
		case *For:
			collectLabels(v.Head, labels, gotos)
			collectLabels(v.Body, labels, gotos)
			collectLabels(v.Post, labels, gotos)
		case *Switch:
			for _, c := range v.Cases {
				collectLabels(c.Body, labels, gotos)
			}
		case *Region:
			collectLabels(v.Body, labels, gotos)
		}
	}
}

func verifyStmts(sym string, list []Stmt, loopDepth int) error {
	open := 0 // 平铺 region 的开放数（ Region{Body} 自配平，不计 ）
	for _, st := range list {
		switch v := st.(type) {
		case *Leaf:
			if v.In == nil {
				return fmt.Errorf("cir: %s has a nil leaf instruction", sym)
			}
			switch v.In.(type) {
			case *air.RegionEnter, *air.RegionExit:
				return fmt.Errorf("cir: %s still carries a raw region.enter/exit leaf (structuring incomplete)", sym)
			}
		case *Guard:
			if v.Limit == "" {
				return fmt.Errorf("cir: %s has a guard with an empty limit", sym)
			}
		case *If:
			if v.Cond == "" {
				return fmt.Errorf("cir: %s has an if with an empty condition", sym)
			}
			if len(v.Then) == 0 {
				return fmt.Errorf("cir: %s has an if with an empty then-branch", sym)
			}
			if err := verifyStmts(sym, v.Then, loopDepth); err != nil {
				return err
			}
			if err := verifyStmts(sym, v.Else, loopDepth); err != nil {
				return err
			}
		case *For:
			if len(v.Body) == 0 {
				return fmt.Errorf("cir: %s has a loop with an empty body", sym)
			}
			if err := verifyStmts(sym, v.Head, loopDepth+1); err != nil {
				return err
			}
			if err := verifyStmts(sym, v.Body, loopDepth+1); err != nil {
				return err
			}
			if err := verifyStmts(sym, v.Post, loopDepth+1); err != nil {
				return err
			}
		case *Switch:
			if v.Val == "" {
				return fmt.Errorf("cir: %s has a switch without a value", sym)
			}
			if len(v.Cases) == 0 {
				return fmt.Errorf("cir: %s has a switch with no cases", sym)
			}
			for _, c := range v.Cases {
				if err := verifyStmts(sym, c.Body, loopDepth); err != nil {
					return err
				}
			}
		case *Region:
			if len(v.Body) == 0 {
				return fmt.Errorf("cir: %s has an empty region block", sym)
			}
			if err := verifyStmts(sym, v.Body, loopDepth); err != nil {
				return err
			}
		case *RegionOpen:
			// 平铺 region 入口：scope 域必须带 aic_scope 变量名（spawn 站点取用）。
			if v.Scope && v.Name == "" {
				return fmt.Errorf("cir: %s has a scope region-open without a name", sym)
			}
			open++
		case *RegionClose:
			if open == 0 {
				return fmt.Errorf("cir: %s has a region-close without a region-open", sym)
			}
			if v.Scope && v.Name == "" {
				return fmt.Errorf("cir: %s has a scope region-close without a name", sym)
			}
			open--
		case *Scope:
			if len(v.Body) == 0 {
				return fmt.Errorf("cir: %s has an empty lexical scope", sym)
			}
			if err := verifyStmts(sym, v.Body, loopDepth); err != nil {
				return err
			}
		case *Break, *Continue:
			if loopDepth == 0 {
				return fmt.Errorf("cir: %s has break/continue outside a loop", sym)
			}
		case *Label:
			if v.Name == "" {
				return fmt.Errorf("cir: %s has an unnamed label", sym)
			}
		case *Goto:
			if v.Name == "" {
				return fmt.Errorf("cir: %s has a goto without a target", sym)
			}
		}
	}
	if open != 0 {
		return fmt.Errorf("cir: %s has %d unclosed region-open(s)", sym, open)
	}
	return nil
}

// Summary 是 cir 的一行统计（覆盖数上升的判据）。
func Summary(p *Prog) string {
	leaves, nodes := 0, 0
	for _, f := range p.Funcs {
		l, n := CountStmts(f.Body)
		leaves += l
		nodes += n
	}
	return fmt.Sprintf("cir: %d funcs, %d leaves, %d structured nodes", len(p.Funcs), leaves, nodes)
}

// CheckFailStmt 是 check（?）的传播出口（air 里是终结符，CIR 里是语句）。
type CheckFailStmt struct {
	Err string
	Tmp string
	Loc air.Loc
}

func (*CheckFailStmt) cirStmt() {}

// CountKinds 按语句种类统计（自检与报告共用）。
func CountKinds(list []Stmt) map[string]int {
	out := map[string]int{}
	var walk func([]Stmt)
	walk = func(ls []Stmt) {
		for _, st := range ls {
			switch v := st.(type) {
			case *Leaf:
				out["leaf"]++
			case *Guard:
				out["guard"]++
			case *CheckFailStmt:
				out["checkfail"]++
			case *Return:
				out["ret"]++
			case *Break:
				out["break"]++
			case *Continue:
				out["continue"]++
			case *Label:
				out["label"]++
			case *Goto:
				out["goto"]++
			case *If:
				out["if"]++
				walk(v.Then)
				walk(v.Else)
			case *For:
				out["for"]++
				walk(v.Head)
				walk(v.Body)
				walk(v.Post)
			case *Switch:
				out["switch"]++
				for _, c := range v.Cases {
					walk(c.Body)
				}
			case *Region:
				out["region"]++
				walk(v.Body)
			case *RegionOpen:
				out["region-open"]++
			case *RegionClose:
				out["region-close"]++
			case *Scope:
				out["scope"]++
				walk(v.Body)
			}
		}
	}
	walk(list)
	return out
}
