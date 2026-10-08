// Package emit turns a checked AST into C11 text (核心设计 §十: AST → C 文本,
// 纯函数, 无全局状态).
//
// 纪律:
//   - 纯函数: 同一输入两次调用必须产出逐字节相同的文本 (红线 12, H2)。
//     故本包不持有包级可变状态; 一切状态都在 Ctx 里, 每次 Emit 新建。
//   - 每条语句/表达式前带 #line 回指 .aic 源 (红线 9), 由 Ctx.line 统一发射。
//   - 类型→C 名与 mangling 各只有一处实现 (红线 10, 防坑 5)。
//   - 发射目标 = runtime/*.h 已定案的 ABI (L0 区域/守卫/trap/浮点, L1 容器)。
package emit

import (
	"fmt"
	"sort"
	"strings"

	"aic/internal/air"
	"aic/internal/cir"
	"aic/internal/parse"
	"aic/internal/types"
)

// GuardCounts 是守卫点分桶（§10.3：total / eliminated / kept，按存储点种类分）。
// 现状：kept = 实际发射的守卫点数；eliminated 由检查器静态证明（未标 GuardSpec）承担，
// 故这里只报 kept 的分桶 —— H9③ 只断言 kept（无 region 块的语料必须为 0）。
type GuardCounts struct {
	Kept   int
	Store  int
	Return int
}

// Result 是一次发射的产物。
type Result struct {
	// C 是完整的翻译单元文本 (含 #line 回指)。
	C string
	// Warnings 是"emittable but semantically incomplete"的提示 (不阻断编译)。
	Warnings []string
	// Needed 是本产物用到的运行时能力 (排序稳定; 红线 16: L2 按需链接)。
	Needed []string
	// Guards 是守卫点计数（--report-guards）。
	Guards GuardCounts
}

// Ctx 是一次发射的全部状态 (无包级全局状态)。
type Ctx struct {
	// Path 是源文件路径 (诊断与 #line 用, 一律仓库根相对)。
	Path string
	// Info 是检查产物视图 (局部名/签名/具名类型/守卫标记)。
	Info *types.Info
	// File 是被发射的翻译单元。
	File *parse.File
	// cur 是当前发射的文件单元 (整程序发射时逐文件切换)。
	cur *FileUnit
	// deps 是包名 → 检查产物 (跨包调用/类型发射; 含本包)。
	deps map[string]*types.Info
	// retSeen 是已发射的合成返回结构名 (跨文件去重, 同一形状只发一次)。
	retSeen map[string]bool
	// entryPath 是入口包名 (main 所在包)。
	entryPath string

	// curFuncName 是 AIC 侧的当前函数标识 (FuncKey: 自由函数=名字, 方法=类.方法)。
	curFuncName string
	// curFunc 是当前函数签名 (返回形状/this 有无)。
	curFunc *types.FuncSig
	// retVar 是出口标签使用的返回结构变量名 (多返回 / Err 位时非空)。
	retVar string
	// loopDepth > 0 时 break/continue 可用 (发射 switch/for 的出口)。
	loopDepth int
	// tmpSeq 生成确定性的临时名序号 (禁随机/地址, 保 H2)。
	tmpSeq int
	// strSeq 生成确定性的字符串字面量名序号。
	strSeq int
	// lines 是输出缓冲 (按行拼, 便于 #line 与缩进)。
	buf strings.Builder
	// declared 记录已发射的字符串字面量 (同一文本只发一次)。
	declared map[string]string
	// names 是 pass 1 建立的名字→类型表 (函数标识 → 名字 → 类型)。
	names nameTypes
	// warnings 收集非阻断提示。
	warnings []string
	// needed 记录本翻译单元需要的运行时能力 (L2 按需链接的判据, 红线 16)。
	needed map[string]bool
	// deferCur 是当前函数的 defer 发射状态 (每函数重建; nil = 未在函数内)。
	deferCur *deferState
	// subst 是"node -> captured expression"的替换表 (defer trampoline 回放实参用)。
	subst map[parse.Expr]string
	// deferSeq 是全文件 defer 站点编号 (trampoline/上下文结构名唯一且确定)。
	deferSeq int
	// insts/instSeen 是泛型实例登记 (确定性顺序 + 实例缓存, 红线 11)。
	insts    []instEntry
	instSeen map[string]bool
	// printers/printSeen/printSeq 是按需合成的打印函数登记 (§十四 任意单值)。
	printers  []printerEntry
	printSeen map[string]string
	printSeq  int
	// witnesses/witnessSeen 是 interface 见证表登记 (§十 槽位分发 + trampoline)。
	witnesses   []witnessEntry
	witnessSeen map[string]bool
	// ifacePrint 是需要打印槽的接口集合 (§十四 扩展槽；键 = 包名.接口名)。
	ifacePrint map[string]bool
	// packedCmp/packedCmpUse/packedCmpOrder 是 @packed 比较助手登记（§三：`==` 逐字段、
	// `<` 等需 @derive(Compare)）。键 = 包名.类名；助手原型在原型段、体在 TU 末尾。
	packedCmp      map[string]bool
	packedCmpUse   map[string]string
	packedCmpOrder []*types.Class
	// seeded 报告本次是否为第二遍 (带着第一遍的发现结果发射)。
	seeded bool
	// substParams/substArgs 是当前发射上下文的类型代换（泛型实例化时 T → 实参）。
	// 类型名、表达式类型表、类型表达式三处都要过它，否则实例化后的 C 里会残留 T。
	substParams []string
	substArgs   []types.Type
	// funcInsts/funcInstSeen 是泛型函数实例的发射登记 (§十 单态化 + 实例缓存)。
	funcInsts    []funcInstEntry
	funcInstSeen map[string]bool
	// lenFacts 是「容器长度 = 常量」的事实（仅对字面量初始化且无变更点的容器成立）。
	lenFacts map[string]int
	// loopInduct 是当前循环的归纳变量与**字面量**上界（栈式：嵌套循环取最内层）。
	loopInduct *inductFact
	// probe 为真时 line() 不产文本（用于「探测遍」收集事实，见 emitForRange 的 F3）。
	probe bool
	// lenMutated 记录探测遍里「可能改变长度/数据指针」的容器（键 = 变量名）。
	lenMutated map[string]bool
	// lenSym 是容器的**符号长度式**（F4：填满再用形态，见 proofsym.go）。
	lenSym map[string]lin
	// appendFacts 是探测遍里对每个容器的 append 次数与「是否有其他变更」。
	appendFacts map[string]*appendCountFact
	// loopCondBound 是当前循环条件给出的上界事实（`for m <= limit`，栈式）。
	loopCondBound *condBound
	// loopLenBound 是 F3 事实：上界 = container.len() 的循环（栈式）。
	loopLenBound *lenBoundFact
	// unchecked 是本产物发出的 unchecked 访问次数（--report-guards 报告用）。
	unchecked int
	// declaredVars 是本函数**已声明**的变量（F5 只能缓存循环前已可见的容器）。
	declaredVars map[string]bool
	// indexUsed 是探测遍里「被下标访问的容器名」（F5 适用面）。
	indexUsed map[string]bool
	// cached 是当前循环体内走缓存形态的容器（名 → 缓存 id，见 proofcache.go 的 F5）。
	cached map[string]string
	// cachedCT 是缓存容器的 C 元素类型（名 → C 类型文本）。
	cachedCT map[string]string
	// cacheSeq 是缓存 id 序列（确定性）。
	cacheSeq int
	// guards 是守卫点计数（--report-guards / H9③：无 region 块的语料 kept 必须为 0）。
	guards GuardCounts
	// liveNext 为真时，紧随其后的创建表达式用 aic_alloc_live（@live，§五 R4）。
	liveNext bool
	// scopeCur 是当前 scope 的发射状态（spawn 需要它来登记任务，§七）。
	scopeCur *scopeState
	// spawnThunks/spawnSeen 是 spawn 薄 thunk 登记（每被调函数一份）。
	spawnThunks []*spawnThunk
	spawnSeen   map[string]bool
	// lamSeq/lamSites/lams 是 lambda 字面量登记 (提升为静态函数, §三)。
	lamSeq   int
	lamSites map[*parse.LambdaExpr]*lamSite
	lams     []*lamSite

	// --- IR 路径（L5d：体从 IR 取，不再走 AST）-----------------------------
	// ir 非空 = 本次发射走 IR 路径（见 irbody.go）。
	ir *IRProg
	// irTab 是「AIR 类型文本 → 语义类型」表（从检查产物重建，见 buildIRTypeTab）。
	irTab map[string]types.Type
	// irTemps 是当前函数里 值名 → AIR 类型文本（元素读/打印分派要用）。
	irTemps map[string]string
	// irCall 是 IR 符号 → C 符号（用户函数/方法/泛型实例）。
	irCall map[string]string
	// irCurFunc 是当前正在发射的 IR 函数体；irCurDefer 是它的 defer 状态。
	irCurFunc  *cir.Func
	irCurDefer *irDeferState
	// irPendingTy 是当前 let 的类型（接口调用/装箱的返回类型只能从这里拿）。
	irPendingTy string
	// irPendingLoc 是当前 let 的位置（运行时调用的 file/line 从这里来：trap 文本要用）。
	irPendingLoc air.Loc
	// irCurSym 是当前 IR 函数体的符号（defer 站点命名的唯一来源：实例之间不能撞名）。
	irCurSym string
	// irLoopPost 是当前嵌套的循环 Post 标签栈（continue 的落点）。
	irLoopPost []loopPost
	// irScopes 是 C 作用域的「AIR 名 → (C 名, AIR 类型)」绑定栈（源语言允许遮蔽，
	// C 不允许重定义；类型也必须跟着作用域走，否则拼接/比较会选错原语）。
	irScopes []map[string]irBinding
	// irShadow 是遮蔽改名序号（确定性，H2）。
	irShadow int
	// irRetTypes/irRetOrder 是 IR 路径发现的多返回值结构体（名 → 元素类型，按发现序）。
	irRetTypes map[string][]types.Type
	irRetOrder []string
}

// substT 应用当前上下文的类型代换（无代换时原样返回）。
func (c *Ctx) substT(t types.Type) types.Type {
	if len(c.substParams) == 0 || t == nil {
		return t
	}
	return types.Subst(t, c.substParams, c.substArgs)
}

// withSubst 在给定的类型代换下跑 f（嵌套实例化时按栈式恢复）。
func (c *Ctx) withSubst(params []string, args []types.Type, f func() error) error {
	sp, sa := c.substParams, c.substArgs
	c.substParams, c.substArgs = params, args
	defer func() { c.substParams, c.substArgs = sp, sa }()
	return f()
}

// Emit 是单文件入口: 纯函数, 同一 (f, path, info) 两次调用产出相同文本。
//
// **两遍**：第一遍只为发现泛型实例（实例定义必须在任何使用点之前发射，而某个实例
// 可能直到函数体里才第一次出现——例如 `match Some(v)` 的 T 由实参决定），第二遍
// 带着实例表正式发射。emit 是纯函数且很快，两遍远比"deferring type definitions past their use sites"安全。
func Emit(f *parse.File, path string, info *types.Info) (*Result, error) {
	return EmitUnit(Single(f, path, info))
}

// EmitUnit 是整程序入口（多包一份 C 翻译单元；红线 23）。
// 纯函数：同一 Unit 两次调用产出逐字节相同的文本（红线 12, H2）。
func EmitUnit(u *Unit) (*Result, error) {
	if u == nil || len(u.Files) == 0 {
		return nil, fmt.Errorf("emit: empty emission unit")
	}
	_, pre, err := runEmit(u, nil, nil)
	if err != nil {
		return nil, err
	}
	res, _, err := runEmit(u, pre, nil)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// seed 是第一遍的发现结果（泛型实例 + 合成打印函数 + 见证表），第二遍带着它正式发射。
type seed struct {
	insts      []instEntry
	printers   []printerEntry
	witnesses  []witnessEntry
	ifacePrint []string
	needed     []string
	spawn      []*spawnThunk
	// irRets/irRetT 是 IR 路径的多返回结构体（第一遍发现，第二遍在类型段统一发）。
	irRets []string
	irRetT map[string][]types.Type
}

// runEmit 跑一遍完整发射，返回产物与本次发现的实例/打印函数表。
// ir 非空 = 走 IR 路径（函数体从 IR 取；见 irbody.go）。
func runEmit(u *Unit, pre *seed, ir *IRProg) (*Result, *seed, error) {
	c := &Ctx{
		cur:       u.Files[0],
		Path:      u.Files[0].Path,
		File:      u.Files[0].File,
		Info:      u.Files[0].Info,
		declared:  map[string]string{},
		needed:    map[string]bool{},
		lamSites:  map[*parse.LambdaExpr]*lamSite{},
		deps:      map[string]*types.Info{},
		retSeen:   map[string]bool{},
		entryPath: u.Entry,
	}
	for _, fu := range u.Files {
		if fu.Info != nil && fu.Info.Pkg != "" {
			c.deps[fu.Info.Pkg] = fu.Info
		}
	}
	if ir != nil {
		// IR 路径：类型文本 → 语义类型表与「IR 符号 → C 符号」表都从检查产物重建
		// （跨包也在其中），于是后端不必解析类型文本、也不再写一份 mangling。
		c.ir = ir
		c.buildIRTypeTab()
		c.buildIRCallMap()
	}
	if pre != nil {
		c.seeded = true
		c.insts = append([]instEntry{}, pre.insts...)
		c.instSeen = make(map[string]bool, len(pre.insts))
		for _, e := range pre.insts {
			c.instSeen[e.name] = true
		}
		c.printers = append([]printerEntry{}, pre.printers...)
		c.printSeen = make(map[string]string, len(pre.printers))
		for _, e := range pre.printers {
			// **键方案必须与登记时一致**（红线 10）：接口打印器登记用的是
			// ifacePrintKey（`pkg.Name`），这里曾一律用 typeSeg ⇒ 接口键对不上、
			// 第二遍又想登记一次 ⇒ "printer for interface … only surfaced on the
			// second pass" —— 接口值打印因此完全不可用。
			if e.iface != nil {
				c.printSeen[ifacePrintKey(e.iface)] = e.name
				continue
			}
			c.printSeen[typeSeg(e.ty)] = e.name
		}
		c.printSeq = len(pre.printers)
		c.witnesses = append([]witnessEntry{}, pre.witnesses...)
		c.witnessSeen = make(map[string]bool, len(pre.witnesses))
		for _, w := range pre.witnesses {
			c.witnessSeen[w.name] = true
		}
		c.ifacePrint = make(map[string]bool, len(pre.ifacePrint))
		for _, k := range pre.ifacePrint {
			c.ifacePrint[k] = true
		}
		// 运行时能力（红线 16 按需链接）：第一遍发现，第二遍据此发 include。
		for _, k := range pre.needed {
			c.needed[k] = true
		}
		if ir != nil {
			// IR 路径的多返回结构体：第一遍发现，第二遍发 typedef。
			c.irRetOrder = append([]string{}, pre.irRets...)
			c.irRetTypes = map[string][]types.Type{}
			for k, v := range pre.irRetT {
				c.irRetTypes[k] = v
			}
		}
		// spawn thunk：第一遍发现，第二遍 TU 末尾统一发（调用点只发块作用域原型）。
		c.spawnThunks = append([]*spawnThunk{}, pre.spawn...)
		c.spawnSeen = make(map[string]bool, len(pre.spawn))
		for _, th := range pre.spawn {
			c.spawnSeen[th.name] = true
		}
	}
	// pass 1 + 2：逐文件建名字表与收集泛型实例（实例定义必须在任何使用点之前）。
	// IR 路径上实例由 IR 侧的 cTypeName 调用按需登记（两遍照旧：第一遍发现、第二遍带表）。
	for _, fu := range u.Files {
		c.use(fu)
		if ir == nil {
			c.collectInstances()
		}
	}
	c.use(u.Files[0])
	if err := c.prologue(); err != nil {
		return nil, nil, err
	}
	// ① 前向 typedef（全部包）→ ② 完整类型定义（全部包）
	for _, fu := range u.Files {
		c.use(fu)
		c.forwardTypes()
	}
	c.line("")
	for _, fu := range u.Files {
		c.use(fu)
		if err := c.emitTypeDefs(); err != nil {
			return nil, nil, err
		}
	}
	c.emitInstances()
	if err := c.emitPrinters(); err != nil {
		return nil, nil, err
	}
	if err := c.emitDerived(); err != nil {
		return nil, nil, err
	}
	if err := c.emitWitnesses(); err != nil {
		return nil, nil, err
	}
	c.collectFuncInsts(u)
	if err := c.emitFuncInsts(u); err != nil {
		return nil, nil, err
	}
	if ir == nil {
		c.emitSpawnThunkProtos()
	}
	c.emitPackedCmpProtos()
	instMethods, err := c.emitInstMethodProtos(u)
	if err != nil {
		return nil, nil, err
	}
	// ③ 全部原型 → ④ 全部函数体（跨包调用与声明序无关）
	for _, fu := range u.Files {
		c.use(fu)
		if err := c.emitProtos(); err != nil {
			return nil, nil, err
		}
	}
	// N10 委托方法的原型在全部原型之后（它们的体要调用被委托类的方法）。
	if err := c.emitDelegatedProtos(); err != nil {
		return nil, nil, err
	}
	if ir != nil {
		// IR 路径发现的多返回结构体：在原型段之后、函数体之前发 typedef。
		c.irEmitRetStructs()
		// IR 路径：体一律从 IR 取（含泛型实例与实例方法），不再走 AST。
		if err := c.emitIRFuncs(u); err != nil {
			return nil, nil, err
		}
		if err := c.emitIRInstFuncs(u); err != nil {
			return nil, nil, err
		}
	} else {
		for _, fu := range u.Files {
			c.use(fu)
			if err := c.emitFuncs(); err != nil {
				return nil, nil, err
			}
		}
		if err := c.emitFuncInstBodies(u); err != nil {
			return nil, nil, err
		}
		if err := c.emitInstMethodBodies(instMethods); err != nil {
			return nil, nil, err
		}
	}
	if eu := c.entryUnit(u); eu != nil {
		c.use(eu)
	}
	if err := c.emitDelegatedBodies(); err != nil {
		return nil, nil, err
	}
	c.emitEntry()
	if ir == nil {
		c.emitSpawnThunks() // spawn 薄 thunk 在 TU 末尾（调用点只发原型）
	}
	if err := c.emitPackedCmpBodies(); err != nil { // @packed 比较助手（同上：末尾定义）
		return nil, nil, err
	}
	c.epilogue()
	if ir != nil {
		// 收口校验：IR 里不该有没人要的函数体（有 = 签名侧与 IR 侧的符号对不上，
		// 是本类改动最容易犯的错，必须在编译期炸出来而不是静静丢掉一个函数）。
		if err := ir.verifyAllUsed(); err != nil {
			return nil, nil, err
		}
	}
	return &Result{C: c.buf.String(), Warnings: c.warnings, Needed: c.Needed(), Guards: c.guards},
		&seed{insts: c.insts, printers: c.printers, witnesses: c.witnesses,
			ifacePrint: c.sortedIfacePrint(), needed: c.Needed(), spawn: c.spawnThunks,
			irRets: c.irRetOrder, irRetT: c.irRetTypes}, nil
}

// sortedIfacePrint 把需要打印槽的接口键排序（确定性；map 序随机会破坏 H2）。
func (c *Ctx) sortedIfacePrint() []string {
	out := make([]string, 0, len(c.ifacePrint))
	for k := range c.ifacePrint {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- 输出原语 -----------------------------------------------------------------

func (c *Ctx) w(format string, args ...any) {
	fmt.Fprintf(&c.buf, format, args...)
}

func (c *Ctx) line(format string, args ...any) {
	if c.probe {
		return
	}
	c.w(format, args...)
	c.w("\n")
}

// srcLine 发射 #line, 使其后一行在 C 侧报错时回指 .aic 源行 (红线 9)。
func (c *Ctx) srcLine(pos parse.Pos) {
	line := pos.Line
	if line <= 0 {
		line = 1
	}
	c.line("#line %d %s", line, cstr(c.Path))
}

// warn 记录非阻断提示。
func (c *Ctx) warn(format string, args ...any) {
	c.warnings = append(c.warnings, fmt.Sprintf(format, args...))
}

// tmp 生成确定性临时名 (禁地址/随机: H2 要求两次构建逐字节一致)。
func (c *Ctx) tmp(prefix string) string {
	c.tmpSeq++
	return fmt.Sprintf("aic_t_%s_%d", prefix, c.tmpSeq)
}

// need 登记运行时能力 (L2 按需链接判据)。
func (c *Ctx) need(capability string) {
	c.needed[capability] = true
}

// Needed 返回本产物用到的运行时能力集合 (排序稳定)。
func (c *Ctx) Needed() []string {
	out := make([]string, 0, len(c.needed))
	for k := range c.needed {
		out = append(out, k)
	}
	return out
}

// --- 字面量与转义 ---------------------------------------------------------------

// cstr 把 Go 字符串转成 C 字符串字面量 (含引号)。
func cstr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch ch {
		case '"':
			b.WriteString("\\\"")
		case '\\':
			b.WriteString("\\\\")
		case '\n':
			b.WriteString("\\n")
		case '\r':
			b.WriteString("\\r")
		case '\t':
			b.WriteString("\\t")
		default:
			if ch < 0x20 {
				fmt.Fprintf(&b, "\\%03o", ch)
			} else {
				b.WriteByte(ch)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// strLit 返回一个 aic_str 字面量表达式, 字节入任务区域 (§二.3: str 字节恒任务区域)。
// 静态文本直接引用 C 字面量: 生命周期 = 程序全程, 不违反区域纪律。
func (c *Ctx) strLit(s string) string {
	return fmt.Sprintf("((aic_str){ %s, %d })", cstr(s), len(s))
}

// Escaped 暴露给测试: C 字符串转义。
func Escaped(s string) string { return cstr(s) }
