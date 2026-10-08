package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// 作用域与符号（核心设计 §三 遮蔽规则：内层可遮蔽、同块重复声明 = 编译错、
// 参数视为函数体块的既有名）。Scope.Offset = 该块的 region 偏移——同一块内
// 声明的变量共享声明偏移（§五 R3 传播规则：变量读界 = 声明偏移）。
// ---------------------------------------------------------------------------

type SymKind int

const (
	SymVar SymKind = iota // 局部变量 / 参数
	SymConst
	SymFunc
	SymType // class / interface / enum
	SymTypeParam
)

type Symbol struct {
	Kind       SymKind
	Name       string
	Type       Type
	DeclOffset int // SymVar: region 偏移（@live 已减一）
	Live       bool
	// Param = 形参（含 this、闭包形参）：其界只是"绝对深度 ≤ 入口"，与另一个形参
	// 之间没有顺序 —— 存储点判定靠它（见 bound.param / markStoreGuard）。
	Param   bool
	Private bool // 非公开（首字母非大写）= 包私有（核心设计 §四）
	Const   ConstVal
	Sig     *FuncSig // SymFunc
}

// FuncSig 是函数/方法的签名 + R3 返回摘要（必须 = 0，见 checker）。
type FuncSig struct {
	Name       string
	Recv       string // 类名；空 = 自由函数
	Params     []string
	ParamTypes []Type
	Results    []Type
	Export     bool
	Extern     bool
	IsInit     bool
	TypeParams []string
	// Constraints 是泛型形参的约束（N3）：调用点/实例化点校验。
	Constraints []parse.TypeParamConstraint
	Summary     int // 返回界摘要（相对入口；不变量 = 0）
	// Derived 报告该方法是 @derive 生成的（无源码声明，方法体由 emit 按字段序合成）。
	Derived bool
	// NoRecv 报告该方法是**类型级**的（无接收者）：调用点写 `Class.method(...)`，
	// 实参表里没有 this（N4 的 @derive(Default) 生成 default() -> T）。
	NoRecv bool
	// DelegateField / DelegateClass 非空 = 该方法是**委托方法**（N10）：本类没有
	// 方法体，调用转发到 `this.<DelegateField>`（其类型 = DelegateClass）的同名方法。
	DelegateField string
	DelegateClass string
}

type ConstVal struct {
	Kind  ConstKind
	Int   int64
	Uint  uint64
	Float float64
	Str   string
	Bool  bool
}

type ConstKind int

const (
	ConstNone ConstKind = iota
	ConstInt            // 带符号（i64 域）
	ConstUint           // 无符号（u64 域，> i64 max 的字面量）
	ConstFloat
	ConstStr
	ConstBool
)

type Scope struct {
	parent *Scope
	names  map[string]*Symbol
	Offset int
	// LoopDepth > 0 时 break/continue 合法（核心设计 §三）
	LoopDepth int
	// InRegion 标记本块是否 region 块（偏移由 checker 维护在 Offset 里）
}

func NewScope(parent *Scope, offset int) *Scope {
	return &Scope{parent: parent, names: map[string]*Symbol{}, Offset: offset}
}

// Declare 同块重复声明 = 编译错误（返回 false，由调用方报三段式）。
func (s *Scope) Declare(sym *Symbol) bool {
	if _, exists := s.names[sym.Name]; exists {
		return false
	}
	s.names[sym.Name] = sym
	return true
}

// Lookup 沿作用域链查找。
func (s *Scope) Lookup(name string) (*Symbol, bool) {
	for cur := s; cur != nil; cur = cur.parent {
		if sym, ok := cur.names[name]; ok {
			return sym, true
		}
	}
	return nil, false
}

// LookupLocal 只查本块（参数/变量重复声明判定用）。
func (s *Scope) LookupLocal(name string) (*Symbol, bool) {
	sym, ok := s.names[name]
	return sym, ok
}

// LoopScope 沿链找最近的循环层深。
func (s *Scope) LoopScope() int {
	for cur := s; cur != nil; cur = cur.parent {
		if cur.LoopDepth > 0 {
			return cur.LoopDepth
		}
	}
	return 0
}
