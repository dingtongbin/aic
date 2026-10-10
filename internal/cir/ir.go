package cir

import (
	"fmt"
	"sort"
	"strings"

	"aic/internal/air"
)

// ---------------------------------------------------------------------------
// cir = **C 形态 IR**（核心设计 §十：tast → air → mair → eair → cir）。
//
// 与 air 的差别只有一条，但很关键：**控制流是结构化的**（if / for / switch），
// 不是 CFG + goto 汤。C 后端因此可以直接发 `if`/`for`/`switch` —— 既保住可读性，
// 也保住 C 编译器对 for 的识别（向量化的前提）。
//
// 叶子指令**直接复用 air 的指令集**（Let/Store/Call/… 都是 C 形态的线性指令）：
// 再造一套叶子类型就是"同一条规则两处实现"（红线 10），而 air 的叶子在 CIR 层
// 无需任何改写。新增的只有**结构化节点**（If/For/Switch/Region/Return/Break/…）。
//
// 区域与守卫在 CIR 层是**显式语句**：`region { … }` 是 `Region` 语句，
// 存储点守卫是 `Store` 的 `Guard` 字段（守卫是 store 的修饰符，不是表达式树上的
// 标记 —— 这条在 air 里已定案，CIR 只是把它摆到 C 语法的位置上）。
// ---------------------------------------------------------------------------

// Prog 是一个包的 C 形态程序。
type Prog struct {
	Pkg     string
	Imports []string
	Types   []*air.TypeDecl
	Funcs   []*Func
}

// Func 是一个函数（体 = 结构化语句序列）。
type Func struct {
	Sym    string
	Params []air.Param
	Rets   []string
	Flags  []string
	Body   []Stmt
	Loc    air.Loc
	// SrcHash 是**降级前**的 AIR 函数体内容哈希（F1 增量翻译的缓存键原料：
	// CIR 是折叠后的形态，缺了折叠期的注释/边界信息，直接哈希 CIR 会让
	// "同一个源函数的合法再折叠"误失效；用 AIR 原文哈希则稳定）。空 = 未填
	// （老路径/测试），此时调用方自行退化为 CIR 形态哈希。
	SrcHash string
}

// Stmt 是结构化语句。
type Stmt interface{ cirStmt() }

// Leaf 是一条线性指令（叶子上复用 air 的指令集）。
type Leaf struct {
	In  air.Inst
	Loc air.Loc
}

// Guard 是一个存储点守卫（limit = 目标存储的深度表达式）。
type Guard struct {
	Limit string
	What  string
	Loc   air.Loc
}

// If 是条件分支。
type If struct {
	Cond string
	Then []Stmt
	Else []Stmt
	Loc  air.Loc
}

// For 是**唯一**的循环形态（三种写法统一成 C 的 for）。
//
// Head = 循环头（air 的 forhead 块）的指令：**每轮先跑**，算出的值就是 Cond。
// 它绝不能留在循环之前 —— 那样条件只算一次（死循环），而且 Post 里再算一次会
// 在 C 里声明出新的内层同名变量，改不到外层那份（012 语料的实测形态）。
type For struct {
	Head []Stmt
	Cond string
	Post []Stmt
	Body []Stmt
	Loc  air.Loc
}

// Case 是 switch 的一个分支（Label 为空 = default）。
type Case struct {
	Label string
	Body  []Stmt
}

// Switch 是多路分支（match 的变体分派、select 的就绪分派都落在这里）。
type Switch struct {
	Val   string
	Cases []Case
	Loc   air.Loc
}

// Region 是一个 region 块（进入/退出显式成对，V3.1 在 air 层已保证）。
// Scope = `scope { }` 任务域（退出 join spawn 的任务），非 Scope = `region { }` 内存区域。
type Region struct {
	Body  []Stmt
	Scope bool
	Loc   air.Loc
}

// RegionOpen / RegionClose 是**跨块** region 的平铺形态：enter 与 exit 降级后不在
// 同一个 air 块里（典型 = region 块含 for/if，体被折进循环/分支树）。同块内成对的
// 仍折成 Region{Body}；跨块只能平铺——"出口在后面的块"这条边在树形结构里无处安放。
//
// 这是"每请求一区域"的标准写法（B10 实测）：region 包住每轮的容器/循环，退出整块
// 回滚。air 层 V3.1 已保证结构化配平（沿 CFG 边传播深度），折叠器只需按遇到顺序
// 平铺，配对由 fl.regOpen 栈跨块记录。
//
// Scope = `scope { }` 任务域（Name = 该域的 aic_scope 变量名，spawn 站点取用）；
// 非 Scope = `region { }` 内存区域（Name 空）。
type RegionOpen struct {
	Name  string
	Scope bool
	Loc   air.Loc
}

type RegionClose struct {
	Name  string
	Scope bool
	Loc   air.Loc
}

// Scope 是一个**词法块**（源码里的 `{ … }`；air 里是 scopeN → scopeendM 这一对块）。
//
// 为什么必须留着：同名遮蔽（`var x i32 = 1; { var x i32 = 2 }`）靠块边界决定
// "这个名字从哪儿起指谁"。折叠器曾把 scope 块当普通直线延续折平，于是块外那次
// 引用也绑到了内层的 C 名（722 实测：打印 2,2 而不是 2,1）。
type Scope struct {
	Body []Stmt
	Loc  air.Loc
}

// Return 是函数出口（值名表 = 返回槽）。
type Return struct {
	Vals []string
	Loc  air.Loc
}

// Break / Continue 是循环控制（结构化的 break/continue，不是 goto）。
type Break struct{ Loc air.Loc }
type Continue struct{ Loc air.Loc }

// Label / Goto 是**保真**用的跳转形态（P1：IR 先无损装下语义；把 goto 再收成
// if/for 是 P3 的"理想 C"工作，不是 IR 的职责）。
//
// 什么时候需要：一个块有**多个前驱**（汇合点）而它又不能落在"本层结构的下一句"
// 位置时，只能把边物化成 goto —— 否则那条边就没了。丢边的后果不是"C 难看"，
// 而是**静默错译**：791 实测 `load("hash")` 的 `br exit` 被丢，函数缺 return，
// 返回值是栈上垃圾（stdout 里 2.8 MB 空白）。
type Label struct {
	Name string
	Loc  air.Loc
}

// Goto 跳到同名 Label（块标签，C 里发 `goto <name>;`）。
type Goto struct {
	Name string
	Loc  air.Loc
}

// Exit 是唯一收尾块的标记（值已存进返回槽，ret 由 Return 承载）。
type Exit struct{ Loc air.Loc }

func (*Leaf) cirStmt()     {}
func (*Guard) cirStmt()    {}
func (*If) cirStmt()       {}
func (*For) cirStmt()      {}
func (*Switch) cirStmt()   {}
func (*Region) cirStmt()   {}
func (*RegionOpen) cirStmt()  {}
func (*RegionClose) cirStmt() {}
func (*Scope) cirStmt()    {}
func (*Return) cirStmt()   {}
func (*Break) cirStmt()    {}
func (*Continue) cirStmt() {}
func (*Exit) cirStmt()     {}
func (*Label) cirStmt()    {}
func (*Goto) cirStmt()     {}

// FuncBySym 取函数（按符号名）。
func (p *Prog) FuncBySym(sym string) *Func {
	for _, f := range p.Funcs {
		if f.Sym == sym {
			return f
		}
	}
	return nil
}

// --- 打印（与 .air 同一套文法，缩进仅助读） ---------------------------------

// Print 打印一个程序（确定性：类型/函数按符号序）。
func (p *Prog) Print() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cir v1 module %s\n", p.Pkg)
	imps := append([]string{}, p.Imports...)
	sort.Strings(imps)
	for _, im := range imps {
		fmt.Fprintf(&b, "import %s\n", im)
	}
	types := append([]*air.TypeDecl{}, p.Types...)
	sort.Slice(types, func(i, j int) bool { return types[i].Sym < types[j].Sym })
	for _, td := range types {
		printType(&b, td)
	}
	funcs := append([]*Func{}, p.Funcs...)
	sort.Slice(funcs, func(i, j int) bool { return funcs[i].Sym < funcs[j].Sym })
	for _, f := range funcs {
		printFunc(&b, f)
	}
	return b.String()
}

func printType(b *strings.Builder, td *air.TypeDecl) {
	switch td.Kind {
	case "struct":
		kind := "struct"
		if td.Packed {
			kind = "@packed struct"
		}
		fmt.Fprintf(b, "type %s = %s\n", td.Sym, kind)
		for _, f := range td.Fields {
			fmt.Fprintf(b, "    %d:%s:%s\n", f.Idx, f.Name, f.Ty)
		}
	case "enum":
		fmt.Fprintf(b, "type %s = enum\n", td.Sym)
		for _, v := range td.Variants {
			if v.Payload == "" {
				fmt.Fprintf(b, "    %d:%s\n", v.Tag, v.Name)
			} else {
				fmt.Fprintf(b, "    %d:%s(%s)\n", v.Tag, v.Name, v.Payload)
			}
		}
	case "iface":
		fmt.Fprintf(b, "type %s = iface\n", td.Sym)
		for _, s := range td.Slots {
			fmt.Fprintf(b, "    %d:%s %s\n", s.Idx, s.Name, s.Sig)
		}
	}
}

func printFunc(b *strings.Builder, f *Func) {
	flags := ""
	if len(f.Flags) > 0 {
		flags = " [" + strings.Join(f.Flags, ",") + "]"
	}
	params := make([]string, 0, len(f.Params))
	for _, p := range f.Params {
		params = append(params, p.Name+":"+p.Ty)
	}
	rets := ""
	if len(f.Rets) > 0 {
		rets = " -> (" + strings.Join(f.Rets, ", ") + ")"
	}
	fmt.Fprintf(b, "func %s(%s)%s%s {\n", f.Sym, strings.Join(params, ", "), rets, flags)
	printStmts(b, f.Body, "  ")
	fmt.Fprintf(b, "}\n")
}

func printStmts(b *strings.Builder, stmts []Stmt, ind string) {
	for _, s := range stmts {
		printStmt(b, s, ind)
	}
}

func printStmt(b *strings.Builder, s Stmt, ind string) {
	switch v := s.(type) {
	case *Leaf:
		fmt.Fprintf(b, "%s%s\n", ind, air.PrintInst(v.In))
	case *Guard:
		fmt.Fprintf(b, "%sguard %s /* %s */\n", ind, v.Limit, v.What)
	case *If:
		fmt.Fprintf(b, "%sif %s {\n", ind, v.Cond)
		printStmts(b, v.Then, ind+"  ")
		if len(v.Else) > 0 {
			fmt.Fprintf(b, "%s} else {\n", ind)
			printStmts(b, v.Else, ind+"  ")
		}
		fmt.Fprintf(b, "%s}\n", ind)
	case *For:
		fmt.Fprintf(b, "%sfor {\n", ind)
		if len(v.Head) > 0 {
			fmt.Fprintf(b, "%s  head:\n", ind)
			printStmts(b, v.Head, ind+"    ")
		}
		if v.Cond != "" {
			fmt.Fprintf(b, "%s  cond: %s\n", ind, v.Cond)
		}
		printStmts(b, v.Body, ind+"  ")
		if len(v.Post) > 0 {
			fmt.Fprintf(b, "%s  post:\n", ind)
			printStmts(b, v.Post, ind+"    ")
		}
		fmt.Fprintf(b, "%s}\n", ind)
	case *Switch:
		fmt.Fprintf(b, "%sswitch %s {\n", ind, v.Val)
		for _, c := range v.Cases {
			label := c.Label
			if label == "" {
				label = "default"
			}
			fmt.Fprintf(b, "%s  case %s:\n", ind, label)
			printStmts(b, c.Body, ind+"    ")
		}
		fmt.Fprintf(b, "%s}\n", ind)
	case *Region:
		fmt.Fprintf(b, "%sregion {\n", ind)
		printStmts(b, v.Body, ind+"  ")
		fmt.Fprintf(b, "%s}\n", ind)
	case *RegionOpen:
		if v.Scope {
			fmt.Fprintf(b, "%sregion-open scope %s\n", ind, v.Name)
		} else {
			fmt.Fprintf(b, "%sregion-open\n", ind)
		}
	case *RegionClose:
		if v.Scope {
			fmt.Fprintf(b, "%sregion-close scope %s\n", ind, v.Name)
		} else {
			fmt.Fprintf(b, "%sregion-close\n", ind)
		}
	case *Scope:
		fmt.Fprintf(b, "%sscope {\n", ind)
		printStmts(b, v.Body, ind+"  ")
		fmt.Fprintf(b, "%s}\n", ind)
	case *Return:
		fmt.Fprintf(b, "%sret %s\n", ind, strings.Join(v.Vals, ", "))
	case *Break:
		fmt.Fprintf(b, "%sbreak\n", ind)
	case *Continue:
		fmt.Fprintf(b, "%scontinue\n", ind)
	case *Exit:
		fmt.Fprintf(b, "%sexit\n", ind)
	case *Label:
		fmt.Fprintf(b, "%s%s:\n", ind, v.Name)
	case *Goto:
		fmt.Fprintf(b, "%sgoto %s\n", ind, v.Name)
	}
}

// CountStmts 统计语句树的规模（H8 的稳定性报告与"覆盖数上升"判据用）。
func CountStmts(stmts []Stmt) (leaves, nodes int) {
	for _, s := range stmts {
		switch v := s.(type) {
		case *Leaf, *Guard, *Return, *Break, *Continue, *Exit, *Label, *Goto,
			*RegionOpen, *RegionClose:
			leaves++
		case *If:
			nodes++
			l1, n1 := CountStmts(v.Then)
			l2, n2 := CountStmts(v.Else)
			leaves += l1 + l2
			nodes += n1 + n2
		case *For:
			nodes++
			for _, part := range [][]Stmt{v.Head, v.Body, v.Post} {
				l, n := CountStmts(part)
				leaves += l
				nodes += n
			}
		case *Switch:
			nodes++
			for _, c := range v.Cases {
				l, n := CountStmts(c.Body)
				leaves += l
				nodes += n
			}
		case *Region:
			nodes++
			l, n := CountStmts(v.Body)
			leaves += l
			nodes += n
		case *Scope:
			nodes++
			l, n := CountStmts(v.Body)
			leaves += l
			nodes += n
		}
	}
	return leaves, nodes
}
