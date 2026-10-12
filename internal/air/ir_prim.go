// IR-1 原语层：AIC 独有语义的**一等节点**（《AIC 语言规范》§13.1 / §13.2）。
//
// 本文件与 ir.go 的低层节点是**两层**，不是同一层的补充：
//
//	IR-1（本文件）  match / for-in / check / catch / interp / assert / propagate
//	  ↓ 单态化（mono.go，在这一层上做）
//	  ↓ 去糖（desugar.go，只在这一层之后）
//	IR-2（ir.go）   EnumTag / Switch / Cbr / CheckFail / 下标循环 / 字符串拼接
//	  ↓
//	C11
//
// 为什么原语必须在**单态化之前**保留：这些节点携带的类型信息含类型形参
// —— match 臂绑定的 BindTy、check 的多返回类型表 Mt、catch/slot 的类型。
// 一旦在 lowering 时就把它们拆成 EnumTag/Switch/CheckFail，单态化的
// "类型文本替换"就再也不知道该替换哪里，泛型体内的这些形态会**静默丢掉
// 实参化**（§13.2 的硬前置要求，不是风格问题）。
//
// 结构纪律（与 ir.go 的 Block 模型一致）：
//   - 函数体是 flat 的 Blocks 列表；原语节点里只存**块标签**，不嵌 *Block；
//   - 原语节点只带 **After**（lowering 续写块的标签），不带 OnFail ——
//     失败路径的 C 形态（CheckFail 块）是 desugar 的产物，IR-1 不含 IR-2 内容；
//   - 原语节点自己带 Loc（诊断回指源码位置）。
//
// 已声明但 lowering 尚未发射的原语（声明 ≠ 落地，落地见 AIC_重构TODO.md）：
//   - ForInInst：下标循环的 head/post/done 骨架与 break/continue 目标在
//     lowering 侧已具名，改造收益低于 match/check 一族，排在后面；
//   - SelectInst / ScopeInst：同理，等前三族稳定后迁移。
// 已定义的原语若 lowering 不发射，desugar 就不会遇到它们（零风险）。

package air

// ---------------------------------------------------------------------------
// 匹配：match（§13.3：tag 取值 + 分支 + 每臂 payload 绑定）
// ---------------------------------------------------------------------------

// MatchTerm 是语句位 `match <subject> { arms }`。
// 穷尽性已由检查器验过（E-5）；IR-1 不重复校验，只承载语义。
//
// After = 汇合块标签（lowering 建好，所有臂体都 Term→After）。
type MatchTerm struct {
	Subject string
	SubjTy  string // 主体类型文本（enum 或 Base[a, b] 实例）
	Arms    []MatchArm
	After   string
	Loc     Loc
}

// MatchExprRHS 是表达式位 match：值是各臂的公共类型（检查器已验一致）。
// Slot/SlotTy = 结果槽（lowering 声明 `var <Slot> : <SlotTy>`，各臂体往里
// store；无 phi 也满足 —— 槽只定义一次，写入多次）。
type MatchExprRHS struct {
	Subject string
	SubjTy  string
	Arms    []MatchArm
	Slot    string
	SlotTy  string
	After   string
	Loc     Loc
}

// MatchArm 是一个臂：多模式（`A | B =>`）+ 一个体块。
//
// Body 是臂体块标签。**表达式形 match 也用块**：lowering 把 `=> expr` 折成
// "求值 + 存槽 + Br After"的隐式块，两种分支形态在 IR-1 里同形。
type MatchArm struct {
	Patterns []MatchPattern
	Body     string
	Loc      Loc
}

// MatchPattern 是一个模式。
//
// BindTy 是**已实参化**的绑定类型（`Option[i32]` 的 Some 绑定 = i32）。
// 它是 IR-1 必须存在的理由：泛型模板体里这个字段含类型形参，单态化据此
// 替换；提前去糖成 EnumPayload 时这个信息只存在于 IR-2 的 Var.Ty，而那时
// 替换窗口已过。
type MatchPattern struct {
	Variant string // 变体名（已去掉 `Enum.` 限定前缀）
	Tag     int    // 变体序号（声明序下标）
	Binding string // 绑定名；"" 或 "_" = 无绑定
	BindTy  string // 绑定类型文本（HasPay = false 时为空串）
	HasPay  bool
	Loc     Loc
}

// ---------------------------------------------------------------------------
// 迭代：for-in（§13.3：索引循环 + len 缓存 / recv 循环 / next() 调用循环）
// ---------------------------------------------------------------------------

// ForInInst 是 `for <names> in <subject>`。
//
// 覆盖全部四类主体：切片、map、set、chan[T]，以及实现 Iterator[T] 者。
// str **不在**协议内（字节 ≠ 字符）。
//
// Names/NameTys 逐位对齐；语义按 SubjTy 而定（权威推导在 types 的
// RangeBindTypes，IR-1 不重复推导）：
//
//	切片/数组/set/chan：单名 = 元素；两名 = (下标, 元素)
//	map：               单名 = 键；两名 = (键, 值)
type ForInInst struct {
	Names   []string
	NameTys []string
	SubjTy  string
	Subject string
	Body    string
	After   string
	Loc     Loc
}

// ---------------------------------------------------------------------------
// 错误处理：check / catch / assert / propagate（§5.6 / §13.3）
// ---------------------------------------------------------------------------

// CheckRHS 是 `check <call>`（E-2/E-3）。
//
// Mt 是调用结果的多返回类型表（**末位是 Err**），长度 ≥ 2。
// Payload 是前置返回位的值名；空串 = 该调用只返回 Err（仅传播，无值）。
//
// 语义：末位 Err 的 code ≠ 0 ⇒ 失败路径传播给调用者；否则继续，值 = Payload。
type CheckRHS struct {
	Operand string
	Mt      []string
	Payload string
	After   string
	Loc     Loc
}

// CatchRHS 是 `<expr> catch [e] { <body> }`（E-3 就地处理）。
//
// 与 CheckRHS 的分工：check 向**上抛**，catch **就地**处理。Bind 非空时
// 处理块内可引用该 Err 值（E-7 允许不绑定）。
//
// Mt = 运算对象的多返回类型表（末位 Err）；结果类型 = 去掉末位的前置位。
// Slot = 结果槽名（lowering 声明的 `var <Slot> : <Mt[0]>`）：ok 路径写负载，
// Body 处理块写处理结果，两路在 After 合流。
type CatchRHS struct {
	Operand string
	Mt      []string
	Bind    string
	Body    string
	Slot    string
	After   string
	Loc     Loc
}

// AssertRHS 是 `expr!`（E-4 断言：出错即 trap）。
//
// 必须是一等原语而不是直接发 TrapIfErr：TrapIfErr 已在 IR-2。二者的差别在
// R-Move —— 断言点决定"负载存活、Err 消亡"，而这个决策只有知道这是断言时
// 才能做（§13.2：语义决策集中在去糖之前）。
type AssertRHS struct {
	Operand string
	Mt      []string
	Payload string
	Loc     Loc
}

// PropagateInst 是**自动传播**（§5.6）：fallible 调用在语句位（`f()` 独成
// 一句）与 return 位（`return f()` 的实参是 fallible 调用）不需写 check，
// 语义即"失败即上抛"。
//
// 是一等原语而不是 lowering 就地拆开：它与 check 的失败路径语义相同，但
// **触发条件由调用点语境决定**。把语境判断放进去糖期，§5.6 的"哪些位置自动
// 传播"才只有一处实现（§13.2 的单一实现位置判据）。
//
// Mode："stmt" 语句位（无值）；"arg" return 位实参（Err 上抛，不产值）。
type PropagateInst struct {
	Operand string
	Mt      []string
	Mode    string
	After   string
	Loc     Loc
}

// ---------------------------------------------------------------------------
// 格式化：插值（§13.3：拼接 + 逐分量格式化）
// ---------------------------------------------------------------------------

// InterpRHS 是字符串插值 `"…{expr}…"`（§1.4：格式化唯一入口）。
//
// Parts 与 Values 交替：Parts 比 Values 多一个（首尾与每槽之间都有字面片段，
// 空片段保留以固定下标对应）。ValueTys 逐位给出占位符值的**类型文本** ——
// 去糖期按它机械选格式化器（str 直通，其余走 str_fromI64 / str_fromU64 /
// str_fromF64 / str_fromBool），去糖不做语义判断（§13.5）。
type InterpRHS struct {
	Parts    []string
	Values   []string
	ValueTys []string
	Loc      Loc
}

// ---------------------------------------------------------------------------
// 任务域 / 多路等待：scope / select
// ---------------------------------------------------------------------------

// ScopeInst 是 `scope { }` / `scope.timeout(d) { }`：**任务域**（不是内存
// region）——退出时 join 域内全部 spawn 的任务。
//
// 与 IR-2 的 RegionEnter/Exit 的边界：后者是"区域入栈/出栈"的 C 指令序列，
// 本节点是"任务域 + join 纪律"的语言语义（§13.3：区域入栈/出栈 + scope join）。
type ScopeInst struct {
	Body    string
	Timeout string // 截止点名（单调毫秒）；"" = 无超时
	IsTimeo bool
	After   string
	Loc     Loc
}

// SelectInst 是 `select { arms }`。
//
// 与 ir.go 的 SelWait 的区别：SelWait 是 **C 运行时 ABI 描述符**（描述怎么
// 调 aic_select_wait），属于 IR-2；本节点是**语言语义**（有哪些臂、谁 send
// 谁 recv、绑定什么），属于 IR-1。
type SelectInst struct {
	Arms  []SelectArm
	After string
	Loc   Loc
}

// SelectArm 是一个 select 臂。Kind ∈ {"send","recv",""}（空 = 时间臂）。
type SelectArm struct {
	Kind     string
	Chan     string
	Val      string
	Bind     string
	BindTy   string
	Body     string
	Deadline string
	Loc      Loc
}

// ---------------------------------------------------------------------------
// Inst / RHS / Term 归属标记
// ---------------------------------------------------------------------------

func (*MatchTerm) airTerm()     {}
func (*ForInInst) airInst()     {}
func (*SelectInst) airInst()    {}
func (*PropagateInst) airInst() {}
func (*ScopeInst) airInst()     {}

func (*MatchExprRHS) airRHS() {}
func (*CheckRHS) airRHS()     {}
func (*CatchRHS) airRHS()     {}
func (*InterpRHS) airRHS()    {}
func (*AssertRHS) airRHS()    {}

// ---------------------------------------------------------------------------
// 判定助手（mono / desugar / verifier 共用）
// ---------------------------------------------------------------------------

// IsPrimInst / IsPrimTerm / IsPrimRHS 报告节点是否属于 IR-1 原语层。
func IsPrimInst(in Inst) bool {
	switch in.(type) {
	case *ForInInst, *SelectInst, *PropagateInst, *ScopeInst:
		return true
	}
	return false
}

func IsPrimTerm(t Term) bool {
	_, ok := t.(*MatchTerm)
	return ok
}

func IsPrimRHS(r RHS) bool {
	switch r.(type) {
	case *MatchExprRHS, *CheckRHS, *CatchRHS, *InterpRHS, *AssertRHS:
		return true
	}
	return false
}

// HasPrim 报告模块里是否还有 IR-1 原语剩余。单态化之后仍在是正常的，但
// **发射之前**（过完 desugar）必须为 false（§13.1 的管线断言）。
func HasPrim(m *Module) bool {
	for _, f := range m.Funcs {
		for _, b := range f.Blocks {
			for _, in := range b.Insts {
				if IsPrimInst(in) {
					return true
				}
				if l, ok := in.(*Let); ok && IsPrimRHS(l.Rhs) {
					return true
				}
			}
			if IsPrimTerm(b.Term) {
				return true
			}
		}
	}
	return false
}

// PrimKinds 报告 IR-1 原语名表（verifier 覆盖面自检用）。
func PrimKinds() []string {
	return []string{"match", "forin", "check", "catch", "interp", "assert", "propagate", "scope", "select"}
}
