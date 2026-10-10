// AST → AIR（核心设计 §十 阶段 4「去糖」；检查点 = air）。
//
// 本文件负责**结构**：模块 / 类型表 / 函数骨架 / 直线语句 / 控制流；
// 表达式到值的提升（含副作用子表达式的 let 化）在 lower_expr.go。
//
// 去糖规则（§10.1）：
//   - 每函数 = 入口块 + 若干块 + **唯一收尾块**（V4.4：全部 ret 先 br 到收尾块）；
//   - `if` → cbr；`for`（三形态）→ 块 + br/cbr；`region` → region.enter/exit；
//   - 赋值 → `store`（守卫由检查器 GuardSpec 决定，是 store 的**修饰符**）；
//   - 创建表达式 → `alloc`（唯一分配来源；@live → alloc live）；
//   - 有副作用的子表达式一律提升为独立 `let`（求值顺序由语句序编码，§10.1）。
package air

import (
	"fmt"
	"sort"
	"strings"

	"aic/internal/parse"
	"aic/internal/types"
)

// LowerInput 是一次 lowering 的输入（一个包的全部文件 + 检查产物）。
type LowerInput struct {
	// Mono = 单态化层（mair）：泛型函数/方法的**每个实例**各降一份体。
	Mono bool

	Pkg     string
	Files   []*parse.File
	Info    *types.Info
	Imports []string
}

// Lower 把一个包降级为 AIR 模块（结构性去糖；表达式在 lower_expr.go）。
func Lower(in LowerInput) (*Module, error) {
	pkg := in.Pkg
	if in.Info != nil && in.Info.Pkg != "" {
		pkg = in.Info.Pkg // 包名唯一所有者 = 检查器（目录即包）
		in.Pkg = pkg
	}
	l := &lowerer{
		mod:  &Module{Pkg: pkg, Imports: in.Imports},
		in:   in,
		info: in.Info,
		seen: map[string]bool{},
	}
	if err := l.types(); err != nil {
		return nil, err
	}
	for _, f := range in.Files {
		for _, d := range f.Decls {
			switch v := d.(type) {
			case *parse.FuncDecl:
				if v.Body == nil {
					continue // extern 声明不产出 AIR 体
				}
				if len(v.TypeParams) > 0 {
					// 泛型函数：**每个实例各降一份体**（mair）；air 层跳过模板。
					if !in.Mono {
						continue
					}
					for _, inst := range instancesOf(in.Info, "", v.Name) {
						if err := l.lowerInstance(v, "", v.TypeParams, inst); err != nil {
							return nil, err
						}
					}
					continue
				}
				if err := l.funcDecl(v, ""); err != nil {
					return nil, err
				}
			case *parse.ClassDecl:
				for _, m := range v.Methods {
					if m.Body == nil {
						continue
					}
					if len(v.TypeParams) > 0 {
						// 泛型类的方法：按**类的实例**逐个降级（方法继承类的形参）。
						if !in.Mono {
							continue
						}
						for _, inst := range instancesOf(in.Info, v.Name, m.Name) {
							if err := l.lowerInstance(m, v.Name, v.TypeParams, inst); err != nil {
								return nil, err
							}
						}
						continue
					}
					if err := l.funcDecl(m, v.Name); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return l.mod, nil
}

// --- 单态化实例上下文（mair 用） -------------------------------------------
//
// instParams/instArgs 非空 = 正在降级一个泛型**实例**：所有类型先按
// (instParams → instArgs) 代换再渲染/使用。这一层是"泛型体只写一次、实例各降一份"
// 的落点：约束授权的方法（`x.compare(y)`，x : T）在代换后接收者就是具体类，
// 后续解析（callee / fieldIndex）自然落到具体方法上 —— 与直译路径的 substT 同一套规则。
func (l *lowerer) sub(t types.Type) types.Type {
	if len(l.instParams) == 0 || t == nil {
		return t
	}
	return types.Subst(t, l.instParams, l.instArgs)
}

// ty 渲染一个语义类型的 AIR 文本（实例上下文下先代换）。
//
// 后端把类型文本翻成 C 类型名时**不解析文本**：它在检查产物（types.Info）上
// 用 air.TyText 重建「文本 → 语义类型」表，于是 mangling 与容器后缀仍只有一处实现。
func (l *lowerer) ty(t types.Type) string { return tyText(l.sub(t)) }

// typeOf 查一个表达式的类型（实例上下文下先代换）。
//
// 未定型字面量检查器不给类型（§一：定宽推迟到使用点），但**方法调用**要用接收者类型
// （`"abc".len()`）：这里补一条与 types.LiteralTy 同源的兜底，否则接收者类型为 nil，
// 降级只能落到 `unknown` 这种不可执行的符号（685/717 语料实测）。
func (l *lowerer) typeOf(e parse.Expr) types.Type {
	if l.info == nil {
		return nil
	}
	if t := l.sub(l.info.LookupType(e)); t != nil {
		return t
	}
	switch v := e.(type) {
	case *parse.StrLit:
		return types.TStr
	case *parse.BoolLit:
		return types.TBool
	case *parse.FloatLit:
		return types.TF64
	case *parse.IntLit:
		if types.LiteralTy(v.Text) == "i64" {
			return types.TI64
		}
		return types.TI32
	}
	return nil
}

type lowerer struct {
	mod  *Module
	in   LowerInput
	info *types.Info
	seen map[string]bool

	fn        *Func
	cur       *Block
	exitLabel string
	tmpSeq    int
	blkSeq    int
	// deferSeq 是本函数的 defer 注册点编号（按源序从 1 起，确定性）；
	// deferErr 记录本函数是否出现过 errdefer（收尾块据此决定要不要判 Err）。
	deferSeq int
	deferErr bool
	depth    int // 当前 region 嵌套深度（守卫 limit 的计算依据）
	loops    []loopCtx
	// blockVars 记录每个 AIR 块里已声明的变量名（V2.6 的判据源）。循环变量若与
	// **同一块**里的外层变量同名 ⇒ 重命名成 fresh 名并走 loopAlias 改写体内引用
	// （审计 D1：`var v i64 = 1; for v in xs` 旧行为 = V2.6 误报"重复声明"）。
	// 只在真冲突时改名 ⇒ 无冲突程序的 IR 快照不动（H8）。
	blockVars  map[*Block]map[string]bool
	loopAlias  map[string]string
	renameSeq  int
	lamSeq   int // lambda 提升编号（确定性）
	// envCur 非空 = 正在降级一个闭包体：捕获引用读 `field envCur, <槽>`（N1）。
	envCur string
	// instParams/instArgs 非空 = 正在降级一个泛型**实例**（mair 层）：类型先代换再渲染。
	instParams []string
	instArgs   []types.Type
	// symOverride 非空 = 函数符号用实例名 `<base>[args]`（单态化实例各占一个符号）。
	symOverride string
	// fnKey 是当前函数的 Locals 键（defer 块形的捕获分析用它取外层局部量表）。
	fnKey string
	// dfblkSeq 是 defer 块形提升体的**模块级**编号（deferSeq 每函数重置，
	// 提升体符号跨函数唯一 ⇒ 必须有一个不随 begin/end 归零的计数器）。
	dfblkSeq int
}

// emitInstanceTypes 发泛型类的**实例**声明（mair）：每个实例一份 struct，
// 字段类型按实例实参代换。实例集合来自检查器登记的函数实例（泛型类的实例必然
// 出现在某个实例体里）—— 逐字段递归收集，不猜。
func (l *lowerer) emitInstanceTypes() {
	if l.info == nil {
		return
	}
	type key struct{ sym string }
	seen := map[key]bool{}
	var emit func(t types.Type)
	emit = func(t types.Type) {
		inst, ok := t.(*types.Instance)
		if !ok {
			return
		}
		cl, isCl := types.IsClass(inst.Base)
		if !isCl || len(cl.TypeParams) == 0 {
			return
		}
		argsText := make([]string, 0, len(inst.Args))
		for _, a := range inst.Args {
			argsText = append(argsText, tyText(a))
			emit(a)
		}
		sym := mangleType(cl.Pkg, cl.Name) + "[" + strings.Join(argsText, ", ") + "]"
		if seen[key{sym}] {
			return
		}
		seen[key{sym}] = true
		td := &TypeDecl{Sym: sym, Kind: "struct", Packed: cl.Packed}
		for i := range cl.Fields {
			ft := types.Subst(cl.Fields[i].Type, cl.TypeParams, inst.Args)
			td.Fields = append(td.Fields, Field{Idx: i, Name: cl.Fields[i].Name, Ty: tyText(ft)})
		}
		l.mod.Types = append(l.mod.Types, td)
	}
	// ① 检查器登记的实例实参
	for _, fi := range l.info.FuncInsts {
		for _, a := range fi.Args {
			emit(a)
		}
	}
	// ② 表达式类型表里出现的实例（含声明位、字面量位）
	for _, t := range l.info.Types {
		emit(t)
	}
	// ③ 字段/签名里出现的实例（构造位可能没有实例记录）
	for _, cl := range l.info.Classes {
		for i := range cl.Fields {
			emit(cl.Fields[i].Type)
		}
	}
	sort.Slice(l.mod.Types, func(i, j int) bool { return l.mod.Types[i].Sym < l.mod.Types[j].Sym })
}

// --- 类型表 -------------------------------------------------------------------

func (l *lowerer) types() error {
	if l.info == nil {
		return fmt.Errorf("air: lowering needs the checker output")
	}
	for _, name := range sortedKeys(l.info.Classes) {
		cl := l.info.Classes[name]
		if len(cl.TypeParams) > 0 {
			// 泛型类：air 层发**模板**（字段类型含类型形参）；mair 层按实例各发一份。
			if l.in.Mono {
				continue // 实例声明由 emitInstanceTypes 发（下面）
			}
		}
		td := &TypeDecl{Sym: mangleType(cl.Pkg, cl.Name), Kind: "struct", Packed: cl.Packed}
		for i := range cl.Fields {
			td.Fields = append(td.Fields, Field{Idx: i, Name: cl.Fields[i].Name, Ty: l.ty(cl.Fields[i].Type)})
		}
		l.mod.Types = append(l.mod.Types, td)
	}
	if l.in.Mono {
		l.emitInstanceTypes()
	}
	for _, name := range sortedKeys(l.info.Enums) {
		en := l.info.Enums[name]
		td := &TypeDecl{Sym: mangleType(en.Pkg, en.Name), Kind: "enum"}
		for i, va := range en.Variants {
			pl := ""
			if va.Payload != nil {
				pl = l.ty(va.Payload)
			}
			td.Variants = append(td.Variants, Variant{Tag: i, Name: va.Name, Payload: pl})
		}
		l.mod.Types = append(l.mod.Types, td)
	}
	for _, name := range sortedKeys(l.info.Interfaces) {
		ifc := l.info.Interfaces[name]
		td := &TypeDecl{Sym: mangleType(ifc.Pkg, ifc.Name), Kind: "iface"}
		for i, slot := range types.InterfaceSlots(ifc) {
			sig, _ := ifc.Method(slot)
			td.Slots = append(td.Slots, Slot{Idx: i, Name: slot, Sig: sigText(sig)})
		}
		l.mod.Types = append(l.mod.Types, td)
	}
	return nil
}

// --- 函数骨架 -----------------------------------------------------------------

func (l *lowerer) funcDecl(d *parse.FuncDecl, recv string) error {
	sig := l.sigOf(d, recv)
	if sig == nil {
		return fmt.Errorf("air: no signature for %s", d.Name)
	}
	sym := mangleFunc(l.in.Pkg, recv, d.Name)
	if l.symOverride != "" {
		sym = l.symOverride
	}
	l.fnKey = types.FuncKey(recv, d.Name)
	f := &Func{Sym: sym, Loc: LocOf(d.Pos)}
	if sig.Export {
		f.Flags = append(f.Flags, "export")
	}
	if recv != "" {
		// 接收者类型：普通方法 = 类本身；**实例方法**（mair）= 该实例（`ok_Box*[i32]`）
		// —— 否则实例体的 this__ 会停在模板类型上，与调用点的实参类型对不上。
		rt := l.recvType(recv)
		if len(l.instArgs) > 0 && rt != nil {
			rt = &types.Instance{Base: rt, Args: l.instArgs}
		}
		f.Params = append(f.Params, Param{Name: "this__", Ty: l.ty(rt)})
	}
	for i, p := range sig.Params {
		ty := "i32"
		if i < len(sig.ParamTypes) && sig.ParamTypes[i] != nil {
			ty = l.ty(sig.ParamTypes[i])
		}
		f.Params = append(f.Params, Param{Name: p, Ty: ty})
	}
	for _, r := range sig.Results {
		f.Rets = append(f.Rets, l.ty(r))
	}
	entry := l.newBlock("entry", d.Pos)
	f.Blocks = append(f.Blocks, entry)
	saved := l.begin(f, entry, d.Pos)
	// V2.6 判据源：形参 / this / 返回槽都住在入口块
	for _, p := range f.Params {
		l.noteBlockVar(p.Name)
	}
	for i := range f.Rets {
		l.noteBlockVar(fmt.Sprintf("ret%d", i))
	}
	// 返回槽（§10.1 V4.4）：全部出口先 store 进槽再 br 到唯一收尾块，
	// 收尾块统一 ret —— 无 phi 也满足「每临时量恰定义一次」。
	for i, r := range f.Rets {
		entry.Insts = append(entry.Insts, &Var{Name: fmt.Sprintf("ret%d", i), Ty: r, Loc: LocOf(d.Pos)})
	}
	if err := l.block(d.Body); err != nil {
		return err
	}
	// 体末落空（无 return）= 隐式落到唯一收尾块（V2.1：每块恰一个终结符）。
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: l.exitLabel, Loc: LocOf(d.Pos)}
	}
	l.finish(f, d.Pos)
	l.end(saved)
	l.mod.Funcs = append(l.mod.Funcs, f)
	return nil
}

// begin/end 切换当前函数上下文（嵌套 lowering 时栈式恢复）。
type fnState struct {
	fn        *Func
	cur       *Block
	exitLabel string
	depth     int
	tmpSeq    int
	blkSeq    int
	deferSeq  int
	deferErr  bool
	blockVars map[*Block]map[string]bool
	loopAlias map[string]string
	renameSeq int
}

func (l *lowerer) begin(f *Func, entry *Block, at parse.Pos) fnState {
	st := fnState{fn: l.fn, cur: l.cur, exitLabel: l.exitLabel, depth: l.depth,
		tmpSeq: l.tmpSeq, blkSeq: l.blkSeq, deferSeq: l.deferSeq, deferErr: l.deferErr,
		blockVars: l.blockVars, loopAlias: l.loopAlias, renameSeq: l.renameSeq}
	l.fn, l.cur, l.exitLabel, l.depth = f, entry, "exit", 0
	l.tmpSeq, l.blkSeq, l.deferSeq, l.deferErr = 0, 0, 0, false
	l.blockVars, l.loopAlias, l.renameSeq = map[*Block]map[string]bool{}, map[string]string{}, 0
	return st
}

func (l *lowerer) end(st fnState) {
	l.fn, l.cur, l.exitLabel, l.depth = st.fn, st.cur, st.exitLabel, st.depth
	l.tmpSeq, l.blkSeq = st.tmpSeq, st.blkSeq
	l.deferSeq, l.deferErr = st.deferSeq, st.deferErr
	l.blockVars, l.loopAlias, l.renameSeq = st.blockVars, st.loopAlias, st.renameSeq
}

// airVarName 取一个源变量名在 AIR 里的名字（循环变量重命名时走别名表，
// 其余原名）。放在 lowerer 上的唯一查找点 = value/place 的 Ident 分支。
func (l *lowerer) airVarName(src string) string {
	if a, ok := l.loopAlias[src]; ok {
		return a
	}
	return src
}

// noteBlockVar 记录一个变量声明落在当前块（V2.6 的判据源；参数与返回槽在入口块）。
func (l *lowerer) noteBlockVar(name string) {
	if name == "" {
		return
	}
	if l.blockVars[l.cur] == nil {
		l.blockVars[l.cur] = map[string]bool{}
	}
	l.blockVars[l.cur][name] = true
}

// loopVarName 取循环变量在 AIR 里的名字：与当前块已有变量同名时重命名
// （源级遮蔽合法，但 AIR 的 Var 全落在同块会被 V2.6 误报），并登记 loopAlias
// 供循环体里的引用改写。返回值 = AIR 名。
func (l *lowerer) loopVarName(src string, wantAlias bool) string {
	if src == "" || src == "_" {
		return src
	}
	if l.blockVars[l.cur] != nil && l.blockVars[l.cur][src] {
		l.renameSeq++
		fresh := fmt.Sprintf("%s__l%d", src, l.renameSeq)
		if wantAlias {
			if l.loopAlias == nil {
				l.loopAlias = map[string]string{}
			}
			l.loopAlias[src] = fresh
		}
		l.noteBlockVar(fresh)
		return fresh
	}
	l.noteBlockVar(src)
	return src
}

// unaliasLoopVar 循环体降级结束：撤掉本层循环的循环变量别名（外层遮蔽关系恢复）。
func (l *lowerer) unaliasLoopVar(src, airName string) {
	if airName != src {
		delete(l.loopAlias, src)
	}
}

// finish 发射唯一收尾块：全部出口先 br 到它（V4.4），由它执行 ret；
// 同时**剪掉不可达块**（V2.3：全路径 return 之后的延续代码是死代码，直接丢弃）。
func (l *lowerer) finish(f *Func, at parse.Pos) {
	l.pruneUnreachable(f)
	exit := &Block{Label: l.exitLabel, Loc: LocOf(at)}
	// defer 是**函数级**的（region 块退出不触发）：全部出口都汇到唯一收尾块，
	// 所以 defer 栈只在这里跑一次（LIFO）。errdefer 项只在返回的 Err 非零时跑，
	// 故收尾块要把末位返回槽（Err）交给 DeferRun 当判据。
	if l.deferSeq > 0 {
		run := &DeferRun{Loc: LocOf(at)}
		if l.deferErr && len(f.Rets) > 0 && f.Rets[len(f.Rets)-1] == "Err" {
			run.Err = fmt.Sprintf("ret%d", len(f.Rets)-1)
		}
		exit.Insts = append(exit.Insts, run)
	}
	if len(f.Rets) == 0 {
		exit.Term = &Ret{Loc: LocOf(at)}
	} else {
		vals := make([]string, 0, len(f.Rets))
		for i := range f.Rets {
			vals = append(vals, fmt.Sprintf("ret%d", i))
		}
		exit.Term = &Ret{Vals: vals, Loc: LocOf(at)}
	}
	f.Blocks = append(f.Blocks, exit)
}

// pruneUnreachable 剪掉从入口不可达的块（V2.3）。
func (l *lowerer) pruneUnreachable(f *Func) {
	if len(f.Blocks) == 0 {
		return
	}
	byLabel := map[string]*Block{}
	for _, b := range f.Blocks {
		byLabel[b.Label] = b
	}
	reach := map[string]bool{}
	var walk func(string)
	walk = func(label string) {
		if reach[label] {
			return
		}
		reach[label] = true
		b := byLabel[label]
		if b == nil {
			return
		}
		for _, tgt := range termTargets(b.Term) {
			walk(tgt)
		}
	}
	walk(f.Blocks[0].Label)
	kept := make([]*Block, 0, len(f.Blocks))
	for _, b := range f.Blocks {
		if reach[b.Label] {
			kept = append(kept, b)
		}
	}
	f.Blocks = kept
}

// newBlock 建一个新块并登记（名字确定性：b1、b2…）。
func (l *lowerer) newBlock(prefix string, at parse.Pos) *Block {
	l.blkSeq++
	label := prefix
	if prefix != "entry" && prefix != "exit" {
		label = fmt.Sprintf("%s%d", prefix, l.blkSeq)
	}
	b := &Block{Label: label, Loc: LocOf(at)}
	if l.fn != nil && prefix != "entry" {
		l.fn.Blocks = append(l.fn.Blocks, b)
	}
	return b
}

// tmp 生成确定性临时量名。
func (l *lowerer) tmp(prefix string) string {
	l.tmpSeq++
	return fmt.Sprintf("%s%d", prefix, l.tmpSeq)
}

// --- 语句 ---------------------------------------------------------------------

func (l *lowerer) block(b *parse.Block) error {
	if b == nil {
		return nil
	}
	for _, s := range b.Stmts {
		if err := l.stmt(s); err != nil {
			return err
		}
	}
	return nil
}

func (l *lowerer) stmt(s parse.Stmt) error {
	switch v := s.(type) {
	case *parse.VarDecl:
		return l.varDecl(v)
	case *parse.ExprStmt:
		// 赋值在 AIC 里是表达式（§三），但只能作语句用（赋值非表达式值）
		switch x := v.X.(type) {
		case *parse.Assign:
			return l.assign(x)
		case *parse.MultiAssign:
			return l.multiAssign(x)
		}
		_, err := l.value(v.X)
		return err
	case *parse.ReturnStmt:
		return l.ret(v)
	case *parse.IfStmt:
		return l.ifStmt(v)
	case *parse.ForStmt:
		return l.forStmt(v)
	case *parse.Block:
		// 嵌套源块 → 新 AIR 块：遮蔽由块结构显式化（V2.6 的本意）。
		inner := l.newBlock("scope", v.Pos)
		l.cur.Term = &Br{Label: inner.Label, Loc: LocOf(v.Pos)}
		l.cur = inner
		if err := l.block(v); err != nil {
			return err
		}
		out := l.newBlock("scopeend", v.Pos)
		if l.cur.Term == nil {
			l.cur.Term = &Br{Label: out.Label, Loc: LocOf(v.Pos)}
		}
		l.cur = out
		return nil
	case *parse.RegionStmt:
		return l.regionStmt(v)
	case *parse.BranchStmt:
		return l.branchStmt(v)
	case *parse.DeferStmt:
		return l.deferStmt(v, false)
	case *parse.ErrDeferStmt:
		return l.deferStmt(&parse.DeferStmt{Call: v.Call, Block: v.Block, Pos: v.Pos}, true)
	case *parse.MatchStmt:
		return l.matchStmt(v)
	case *parse.ScopeStmt:
		if v.IsTimeout {
			if err := l.scopeTimeoutStmt(v); err != nil {
				return err
			}
		}
		return l.scopeStmt(v)
	case *parse.SelectStmt:
		return l.selectStmt(v)
	case *parse.SpawnStmt:
		return l.spawnStmt(v)
	}
	return fmt.Errorf("air: unsupported statement %T", s)
}

// varDecl：`var x : T = v`；创建表达式走 alloc + 逐字段 store。
func (l *lowerer) varDecl(v *parse.VarDecl) error {
	if len(v.Targets) != 1 {
		return l.multiBind(v) // var a, err = f() → call + multi.extract + var
	}
	tgt := v.Targets[0]
	ty := l.declType(v)
	inst := &Var{Name: tgt.Name, Ty: ty, Live: tgt.Live, Loc: LocOf(v.Pos)}
	if v.Init != nil {
		val, err := l.value(v.Init)
		if err != nil {
			return err
		}
		// 槽位是接口而值是具体类 ⇒ 显式装箱（`var s Shape = rect`）。
		// **只在声明类型是显式写出时**才做：推断形态下槽位类型就是值的类型（无转换），
		// 而 declType 的 `i32` 兜底不能当真（见 slotTextKnown）。
		slot := ""
		if v.Type != nil {
			if t := typeExprOf(v.Type, l.info); t != nil {
				slot = l.ty(t)
			}
		}
		inst.Init = l.coerce(val, v.Init, slot)
	}
	l.cur.Insts = append(l.cur.Insts, inst)
	l.noteBlockVar(tgt.Name)
	return nil
}

// assign：`x = v` / `x += v` → store（守卫按检查器 GuardSpec 作为 store 修饰符）。
func (l *lowerer) assign(v *parse.Assign) error {
	place, err := l.place(v.LHS)
	if err != nil {
		return err
	}
	val, err := l.value(v.Right)
	if err != nil {
		return err
	}
	if v.Op == "=" || v.Op == "" {
		// 槽位是接口而值是具体类 ⇒ 显式装箱（`s = rect`）。槽位类型未知时不猜。
		val = l.coerce(val, v.Right, l.slotTextKnown(v.LHS))
	}
	if v.Op != "=" && v.Op != "" {
		// 复合赋值：先读再算（读路径走 place 的 rhs 形态）
		cur, err := l.placeValue(place, v.Pos)
		if err != nil {
			return err
		}
		t := l.tmp("t")
		op := v.Op[:len(v.Op)-1]
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: l.placeType(v.LHS), Rhs: &Binop{Op: op, A: cur, B: val}, Loc: LocOf(v.Pos)})
		val = t
	}
	st := &Store{Place: place, Val: val, Loc: LocOf(v.Pos)}
	// 守卫标记挂在**值表达式**上（与 emit 的 emitGuardStore(value, target, delta) 同源），
	// 故先看 RHS；LHS 形态只在少数位置带标记（容器方法的实参即语义宿主，§五 R3）。
	g, ok := l.info.HasGuard(v.Right)
	if !ok {
		g, ok = l.info.HasGuard(v.LHS)
	}
	if ok {
		st.Limit = l.limitOf(g, v.LHS)
	}
	l.cur.Insts = append(l.cur.Insts, st)
	return nil
}

// ret：先算出返回值临时量，再 br 到唯一收尾块（V4.4）。
func (l *lowerer) ret(v *parse.ReturnStmt) error {
	for i, r := range v.Results {
		val, err := l.value(r)
		if err != nil {
			return err
		}
		// 返回槽是接口而值是具体类 ⇒ 显式装箱。
		if l.fn != nil && i < len(l.fn.Rets) {
			val = l.coerce(val, r, l.fn.Rets[i])
		}
		// 写进返回槽（入口块声明的那一个），不产生新定义（V2.5）。
		l.cur.Insts = append(l.cur.Insts, &Store{
			Place: &VarPlace{Name: fmt.Sprintf("ret%d", i)}, Val: val, Loc: LocOf(v.Pos)})
	}
	l.cur.Term = &Br{Label: l.exitLabel, Loc: LocOf(v.Pos)}
	return nil
}

// ifStmt：cbr + then/else 块，两支汇入 join 块。
func (l *lowerer) ifStmt(v *parse.IfStmt) error {
	cond, err := l.value(v.Cond)
	if err != nil {
		return err
	}
	thenB := l.newBlock("then", v.Pos)
	elseB := l.newBlock("else", v.Pos)
	join := l.newBlock("join", v.Pos)
	l.cur.Term = &Cbr{Cond: cond, Then: thenB.Label, Else: elseB.Label, Loc: LocOf(v.Pos)}

	l.cur = thenB
	if err := l.block(v.Then); err != nil {
		return err
	}
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: join.Label, Loc: LocOf(v.Pos)}
	}
	l.cur = elseB
	if v.Else != nil {
		if err := l.block(v.Else); err != nil {
			return err
		}
	}
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: join.Label, Loc: LocOf(v.Pos)}
	}
	l.cur = join
	return nil
}

// forStmt：三形态统一成 头块 / 体块 / 后块 / 出口（cond 形与区间形都归约到 cbr）。
func (l *lowerer) forStmt(v *parse.ForStmt) error {
	if v.IsRange {
		return l.rangeFor(v)
	}
	if v.Init != nil {
		if err := l.stmt(v.Init); err != nil {
			return err
		}
	}
	head := l.newBlock("forhead", v.Pos)
	body := l.newBlock("forbody", v.Pos)
	post := l.newBlock("forpost", v.Pos)
	done := l.newBlock("fordone", v.Pos)
	l.cur.Term = &Br{Label: head.Label, Loc: LocOf(v.Pos)}

	l.cur = head
	if v.Cond != nil {
		cond, err := l.value(v.Cond)
		if err != nil {
			return err
		}
		head.Term = &Cbr{Cond: cond, Then: body.Label, Else: done.Label, Loc: LocOf(v.Pos)}
	} else {
		head.Term = &Br{Label: body.Label, Loc: LocOf(v.Pos)}
	}
	l.cur = body
	l.loops = append(l.loops, loopCtx{post: post.Label, done: done.Label})
	if err := l.block(v.Body); err != nil {
		return err
	}
	l.loops = l.loops[:len(l.loops)-1]
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: post.Label, Loc: LocOf(v.Pos)}
	}
	l.cur = post
	if v.Post != nil {
		if err := l.stmt(v.Post); err != nil {
			return err
		}
	}
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: head.Label, Loc: LocOf(v.Pos)}
	}
	l.cur = done
	return nil
}

// rangeFor：`for i in a..b` → 下标循环（init / cond / post 三块，语义 = 半开区间）。
func (l *lowerer) rangeFor(v *parse.ForStmt) error {
	// 容器/字符串迭代（RangeEnd == nil）走下标循环；`a..b` 走区间循环。
	if v.RangeEnd == nil {
		return l.rangeForContainer(v)
	}
	if len(v.Names) != 1 {
		return fmt.Errorf("air: a range for takes exactly one loop variable (line %d)", v.Pos.Line)
	}
	lo, err := l.value(v.RangeX)
	if err != nil {
		return err
	}
	hi, err := l.value(v.RangeEnd)
	if err != nil {
		return err
	}
	name := l.loopVarName(v.Names[0].Name, true)
	l.cur.Insts = append(l.cur.Insts, &Var{Name: name, Ty: "usize", Init: lo, Loc: LocOf(v.Pos)})
	head := l.newBlock("forhead", v.Pos)
	body := l.newBlock("forbody", v.Pos)
	post := l.newBlock("forpost", v.Pos)
	done := l.newBlock("fordone", v.Pos)
	l.cur.Term = &Br{Label: head.Label, Loc: LocOf(v.Pos)}

	l.cur = head
	c := l.tmp("t")
	head.Insts = append(head.Insts, &Let{Tmp: c, Ty: "bool", Rhs: &Cmp{Op: "lt", A: name, B: hi}, Loc: LocOf(v.Pos)})
	head.Term = &Cbr{Cond: c, Then: body.Label, Else: done.Label, Loc: LocOf(v.Pos)}

	l.cur = body
	l.loops = append(l.loops, loopCtx{post: post.Label, done: done.Label})
	if err := l.block(v.Body); err != nil {
		return err
	}
	l.loops = l.loops[:len(l.loops)-1]
	l.unaliasLoopVar(v.Names[0].Name, name)
	if l.cur.Term == nil {
		l.cur.Term = &Br{Label: post.Label, Loc: LocOf(v.Pos)}
	}
	l.cur = post
	n := l.tmp("t")
	post.Insts = append(post.Insts, &Let{Tmp: n, Ty: "usize", Rhs: &Binop{Op: "+", A: name, B: "const 1"}, Loc: LocOf(v.Pos)})
	post.Insts = append(post.Insts, &Store{Place: &VarPlace{Name: name}, Val: n, Loc: LocOf(v.Pos)})
	post.Term = &Br{Label: head.Label, Loc: LocOf(v.Pos)}
	l.cur = done
	return nil
}

// regionStmt：region.enter / region.exit（与作用域严格配对，V3.1）。
func (l *lowerer) regionStmt(v *parse.RegionStmt) error {
	l.depth++
	l.cur.Insts = append(l.cur.Insts, &RegionEnter{TypeID: l.tmp("r"), Loc: LocOf(v.Pos)})
	if err := l.block(v.Body); err != nil {
		return err
	}
	l.depth--
	l.cur.Insts = append(l.cur.Insts, &RegionExit{Loc: LocOf(v.Pos)})
	return nil
}

// deferStmt：defer.reg / defer.init（注册点求值）；onErr = N8 的 errdefer
// （同一个栈，只是只在因 Err 返回时执行）。
//
// 被调者与实参**都在这里定下来**：后端只按 `Callee(Vals…)` 发一个薄 trampoline +
// 上下文结构体（与直译路径同一套运行期 ABI），不必再认识调用形态的差别。
func (l *lowerer) deferStmt(v *parse.DeferStmt, onErr bool) error {
	if v.Block != nil {
		// 块形 defer：提升体 + 按引用捕获（见 lower_deferblk.go）。
		return l.deferBlockStmt(v, onErr)
	}
	call, ok := v.Call.(*parse.Call)
	if !ok {
		return fmt.Errorf("air: defer must be followed by a call or a block (line %d)", v.Pos.Line)
	}
	sym, ta := l.callee(call)
	if sym == "" || sym == "unknown" {
		return fmt.Errorf("air: cannot resolve the deferred call target (line %d)", v.Pos.Line)
	}
	vals := []string{}
	limits := []string{}
	// 方法形态：接收者是首实参（求值时机 = 注册点，与直调一致）。
	if f, isField := call.Fn.(*parse.Field); isField && l.methodRecv(f.X) {
		rv, err := l.value(f.X)
		if err != nil {
			return err
		}
		vals = append(vals, rv)
		limits = append(limits, l.guardLimitOf(f.X))
	}
	for _, a := range call.Args {
		val, err := l.deferVal(a)
		if err != nil {
			return err
		}
		vals = append(vals, val)
		limits = append(limits, l.guardLimitOf(a))
	}
	l.deferSeq++
	if onErr {
		l.deferErr = true
	}
	l.cur.Insts = append(l.cur.Insts, &DeferReg{OnErr: onErr, Loc: LocOf(v.Pos)})
	l.cur.Insts = append(l.cur.Insts, &DeferInit{
		ID: l.deferSeq, Callee: sym, TypeArgs: ta, Vals: vals, Limits: limits,
		OnErr: onErr, Loc: LocOf(v.Pos)})
	return nil
}

// guardLimitOf 取一个表达式上的存储点守卫的 IR limit 文本（没有守卫 = 空串）。
// defer/errdefer 实参的界 ≤ 函数入口（§五 R5），检查器把 GuardDefer 标在实参
// 节点上；此前降级侧从不查它 ⇒ 守卫从未发出（781：defer 退出时读已弹出的区域）。
func (l *lowerer) guardLimitOf(e parse.Expr) string {
	if g, ok := l.info.HasGuard(e); ok {
		return l.limitOf(g, e)
	}
	return ""
}

// deferVal 求值一个 defer 实参：**字面量先绑成临时量**。
// 理由：C 侧的 defer 上下文是结构体，字段类型必须显式写出，而 `const 3` 这种
// "值名"本身不带类型（普通调用不需要，因为 C 会按原型推断）。
func (l *lowerer) deferVal(e parse.Expr) (string, error) {
	val, err := l.value(e)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(val, "const ") {
		return val, nil
	}
	lit := strings.TrimPrefix(val, "const ")
	// 类型：检查器有记录就用它（`const 3` 在 `f(x i64)` 位上是 i64 还是 i32 由语义定），
	// 没有记录（未定型字面量）才按 §一 的最窄规则定 —— 同一条规则由 types 持有。
	ty := types.LiteralTy(lit)
	if t := l.typeOf(e); t != nil {
		ty = l.ty(t)
	}
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{
		Tmp: t, Ty: ty,
		Rhs: &Const{Lit: lit}, Loc: LocOf(parse.ExprPos(e))})
	return t, nil
}

// methodRecv 报告 `x.m(…)` 的 x 是否是"值接收者"（方法调用：接收者当首实参）。
// 与调用降级用**同一条**判定（红线 10：两处各自判断迟早会分叉）。
//
// **标量接收者也算**：语言面标量没有任何内建方法，故出现在标量上的方法调用只有
// 一种可能 = N3 约束授权的方法（`[T: Ord]` 的 a.compare(b)）。它们同样把接收者
// 当首实参（约束签名 compare(other) 之外隐含 this）—— 漏了这条，实例降级会把
// `a.compare(b)` 发成 `opaque_compare(b)`（少一个实参，762 实测）。
func (l *lowerer) methodRecv(x parse.Expr) bool {
	rt := l.typeOf(x)
	if rt == nil {
		return false
	}
	if _, isCl := types.IsClass(rt); isCl {
		return true
	}
	if _, isInst := rt.(*types.Instance); isInst {
		return true
	}
	if _, isCh := rt.(*types.ChanT); isCh {
		return true
	}
	if _, isMu := rt.(*types.MutexT); isMu {
		return true
	}
	if _, isBasic := rt.(*types.Basic); isBasic {
		return true
	}
	return types.IsSlice(rt) || types.IsMap(rt) || types.IsSet(rt) || types.IsArray(rt) ||
		types.IsStrType(rt) || types.IsBytes(rt)
}

// --- 辅助 ---------------------------------------------------------------------

func (l *lowerer) sigOf(d *parse.FuncDecl, recv string) *types.FuncSig {
	if recv == "" {
		sig, _ := l.info.Funcs[d.Name]
		return sig
	}
	if cl, ok := l.info.Classes[recv]; ok {
		sig, _ := cl.Method(d.Name)
		return sig
	}
	return nil
}

func (l *lowerer) recvType(recv string) types.Type {
	if cl, ok := l.info.Classes[recv]; ok {
		return cl
	}
	return nil
}

// declType 取声明类型（显式优先，否则用检查器记下的初值类型）。
func (l *lowerer) declType(v *parse.VarDecl) string {
	if v.Type != nil {
		if t := typeExprOf(v.Type, l.info); t != nil {
			return l.ty(t)
		}
	}
	if v.Init != nil {
		if t := l.typeOf(v.Init); t != nil {
			return l.ty(t)
		}
	}
	return "i32"
}

// placeType 取一个 lvalue 表达式的类型文本（复合赋值用）。
func (l *lowerer) placeType(e parse.Expr) string {
	if t := l.typeOf(e); t != nil {
		return l.ty(t)
	}
	return "i32"
}

// limitOf 把检查器 GuardSpec 翻成文法里的 <limit>（V5.2：形态与目标种类匹配）。
// limitOf 把检查器的守卫标记渲染成 IR 的 limit 文本（**受限文法**，见 ir.go 的 Store）：
//
//	fnentry   返回点守卫：界 ≤ 函数入口（运行时比 0u）
//	depth-N   界 = 当前深度 − N（N = 检查器记下的声明偏移）
//	depth     界 = 当前深度
//	host      界 = 宿主对象所在区域的深度（形参派生：只能运行期读）
//
// 为什么不是"C 表达式"：limit 的最终形态取决于宿主对象与区域深度运算，只有后端
// 知道（红线 10：表达式的唯一生成处是后端）。此前 air 直接发 `decl(X)` 或裸数字，
// Delta/Dynamic 两个决定性的位在降级时被丢掉 —— 后端无法复原，只能猜，于是守卫
// 要么发错界、要么整条丢掉（实测：`func sink(h Holder, t Buf) { h.b = t }` 会静默悬垂）。
func (l *lowerer) limitOf(g types.GuardSpec, e parse.Expr) string {
	if g.Dynamic {
		return "host"
	}
	switch g.Kind {
	case types.GuardReturn:
		return "fnentry"
	case types.GuardStore, types.GuardComposite, types.GuardDefer:
		if g.Delta > 0 {
			return fmt.Sprintf("depth-%d", g.Delta)
		}
		return "depth"
	}
	return ""
}

// instInstance 是一个待降级的泛型实例。
type instInstance struct {
	Args []types.Type
}

// instancesOf 取（接收者, 名字）对应的全部实例实参表（按实例键排序：确定性）。
func instancesOf(info *types.Info, recv, name string) []instInstance {
	if info == nil {
		return nil
	}
	var out []instInstance
	for _, fi := range info.FuncInsts {
		if fi.Fn == nil || fi.Fn.Recv != recv || fi.Fn.Name != name || len(fi.Args) == 0 {
			continue
		}
		out = append(out, instInstance{Args: fi.Args})
	}
	sortInstances(out)
	return out
}

// lowerInstance 用实例实参降级一份泛型体：符号 = `<base>[args]`，类型先代换再渲染。
func (l *lowerer) lowerInstance(d *parse.FuncDecl, recv string, params []string, inst instInstance) error {
	savedP, savedA, savedSym := l.instParams, l.instArgs, l.symOverride
	l.instParams, l.instArgs = params, inst.Args
	l.symOverride = FuncInstSym(l.in.Pkg, recv, d.Name, inst.Args)
	err := l.funcDecl(d, recv)
	l.instParams, l.instArgs, l.symOverride = savedP, savedA, savedSym
	return err
}

// sortInstances 按实参文本排序（map 序不进 IR，H8/V7.1）。
func sortInstances(in []instInstance) {
	sort.Slice(in, func(i, j int) bool { return instKey(in[i]) < instKey(in[j]) })
}

func instKey(in instInstance) string {
	parts := make([]string, 0, len(in.Args))
	for _, a := range in.Args {
		parts = append(parts, tyText(a))
	}
	return strings.Join(parts, ", ")
}
