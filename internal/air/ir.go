// Package air 是 AIC 的类型化核心 IR（核心设计 §十 10.1/10.1.1）。
//
// 五个检查点共用**一份数据结构**：tast（检查后）→ air（去糖后）→ mair（单态化后）
// → eair（分析决策后）→ cir（C 形态）。文本（`.air`）只是投影：编译器不解析它，
// 只有 verifier 的反例语料以手写 `.air` 为输入。
//
// 结构性封堵（§10.1）：守卫是 `store` 的**修饰符**，不是独立指令、也不是表达式树
// 上的标记 —— 「守卫丢了但存储还在」在本结构里无法表达；`alloc` 是唯一分配来源。
package air

import "aic/internal/parse"

// Loc 是一条指令的源位置（红线 9 的 IR 载体：`loc <file>:<line>:<col>`）。
type Loc struct {
	File string
	Line int
	Col  int
}

// LocOf 从 parse 位置取 Loc。
func LocOf(p parse.Pos) Loc { return Loc{File: p.File, Line: p.Line, Col: p.Col} }

// Pos 把 Loc 还原成 parse 位置（后端发 #line 回指用；defFile = 缺文件时的兜底）。
func (l Loc) Pos(defFile string) parse.Pos {
	f := l.File
	if f == "" {
		f = defFile
	}
	return parse.Pos{File: f, Line: l.Line, Col: l.Col}
}

// Module 是一个包（整程序 = 多个 Module，按拓扑序）。
type Module struct {
	Pkg     string
	Imports []string
	Types   []*TypeDecl
	Funcs   []*Func
}

// TypeDecl 是具名类型声明（struct / enum / iface）。
type TypeDecl struct {
	Sym      string // aic_<pkg>_<Name>（§八 mangling 单表）
	Kind     string // "struct" | "enum" | "iface"
	Packed   bool
	Fields   []Field   // struct：字段按声明序（ABI 冻结）
	Variants []Variant // enum：tag = 声明序
	Slots    []Slot    // iface：槽位号 = 方法声明序
	Loc      Loc
}

// Field 是 struct 字段（Idx = 声明序下标）。
type Field struct {
	Idx  int
	Name string
	Ty   string
}

// Variant 是 enum 变体（Tag = 声明序下标；Payload 空 = 无数据）。
type Variant struct {
	Tag     int
	Name    string
	Payload string
}

// Slot 是接口方法槽位（Idx = 声明序下标）。
type Slot struct {
	Idx  int
	Name string
	Sig  string // `(T1, T2) -> (R1, R2)` 形态
}

// Param 是函数形参。
// Ref = 按引用传参（defer 块形提升体的捕获槽：C 侧是 `T *name__p`，体内对 name
// 的读写都是 `(*name__p)` —— 出口读到的必须是最终值，035/713 锚点）。
type Param struct {
	Name string
	Ty   string
	Ref  bool
}

// Func 是一个函数/方法体。
type Func struct {
	Sym    string // 具体符号名（实例已显式化，§10.2）
	Params []Param
	Rets   []string
	Flags  []string // export / extern / static / init / derived
	Blocks []*Block
	Loc    Loc
}

// Block 是一个基本块：恰一个终结符在块尾（V2.1）。
type Block struct {
	Label string
	Insts []Inst
	Term  Term
	Loc   Loc
}

// Inst 是块内指令（不含终结符）。
type Inst interface{ airInst() }

// Let 是 `let <tmp> : <ty> = <rhs>`。
type Let struct {
	Tmp string
	Ty  string
	Rhs RHS
	Loc Loc
}

// Var 是 `var <name> : <ty> [= <val>]`（可带 @live 标记）。
type Var struct {
	Name string
	Ty   string
	Init string // 空 = 未初始化
	Live bool
	Loc  Loc
}

// Store 是 `store [.guard(<limit>)] <place>, <val>` —— 唯一带守卫的指令形态。
// Limit 非空 = 该存储点需要运行时守卫（§五 R3 的七个规范位置）。
//
// Limit 是**受限文法**（不是 C 表达式：表达式的唯一生成处是后端，红线 10）：
//
//	fnentry   返回点守卫（界 ≤ 函数入口，运行时比 0u）
//	depth-N   界 = 当前区域深度 − N（N = 检查器算出的声明偏移）
//	depth     界 = 当前区域深度
//	host      界 = 宿主对象所在区域的深度（宿主 = 本 place 的基座；无基座 = 当前深度）
type Store struct {
	Place Place
	Val   string
	Limit string
	Loc   Loc
}

// RegionEnter / RegionExit 与作用域严格配对（V3.1）。
// Scope = 这是 `scope { }`（任务域：退出 join 全部 spawn 的任务），不是
// `region { }`（内存区域）—— 两者在 C 侧的进出序列不同（scope 还要
// aic_scope_enter/exit），降级期就必须分开（773/742/772 锚点）。
type RegionEnter struct {
	TypeID string
	Scope  bool
	Loc    Loc
}
type RegionExit struct {
	Scope bool
	Loc   Loc
}

// Spawn 是 `spawn <callee>(<vals…>)`：在所属 scope 里起一个任务。
// 实参在 spawn 点求值（与 defer 同纪律）；被调符号与实参表都在这里定下来，
// 后端据此发上下文块 + 薄 thunk + aic_scope_spawn。
type Spawn struct {
	Callee   string
	TypeArgs []string
	Vals     []string
	Loc      Loc
}

// DeferReg / DeferInit / DeferRun 是 defer 三件套（§10.1：注册点求值 + 逆序执行）。
// OnErr = 仅因 Err 返回时执行（N8 `errdefer`）。
type DeferReg struct {
	OnErr bool
	Loc   Loc
}

// DeferInit 是 `defer.init <id>, <callee>[<ty>,…](<vals…>)`：注册点的**求值**。
// 被调符号与实参都在注册点定下来（之后改原变量不影响已注册的实参 —— 核心设计
// §三"N8 实参注册点求值"，锚点 ok/757）。方法形态 = 普通函数 + 接收者当首实参
// （与直调同一条 mangling，后端不必再认识"方法"）。
// Limits 与 Vals 逐位对齐（空 = 该实参无守卫）：GuardDefer 的注册点界检查
// （§五 R5：defer 实参的界 ≤ 函数入口；781 锚点 —— 此前检查器标了守卫但降级侧
// 没有载体，守卫从未发出，defer 在退出时读已弹出的区域）。
type DeferInit struct {
	ID       int
	Callee   string
	TypeArgs []string
	Vals     []string
	Limits   []string
	OnErr    bool
	Loc      Loc
}

// DeferRun 是 `defer.run [<err>]`：函数的**唯一收尾块**里跑 defer 栈（LIFO）。
// Err 非空 = 该 Err 的 code ≠ 0 时才跑 onerr 项（N8 的"仅错误路径"）；空 = 全部无条件跑。
type DeferRun struct {
	Err string
	Loc Loc
}

// Trap 是**无条件** trap 的块内指令（N6：select 全部不就绪 = 死锁）。
type Trap struct {
	Code string
	Line int
	Loc  Loc
}

// TrapIfErr 是 `expr!`（E4 断言，核心设计 §15.7）：末位 Err 的 code ≠ 0 → trap
// （消息含 code/msg/位置），否则继续。它是**块内指令**而非终结符 —— 断言之后
// 控制流继续（值 = 去掉末位 Err 的负载）。
type TrapIfErr struct {
	Err  string // Err 值名（{code, msg} 结构）
	Line int
	Loc  Loc
}

// SelWait 是 N6 `select` 的**真等待**（L3 的运行时 ABI，核心设计 §16 N6 / §7.1）：
// 把各分支描述符交给运行期 `aic_select_wait`，结果是就绪下标：
//
//	≥0 = 该臂就绪（随即做一次不会阻塞的收发）
//	-1 = 时间分支到点
//	-2 = 被取消（scope.timeout / Ctx）⇒ 不执行任何分支，直接落到 select 之后
//
// 无就绪、无截止点、无取消且无人能推进 ⇒ 运行期 trap 死锁（位置 = select 点）。
// 存在 recv 臂且**已无别的存活任务**时按【O2】返回该 recv 臂（其 recv 得 (零值,false)）。
type SelWait struct {
	Tmp      string   // 结果临时量（i32 下标）
	Chans    []string // 与 Sends 等长：各臂的通道值名（源码序）
	Sends    []bool   // true = send 臂，false = recv 臂
	Deadline string   // 时间臂的截止值名（"" = 无时间臂）
	Line     int
	Loc      Loc
}

func (*Let) airInst()         {}
func (*Var) airInst()         {}
func (*Store) airInst()       {}
func (*RegionEnter) airInst()      {}
func (*RegionExit) airInst()       {}
func (*Spawn) airInst()            {}
func (*DeferReg) airInst()         {}
func (*DeferInit) airInst()   {}
func (*DeferRun) airInst()    {}
func (*TrapIfErr) airInst()   {}
func (*Trap) airInst()        {}
func (*SelWait) airInst()     {}

// RHS 是 let 的右值形态。
type RHS interface{ airRHS() }

// Call 是 `call <sym> [<ty>,…](<val>,…)`（泛型实参已显式化）。
type Call struct {
	Sym      string
	TypeArgs []string
	Args     []string
}

// CallInd 是 `call.ind <val>.<slot>(<val>,…)`：接口方法按 vt 槽位分发。
// Slot 是接口声明序下标（后端据此发 `vt[Slot]`；只留接收者与实参的话，
// 后端无法复原要调哪个槽 —— 实测的 IR 缺口）。
type CallInd struct {
	Recv string
	Slot int
	Args []string
}

// Closure 是 N1 的闭包值：静态函数符号 + 按值捕获的环境（Env = 创建点的值名表，
// 顺序 = checker 记下的捕获序）。无捕获时 Env 为空 = 纯函数指针。
type Closure struct {
	Sym string
	Env []string
}

// CallClosure 是闭包值调用 `call.closure <val>(<val>,…)`：被调值自身携带环境，
// 故实参表里不出现 env（C 后端按 {fn, env} 两字形态发射）。
type CallClosure struct {
	Val  string
	Args []string
}

// Alloc 是 `alloc (region|task|live)(<ty>)` —— 唯一分配点（V7.2）。
type Alloc struct {
	Kind string // region | task | live
	Ty   string
}

// Binop / Cmp / Unop 是算术与比较（操作数同类型，V1.7）。
type Binop struct{ Op, A, B string }
type Cmp struct{ Op, A, B string }
type Unop struct{ Op, A string }

// Conv 是显式转换（Kind ∈ trunc/sext/zext/fptosi/sitofp/fpext/fptrunc）。
type Conv struct{ Kind, A string }

// SizeOf 是编译期内省 `sizeOf(T)` 的 AIR 形态：真尺寸只由 C 的 sizeof 给出
// （@packed / [T;N] 的 padding 绝不猜）。标量与句柄在 checker 已折成常量。
type SizeOf struct{ Ty string }

// Field / Elem / Len / StrView 是聚合与视图访问。
type FieldRHS struct {
	Place Place
	Idx   int
}
type ElemRHS struct {
	Place Place
	Idx   string // 下标是值（临时量或常量）
}
type LenRHS struct{ Place Place }

// StrViewRHS 是 str 视图读 `s[lo..hi]`：**零拷贝**（字节恒在任务区域），
// Base 是基础 str 的值名，Lo/Hi 是边界值名（§二.5：lo <= hi <= len）。
// `for ch in s` 的单字节视图也走这条（Hi = 下标 + 1）—— 元素类型是 str 不是 u8，
// 只有 `for i, b in s` 两形式才出 u8 字节。
type StrViewRHS struct{ Base, Lo, Hi string }

// MultiExtract / EnumTag / EnumPayload 是解构。
type MultiExtract struct {
	Val string
	Idx int
}
type EnumTag struct{ Val string }
type EnumPayload struct {
	Val string
	Idx int
}

// Box / Witness 是接口装箱与见证表引用（§10.2）。
type Box struct {
	Val   string
	Iface string
}
type Witness struct {
	Ty    string
	Iface string
}

// Const / Tmp / VarRef / Nil 是值形态。
type Const struct{ Lit string }
type TmpRef struct{ Name string }
type VarRef struct{ Name string }
type Nil struct{ Ty string }

// AddrRHS 是**取地址**（defer 块形按引用捕获的注册点：`&x`）。
// 为什么必须有这个形态：defer 块在函数出口跑，块内读外层局部量必须读到**最终值**
// （035/713 的 `failed = true` 之后才生效）⇒ 注册点传的是变量槽的地址，不是值的
// 拷贝。C 有取地址，AIR 没有这条指令就表达不了这个语义（R14：IR 先无损装下）。
type AddrRHS struct{ Val string }

func (*Call) airRHS()         {}
func (*CallInd) airRHS()      {}
func (*Closure) airRHS()      {}
func (*CallClosure) airRHS()  {}
func (*Alloc) airRHS()        {}
func (*Binop) airRHS()        {}
func (*Cmp) airRHS()          {}
func (*Unop) airRHS()         {}
func (*Conv) airRHS()         {}
func (*SizeOf) airRHS()       {}
func (*FieldRHS) airRHS()     {}
func (*ElemRHS) airRHS()      {}
func (*LenRHS) airRHS()       {}
func (*StrViewRHS) airRHS()   {}
func (*AddrRHS) airRHS()      {}
func (*MultiExtract) airRHS() {}
func (*EnumTag) airRHS()      {}
func (*EnumPayload) airRHS()  {}
func (*Box) airRHS()          {}
func (*Witness) airRHS()      {}
func (*Const) airRHS()        {}
func (*TmpRef) airRHS()       {}
func (*VarRef) airRHS()       {}
func (*Nil) airRHS()          {}

// Term 是块终结符（恰一个在块尾）。
type Term interface{ airTerm() }

// Br 是无条件跳转；Cbr 是二路分支；Switch 是多路（enum tag / 值分发）。
type Br struct {
	Label string
	Loc   Loc
}
type Cbr struct {
	Cond       string
	Then, Else string
	Loc        Loc
}
type Switch struct {
	Val    string
	Labels []string
	Loc    Loc
}

// CheckFail 是 check 的传播：判 err → checkfail → 唯一收尾块（§六）。
type CheckFail struct {
	Err string
	Tmp string
	Loc Loc
}

// Ret 是返回（全部 ret 汇入唯一收尾块，V4.4）。
type Ret struct {
	Vals []string
	Loc  Loc
}

func (*Br) airTerm()        {}
func (*Cbr) airTerm()       {}
func (*Switch) airTerm()    {}
func (*CheckFail) airTerm() {}
func (*Ret) airTerm()       {}

// Place 是 lvalue（可嵌套；§10.1：lvalue 必须是显式 place 节点）。
type Place interface{ airPlace() }

type VarPlace struct{ Name string }
type FieldPlace struct {
	Base Place
	Idx  int
}
type ElemPlace struct {
	Base Place
	Idx  string
}
// StrViewPlace 是 str 视图的 place 形态（Lo/Hi 与 StrViewRHS 同语义；
// str 不可变，视图只能当值用，place 形态是为 IR 完整性保留）。
type StrViewPlace struct {
	Base   Place
	Lo, Hi string
}

func (*VarPlace) airPlace()     {}
func (*FieldPlace) airPlace()   {}
func (*ElemPlace) airPlace()    {}
func (*StrViewPlace) airPlace() {}
