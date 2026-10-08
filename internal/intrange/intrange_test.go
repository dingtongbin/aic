package intrange

import (
	"testing"

	"aic/internal/air"
)

func loc() air.Loc { return air.Loc{File: "t.aic", Line: 1, Col: 1} }

// fixedArrayLoop：`var xs [i32;4]` + `for i in 0..4 { store elem xs, i }`
// 循环头的条件事实给出 i ∈ [0,3]，定长数组长度 = 4 → 可证在界内。
func fixedArrayLoop() *air.Module {
	l := loc()
	f := &air.Func{
		Sym:    "t_f",
		Params: nil,
		Loc:    l,
	}
	entry := &air.Block{Label: "entry", Loc: l}
	entry.Insts = []air.Inst{
		&air.Var{Name: "xs", Ty: "[i32;4]", Loc: l},
		&air.Var{Name: "i", Ty: "usize", Init: "const 0", Loc: l},
	}
	entry.Term = &air.Br{Label: "forhead", Loc: l}

	head := &air.Block{Label: "forhead", Loc: l}
	head.Insts = []air.Inst{
		&air.Let{Tmp: "c", Ty: "bool", Rhs: &air.Cmp{Op: "lt", A: "i", B: "const 4"}, Loc: l},
	}
	head.Term = &air.Cbr{Cond: "c", Then: "forbody", Else: "fordone", Loc: l}

	body := &air.Block{Label: "forbody", Loc: l}
	body.Insts = []air.Inst{
		&air.Store{Place: &air.ElemPlace{Base: &air.VarPlace{Name: "xs"}, Idx: "i"},
			Val: "const 1", Loc: l},
	}
	body.Term = &air.Br{Label: "forpost", Loc: l}

	post := &air.Block{Label: "forpost", Loc: l}
	post.Insts = []air.Inst{
		&air.Let{Tmp: "n", Ty: "usize", Rhs: &air.Binop{Op: "+", A: "i", B: "const 1"}, Loc: l},
		&air.Store{Place: &air.VarPlace{Name: "i"}, Val: "n", Loc: l},
	}
	post.Term = &air.Br{Label: "forhead", Loc: l}

	done := &air.Block{Label: "fordone", Loc: l}
	done.Term = &air.Ret{Loc: l}

	f.Blocks = []*air.Block{entry, head, body, post, done}
	return &air.Module{Pkg: "t", Funcs: []*air.Func{f}}
}

// TestProvesFixedArrayLoop：定长数组 `[i32;4]` + `for i in 0..4` → 归纳事实给出
// i ∈ [0,3] < 4 → **可证在界内**（§10.4 第 12 条② unchecked 访问器的前提）。
func TestProvesFixedArrayLoop(t *testing.T) {
	f := Prove(fixedArrayLoop())
	if len(f.Accesses) != 1 {
		t.Fatalf("访问事实数 = %d，期望 1", len(f.Accesses))
	}
	a := f.Accesses[0]
	if !a.InBound {
		t.Fatalf("未证明在界内：%+v", a)
	}
	if a.IdxIv.Lo != 0 || a.IdxIv.Hi != 3 {
		t.Fatalf("归纳区间不符（应为 [0,3]）：%+v", a.IdxIv)
	}
	if f.Proved != 1 || f.Unknown != 0 {
		t.Fatalf("计数不符：%s", f.Summary())
	}
}

// TestKeepsDynamicLength：长度未知（切片）→ 不得证明（宁保守插检查）。
func TestKeepsDynamicLength(t *testing.T) {
	m := fixedArrayLoop()
	// 把定长数组换成切片：长度是运行期值 → 必须判 unknown
	m.Funcs[0].Blocks[0].Insts[0] = &air.Var{Name: "xs", Ty: "i32[]", Loc: loc()}
	f := Prove(m)
	if len(f.Accesses) != 1 || f.Accesses[0].InBound {
		t.Fatalf("动态长度被误证：%+v", f.Accesses)
	}
	if f.Unknown != 1 {
		t.Fatalf("未知计数不符：%s", f.Summary())
	}
}

// TestKeepsOutOfRange：循环上界 > 数组长度 → 不得证明（反例：越界仍要检查）。
func TestKeepsOutOfRange(t *testing.T) {
	m := fixedArrayLoop()
	// 循环到 8（数组只有 4 个元素）→ i 可达 7 ≥ 4
	m.Funcs[0].Blocks[1].Insts[0] = &air.Let{Tmp: "c", Ty: "bool",
		Rhs: &air.Cmp{Op: "lt", A: "i", B: "const 8"}, Loc: loc()}
	f := Prove(m)
	if f.Accesses[0].InBound {
		t.Fatalf("越界访问被误证在界内：%+v", f.Accesses[0])
	}
}

// TestNegativeIndexRejected：下界可能为负 → 不得证明。
func TestNegativeIndexRejected(t *testing.T) {
	m := fixedArrayLoop()
	// 起始 i = -1（用 const -1 表达），循环头只给上界
	m.Funcs[0].Blocks[0].Insts[1] = &air.Var{Name: "i", Ty: "usize", Init: "const -1", Loc: loc()}
	f := Prove(m)
	if f.Accesses[0].InBound {
		t.Fatalf("负下界被误证：%+v", f.Accesses[0])
	}
}

// TestIntervalArithmetic：区间算术与溢出保守。
func TestIntervalArithmetic(t *testing.T) {
	if got := point(2).add(point(3)); got.Lo != 5 || got.Hi != 5 {
		t.Fatalf("add: %+v", got)
	}
	if got := point(10).sub(point(4)); got.Lo != 6 || got.Hi != 6 {
		t.Fatalf("sub: %+v", got)
	}
	big := point(1 << 62)
	if got := big.add(big); got.Known {
		t.Fatalf("溢出应判 unknown：%+v", got)
	}
	if got := big.mul(big); got.Known {
		t.Fatalf("乘法溢出应判 unknown：%+v", got)
	}
	if got := unknown().add(point(1)); got.Known {
		t.Fatalf("未知传播错误：%+v", got)
	}
}
