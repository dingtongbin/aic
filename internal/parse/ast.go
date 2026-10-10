package parse

// ---------------------------------------------------------------------------
// AST for the v2 grammar (核心设计 §〇–§九: 23 关键字 + 5 注解).
// Parsing accepts the whole surface from day one; type checking opens at R2
// (黄金执行规划 §七).
//
// v2 换基：v1 的 WhileStmt/HandleStmt/AwaitStmt/DelStmt/AssertStmt/LiveStmt/
// TupleLit/MapLit 退役；新增 RegionStmt（区域块）、CompositeLit（C{f: v}）、
// LambdaExpr（纯 lambda）、MatchExpr（match 表达式形）、Index 的 i..j 视图形。
// ---------------------------------------------------------------------------

// Ann carries the resolved semantic type for a node. Later stages write it and
// read it back; the field is `any` so parse does not depend on them, keeping
// the layering acyclic.
type Ann struct {
	Resolved any
}

// SetResolved records a node's semantic type.
func (a *Ann) SetResolved(v any) { a.Resolved = v }

// GetResolved reads back the recorded type.
func (a *Ann) GetResolved() any { return a.Resolved }

type Node interface{}

// --- expressions -------------------------------------------------------------

type Expr interface {
	Node
	exprNode()
}

type (
	// Ident is a bare name: variable, function, enum value, etc.
	Ident struct {
		Ann
		Name string
		Pos  Pos
	}
	// IntLit / FloatLit / StrLit keep the raw source text.
	IntLit struct {
		Ann
		Text string
		Pos  Pos
	}
	FloatLit struct {
		Ann
		Text string
		Pos  Pos
	}
	StrLit struct {
		Ann
		Text string // raw, including quotes
		Pos  Pos
	}
	// InterpLit is the interpolated string `"id={id}"` (N2, 核心设计 §16).
	// 占位符只接受**名字或字段访问**（无表达式、无格式说明符）；`{{` / `}}`
	// 是字面 `{` / `}` 的转义。Texts 恒比 Values 多一项（首尾可为空串）。
	InterpLit struct {
		Ann
		Texts  []string
		Values []Expr
		Pos
	}
	BoolLit struct {
		Ann
		Value bool
		Pos   Pos
	}
	// NilLit is the reserved word: the zero value of every type (核心设计 §二.3).
	NilLit struct {
		Ann
		Pos Pos
	}
	// ThisExpr is `this`.
	ThisExpr struct {
		Ann
		Pos Pos
	}
	// Unary covers -x, !x, ~x (无一元 `+`).
	Unary struct {
		Ann
		Op string
		X  Expr
		Pos
	}
	// Binary covers arithmetic, comparison and logic.
	Binary struct {
		Ann
		Op          string
		Left, Right Expr
		Pos
	}
	// Assign covers `=` and the numeric compound forms. 赋值是语句不是表达式.
	Assign struct {
		Ann
		Op    string
		LHS   Expr
		Right Expr
		Pos
	}
	// MultiAssign is `a, err = f()` — every target must already exist, `_`
	// excepted (核心设计 §六 绑定规则).
	MultiAssign struct {
		Ann
		Op    string
		LHS   []Expr
		Right Expr
		Pos
	}
	// Call is f(args) — also explicit conversions i32(x) and constructor C(args).
	Call struct {
		Ann
		Fn   Expr
		Args []Expr
		Pos
	}
	// Index is a[i], or the str view a[i..j] when End != nil (核心设计 §二.3).
	Index struct {
		Ann
		X     Expr
		Index Expr
		End   Expr
		// TypeArgs 是 `Name[T, U]` 里逗号后的其余类型实参（表达式位的泛型实例化）。
		// 单个实参时为空（Index 即那一个）；下标读写恒为空（checker 按头部名字区分）。
		TypeArgs []Expr
		Pos
	}
	// Field is a.b — member access, this.x, or package-qualified names.
	Field struct {
		Ann
		X    Expr
		Name string
		Pos
	}
	// CheckExpr is `check f()`, or `f()?` when Postfix is set (两者等价,
	// 核心设计 §15.7 E1). The operand must be a call whose last result is
	// Err (checker-enforced); err != nil propagates the enclosing function.
	// `check` 只允许出现在 var 初始化/独立语句的**最外层**（解析层规则）；
	// `?` 是后缀运算符、位置无关（推荐写法）。
	CheckExpr struct {
		Ann
		X       Expr
		Postfix bool
		Pos
	}
	// OrExpr is `expr or <默认值>` (E2 降级, 核心设计 §15.7): 末位 Err 非 nil
	// 时取默认值并继续, 不中断控制流。
	OrExpr struct {
		Ann
		X       Expr
		Default Expr
		Pos
	}
	// CatchExpr is `expr catch e { … }` (E3 就地处理): 末位 Err 非 nil 时把该
	// Err 绑定到 e 并执行块。块要么**总是退出**（return 等），要么以一条表达式
	// 语句结尾 —— 那条表达式的值就是整个 catch 表达式的值（负载类型）。
	CatchExpr struct {
		Ann
		X     Expr
		Name  string
		Block *Block
		Pos
	}
	// AssertExpr is `expr!` (E4 断言, 核心设计 §15.7): 末位 Err 非 nil = trap
	// （含 code/msg/位置）；值 = 去掉末位 Err 之后的负载。
	AssertExpr struct {
		Ann
		X Expr
		Pos
	}
	// ArrayLit is [] or [a, b, c] — the list literal. 元素类型必须写在声明处：
	// `var xs = []` is a compile error (核心设计 §二.2).
	ArrayLit struct {
		Ann
		Elems []Expr
		Pos
	}
	// CompositeLit is C{f: v} — class construction when the class has no init
	// (核心设计 §四). Unlisted fields take the zero value.
	CompositeLit struct {
		Ann
		TypeName Expr // Ident or package-qualified Field
		Fields   []CompositeField
		Pos
	}
	CompositeField struct {
		Name  string
		Value Expr
		Pos
	}
	// LambdaExpr is the lambda: x => e / (a, b) => e / (a) => { … } (纯 lambda,
	// 参数不带类型), or the N1 closure literal `func(x i32) -> i32 { … }`
	// (参数带类型 + 可选返回位, 可**按值捕获**外层局部变量).
	// Capturing is allowed only for the `func` form; the arrow form stays pure.
	LambdaExpr struct {
		Ann
		// Func 标记这是 `func(...) -> R { … }` 字面量（自带签名、可捕获），
		// 而不是 `x => e` 箭头形（参数不带类型、不可捕获）。
		Func   bool
		Params []string
		// Ptypes 是 `func` 字面量的形参类型（与 Params 等长；箭头形为空）。
		Ptypes []TypeExpr
		// Results 是 `func` 字面量的返回位（箭头形为空）。
		Results []TypeExpr
		Block   *Block // block-body form; nil for the expression form
		Body    Expr   // expression form; nil for the block form
		Pos
	}
	// MatchExpr is the expression-position match: every arm is an expression
	// and 穷尽性 guarantees a value (核心设计 §三).
	MatchExpr struct {
		Ann
		Subject Expr
		Arms    []MatchArm
		Pos
	}
	// MatchPattern is one level deep (核心设计 §三): an enum variant name, or
	// that variant with a single binding `Name(binding)`. No wildcard.
	MatchPattern struct {
		Kind    string // "value" | "payload"
		Name    string
		Binding string // payload binding; "_" = 弃位
		Pos
	}
)

func (*Ident) exprNode()        {}
func (*IntLit) exprNode()       {}
func (*FloatLit) exprNode()     {}
func (*StrLit) exprNode()       {}
func (*InterpLit) exprNode()    {}
func (*BoolLit) exprNode()      {}
func (*NilLit) exprNode()       {}
func (*ThisExpr) exprNode()     {}
func (*Unary) exprNode()        {}
func (*Binary) exprNode()       {}
func (*Assign) exprNode()       {}
func (*MultiAssign) exprNode()  {}
func (*Call) exprNode()         {}
func (*Index) exprNode()        {}
func (*Field) exprNode()        {}
func (*CheckExpr) exprNode()    {}
func (*OrExpr) exprNode()       {}
func (*CatchExpr) exprNode()    {}
func (*AssertExpr) exprNode()   {}
func (*ArrayLit) exprNode()     {}
func (*CompositeLit) exprNode() {}
func (*LambdaExpr) exprNode()   {}
func (*MatchExpr) exprNode()    {}
func (*MatchPattern) exprNode() {}

// --- statements ---------------------------------------------------------------

type Stmt interface {
	Node
	stmtNode()
}

type (
	// Block is `{ stmts }`; scope/region bodies reuse it.
	Block struct {
		Stmts []Stmt
		Pos
	}
	// VarTarget is one name in a declaration. Live marks `var live x` (外一层
	// 区域创建，核心设计 §五 R4; R20 起是关键字不是注解); Blank marks the `_` 弃位.
	VarTarget struct {
		Name  string
		Live  bool
		Blank bool
		Pos
	}
	// VarDecl covers class fields and local declarations. 顶层 var = 编译错
	// (红线 21)，由 parseDecl 拒绝。
	VarDecl struct {
		Targets []VarTarget
		Type    TypeExpr // nil when inferred
		Init    Expr     // nil for a zero-value declaration
		Pos
	}
	// ExprStmt is a bare expression (call, assignment, match-statement arm…).
	ExprStmt struct {
		X Expr
		Pos
	}
	// IfStmt: 条件必须 bool（checker）。else 配对最近未配对的 if；`else if`
	// 表示为 else 块内的单个 IfStmt。
	IfStmt struct {
		Cond Expr
		Then *Block
		Else *Block // nil when absent
		Pos
	}
	// ForStmt covers all three forms (核心设计 §三):
	//	for cond { }        Init=Cond=Post=nil after Cond
	//	for init; c; post   C-style
	//	for i, x in xs { }  IsRange; Names = bindings (1–2, `_` allowed);
	//	for i in a..b { }   IsRange 且 RangeEnd != nil（半开 [a, b)）
	ForStmt struct {
		Init Stmt
		Cond Expr
		Post Stmt
		Body *Block
		// range form
		IsRange  bool
		Names    []VarTarget
		RangeX   Expr
		RangeEnd Expr
		Pos
	}
	// ReturnStmt carries zero or more results (multi-return is call-position
	// sugar; 元组不可存储，核心设计 §二.2).
	ReturnStmt struct {
		Results []Expr
		Pos
	}
	// BranchStmt is break/continue. 无循环标签.
	BranchStmt struct {
		Keyword string
		Pos
	}
	// MatchArm is shared by the statement and expression forms. 语句形分支为
	// `{ … }` 块（Value == nil）；表达式形分支为表达式（Block == nil）。
	MatchArm struct {
		Patterns []MatchPattern
		Block    *Block
		Value    Expr
		Pos
	}
	// MatchStmt is the statement-position match; arms may be blocks.
	MatchStmt struct {
		Subject Expr
		Arms    []MatchArm
		Pos
	}
	// DeferStmt is `defer f()` or `defer { … }` — function-level LIFO
	// (核心设计 §三). region 块退出不触发 defer。
	DeferStmt struct {
		Call  Expr
		Block *Block
		Pos
	}
	// ErrDeferStmt is `errdefer f()` / `errdefer { … }` (N8, 核心设计 §16):
	// **仅在函数因 Err 返回时**执行；与 defer 共用同一栈（LIFO），实参在注册点
	// 求值；正常返回不执行。所在函数必须有 Err 位，否则编译错。
	ErrDeferStmt struct {
		Call  Expr
		Block *Block
		Pos
	}
	// RegionStmt is `region { }`: 每轮执行为独立区域实例，弹出 = bump 指针
	// 复位 (核心设计 §五 R1)。块内逃逸 = 编译错误 (R2 checker)。
	RegionStmt struct {
		Body *Block
		Pos
	}
	// SpawnStmt is `spawn f()`; 只能出现在 scope { } 内 (红线 5, 词法级强制).
	// 文法形态详见开放点 O1。
	SpawnStmt struct {
		Call Expr
		Pos
	}
	// ScopeStmt is `scope { }`: 退出 join 全部任务; IsTimeout = `scope.timeout(ms) { }`
	// (N6: 到点取消该域内全部任务 —— 结构化取消，任务树向下传播).
	ScopeStmt struct {
		Body      *Block
		Timeout   Expr
		IsTimeout bool
		Pos
	}
	// SelectStmt is `select { case … }` (N6, 核心设计 §16): 多个通道/超时分支里
	// **任选一个就绪的**；锚点必须与选择顺序无关。
	SelectStmt struct {
		Arms []SelectArm
		Pos
	}
	// SelectArm 是一条分支：
	//   recv  `case v, ok = ch.recv() { … }`（1 或 2 个绑定名，`_` 允许）
	//   send  `case ch.send(v) { … }`
	//   time  `case time.after(ms) { … }`
	SelectArm struct {
		Kind    string // "recv" | "send" | "time"（"" = 检查器报错用）
		Targets []VarTarget
		Expr    Expr
		Block   *Block
		Pos
	}
)

func (*Block) stmtNode()      {}
func (*VarDecl) stmtNode()    {}
func (*ExprStmt) stmtNode()   {}
func (*IfStmt) stmtNode()     {}
func (*ForStmt) stmtNode()    {}
func (*ReturnStmt) stmtNode() {}
func (*BranchStmt) stmtNode() {}
func (*MatchStmt) stmtNode()  {}
func (*DeferStmt) stmtNode()    {}
func (*ErrDeferStmt) stmtNode() {}
func (*RegionStmt) stmtNode()   {}
func (*SpawnStmt) stmtNode()  {}
func (*ScopeStmt) stmtNode()  {}
func (*SelectStmt) stmtNode() {}

// --- declarations ---------------------------------------------------------------

type Decl interface {
	Node
	declNode()
}

type (
	ImportDecl struct {
		Name string
		Pos
	}
	// ConstDecl: 仅数值/bool/str 的折叠字面量算术，无函数调用 (核心设计 §三).
	ConstDecl struct {
		Name string
		Type TypeExpr // nil when inferred
		Init Expr
		Pos
	}
	// ComptimeBlock is the package-level `comptime { … }` (N4, 核心设计 §16):
	// 体里只允许 `const 名 = <可折叠表达式>` 与 `assert <可折叠 bool>`；
	// **全部在编译期求值，不生成任何运行期代码**（emit/air 都按未知 Decl 跳过）。
	// 它是"编译期语句化"的唯一落点：`fieldsOf(T)` 这类内省原语的合法归宿。
	ComptimeBlock struct {
		Body []Decl
		Pos
	}
	// ComptimeAssert is `assert <expr>` inside a comptime block. 条件必须折叠成
	// bool；false = 编译错（位置精确到断言点）。**只在 comptime 块内合法** ——
	// 因此 `assert` 仍是普通标识符，不占 23 关键字。
	ComptimeAssert struct {
		Cond Expr
		Pos
	}
	// Param is one function parameter: `名字 类型`.
	Param struct {
		Name string
		Type TypeExpr
		Pos
	}
	// Result is one return slot. ErrPosition is checked by checkErrLast (红线 2).
	Result struct {
		Type TypeExpr
		Pos
	}
	// FuncDecl covers free functions, methods (Recv = class name) and extern
	// bindings (Body == nil). 方法不可重载、无静态方法 (checker).
	FuncDecl struct {
		Recv       string
		Name       string
		TypeParams []string
		// Constraints 是泛型形参的约束（N3，核心设计 §16）：与 TypeParams 同名
		// （未写约束的形参不出现在这里）。
		Constraints []TypeParamConstraint
		Params      []Param
		Results     []Result
		Body        *Block
		Export      bool
		Extern      bool
		IsInit      bool
		Annotations []string
		Pos
	}
	// TypeParamConstraint 是 `T: Ord` / `T: Hash + Eq` / `T: Shape` / `T: value`。
	TypeParamConstraint struct {
		Name        string
		Constraints []string
		Pos
	}
	// FieldDecl is a class field with an optional default initializer.
	FieldDecl struct {
		Targets []VarTarget
		Type    TypeExpr
		Init    Expr
		Pos
	}
	// ClassDecl: 无继承——组合 + 接口 (核心设计 §四).
	ClassDecl struct {
		Name       string
		Packed     bool
		TypeParams []string
		// Constraints 是泛型形参的约束（N3）。
		Constraints []TypeParamConstraint
		Fields      []*FieldDecl
		Methods     []*FuncDecl
		// Uses 是显式委托（N10，核心设计 §16）：`use b B` 把 B 的方法提升到本类
		// （调用点 a.m() 转发到 a.b.m()）。字段仍在 A 内声明（局部性不破）；
		// 同名方法冲突时必须在本类显式实现。
		Uses        []UseDecl
		Annotations []string
		Pos
	}
	// UseDecl 是一条 `use <字段> <类型>`。
	UseDecl struct {
		Field string
		Type  TypeExpr
		Pos
	}
	// InterfaceDecl: 结构化满足，无 implements 关键字.
	InterfaceDecl struct {
		Name    string
		Methods []*FuncDecl // signature-only (Body == nil)
		// Embeds 是嵌入的接口（N9，核心设计 §16）：接口**并集**（槽位 = 嵌入接口
		// 槽位 + 自身方法，按声明序）。AfterMethod = 它前面已有几个方法，用于
		// 还原声明序（不是继承：无覆盖、无层次、无默认实现）。
		Embeds      []IfaceEmbed
		Annotations []string
		Pos
	}
	// IfaceEmbed 是一条嵌入项：本包名 `Reader` 或跨包 `io.Reader`。
	IfaceEmbed struct {
		Name        string
		AfterMethod int
		Pos
	}
	// EnumVariant is one case; Payload is nil for data-less variants.
	EnumVariant struct {
		Name    string
		Payload TypeExpr
		Pos
	}
	EnumDecl struct {
		Name        string
		Variants    []*EnumVariant
		Annotations []string
		Pos
	}
)

func (*ImportDecl) declNode()    {}
func (*ConstDecl) declNode()     {}
func (*FuncDecl) declNode()      {}
func (*ClassDecl) declNode()     {}
func (*InterfaceDecl) declNode() {}
func (*EnumDecl) declNode()      {}
func (*ComptimeBlock) declNode()  {}
func (*ComptimeAssert) declNode() {}

// File is a parsed translation unit.
type File struct {
	Path  string
	Decls []Decl
}
