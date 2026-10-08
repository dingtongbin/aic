package escape

import (
	"strings"
	"testing"

	"aic/internal/air"
)

// build 造一个最小模块：一个 region 块、一个带守卫的 store。
func build(limit string, live, declInRegion bool) (*air.Module, *air.Func) {
	loc := air.Loc{File: "t.aic", Line: 1, Col: 1}
	f := &air.Func{Sym: "t_f", Loc: loc}
	b := &air.Block{Label: "entry", Loc: loc}
	b.Insts = append(b.Insts,
		&air.Var{Name: "slot", Ty: "T*", Live: live, Loc: loc},
		&air.RegionEnter{TypeID: "r1", Loc: loc},
		&air.Var{Name: "inner", Ty: "T*", Init: "const 0", Loc: loc},
		&air.Store{Place: &air.VarPlace{Name: "slot"}, Val: "inner", Limit: limit, Loc: loc},
		&air.RegionExit{Loc: loc},
	)
	b.Term = &air.Ret{Loc: loc}
	f.Blocks = []*air.Block{b}
	m := &air.Module{Pkg: "t", Funcs: []*air.Func{f}}
	return m, f
}

// TestE1DeclarationDomain：存入「本 region 实例内声明」的槽 → E1 消去。
func TestE1DeclarationDomain(t *testing.T) {
	loc := air.Loc{File: "t.aic", Line: 2, Col: 1}
	f := &air.Func{Sym: "t_f", Loc: loc}
	b := &air.Block{Label: "entry", Loc: loc}
	b.Insts = append(b.Insts,
		&air.RegionEnter{TypeID: "r1", Loc: loc},
		&air.Var{Name: "slot", Ty: "T*", Loc: loc}, // 在 region 内声明
		&air.Var{Name: "inner", Ty: "T*", Init: "const 0", Loc: loc},
		&air.Store{Place: &air.VarPlace{Name: "slot"}, Val: "inner", Limit: "1", Loc: loc},
		&air.RegionExit{Loc: loc},
	)
	b.Term = &air.Ret{Loc: loc}
	f.Blocks = []*air.Block{b}
	m := &air.Module{Pkg: "t", Funcs: []*air.Func{f}}

	rep := Analyze(m)
	if rep.Eliminated != 1 || rep.ByRule["E1"] != 1 {
		t.Fatalf("E1 未消去：%s", rep.Summary())
	}
	if b.Insts[3].(*air.Store).Limit != "" {
		t.Fatal("消去后 Limit 未清空（守卫是 store 的修饰符）")
	}
	if len(rep.Elims) != 1 || rep.Elims[0].Rule != "E1" || rep.Elims[0].Reason == "" {
		t.Fatalf("消去记录不完整（V8 要求可复核 + 有理由）：%+v", rep.Elims)
	}
}

// TestE1KeepsLiveAndOuter：@live 变量与外层声明被内层写入的槽**必须保留**（E1 反例）。
func TestE1KeepsLiveAndOuter(t *testing.T) {
	// @live 变量：声明偏移 = 偏移 − 1 → 不满足 E1
	m, b := build("1", true, true)
	rep := Analyze(m)
	if rep.Eliminated != 0 {
		t.Fatalf("@live 槽被误消去：%s", rep.Summary())
	}
	if b.Blocks[0].Insts[3].(*air.Store).Limit == "" {
		t.Fatal("@live 槽的守卫被清空")
	}
	// 外层声明、内层 region 写入：DeclIn = false → 不满足 E1
	m2, _ := build("1", false, false)
	rep2 := Analyze(m2)
	if rep2.Eliminated != 0 {
		t.Fatalf("外层槽被误消去：%s", rep2.Summary())
	}
}

// TestE4ReturnEntry：limit = fnentry 且值界精确 0 → E4；非精确 → 保留。
func TestE4ReturnEntry(t *testing.T) {
	loc := air.Loc{File: "t.aic", Line: 4, Col: 1}
	mk := func(val string, rhs air.RHS) *air.Module {
		f := &air.Func{Sym: "t_g", Loc: loc}
		b := &air.Block{Label: "entry", Loc: loc}
		b.Insts = append(b.Insts,
			&air.Let{Tmp: "v", Ty: "T*", Rhs: rhs, Loc: loc},
			&air.Store{Place: &air.VarPlace{Name: "ret0"}, Val: val, Limit: "fnentry", Loc: loc},
		)
		b.Term = &air.Ret{Vals: []string{"ret0"}, Loc: loc}
		f.Blocks = []*air.Block{b}
		return &air.Module{Pkg: "t", Funcs: []*air.Func{f}}
	}
	// 精确 0（alloc task）→ E4 消去
	rep := Analyze(mk("v", &air.Alloc{Kind: "task", Ty: "T"}))
	if rep.Eliminated != 1 || rep.ByRule["E4"] != 1 {
		t.Fatalf("E4 未消去：%s", rep.Summary())
	}
	// 非精确（调用结果）→ 保留
	rep2 := Analyze(mk("v", &air.Call{Sym: "t_h"}))
	if rep2.Eliminated != 0 || rep2.Kept != 1 {
		t.Fatalf("非精确返回点被误消去：%s", rep2.Summary())
	}
}

// TestE2ExactBounds：宿主与值都是精确界且 D(v) ≤ D(h) → E2。
func TestE2ExactBounds(t *testing.T) {
	loc := air.Loc{File: "t.aic", Line: 5, Col: 1}
	f := &air.Func{Sym: "t_h", Loc: loc}
	b := &air.Block{Label: "entry", Loc: loc}
	b.Insts = append(b.Insts,
		&air.Let{Tmp: "host", Ty: "T*", Rhs: &air.Alloc{Kind: "task", Ty: "T"}, Loc: loc},
		&air.Let{Tmp: "val", Ty: "T*", Rhs: &air.Alloc{Kind: "task", Ty: "T"}, Loc: loc},
		&air.Store{Place: &air.FieldPlace{Base: &air.VarPlace{Name: "host"}, Idx: 0},
			Val: "val", Limit: "0", Loc: loc},
	)
	b.Term = &air.Ret{Loc: loc}
	f.Blocks = []*air.Block{b}
	rep := Analyze(&air.Module{Pkg: "t", Funcs: []*air.Func{f}})
	if rep.Eliminated != 1 || rep.ByRule["E2"] != 1 {
		t.Fatalf("E2 未消去：%s", rep.Summary())
	}
}

// TestSummaryShape：报告摘要形态（--report-guards 的 AIR 侧）。
func TestSummaryShape(t *testing.T) {
	m, _ := build("1", false, true)
	s := Analyze(m).Summary()
	if !strings.HasPrefix(s, "escape: total=") || !strings.Contains(s, "eliminated=") {
		t.Fatalf("摘要形态不符：%s", s)
	}
}
