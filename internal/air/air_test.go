package air

import (
	"strings"
	"testing"
)

// 一个最小的合法模块：struct 声明 + 一个函数（let/store/ret），loc 全覆盖。
func sampleModule() *Module {
	loc := Loc{File: "t.aic", Line: 3, Col: 1}
	return &Module{
		Pkg: "main",
		Types: []*TypeDecl{{
			Sym: "aic_main_Point", Kind: "struct", Loc: loc,
			Fields: []Field{{Idx: 0, Name: "x", Ty: "i32"}, {Idx: 1, Name: "y", Ty: "i32"}},
		}},
		Funcs: []*Func{{
			Sym:    "aic_main_add",
			Params: []Param{{Name: "a", Ty: "i32"}, {Name: "b", Ty: "i32"}},
			Rets:   []string{"i32"},
			Loc:    loc,
			Blocks: []*Block{{
				Label: "entry", Loc: loc,
				Insts: []Inst{
					&Let{Tmp: "t1", Ty: "i32", Rhs: &Binop{Op: "add", A: "a", B: "b"}, Loc: loc},
				},
				Term: &Ret{Vals: []string{"t1"}, Loc: loc},
			}},
		}},
	}
}

// TestPrintGrammarShape：打印形态符合 §10.1.1（module 头 / 指令一行一条 / loc 前缀）。
func TestPrintGrammarShape(t *testing.T) {
	out := Print(sampleModule())
	for _, want := range []string{
		"air v1 module main",
		"type aic_main_Point = struct",
		"    0:x:i32",
		"func aic_main_add(a:i32, b:i32) -> i32 {",
		"  entry:",
		"    loc t.aic:3:1 let t1 : i32 = binop add a, b",
		"    loc t.aic:3:1 ret t1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("打印缺 %q\n--- 实际 ---\n%s", want, out)
		}
	}
}

// TestPrintDeterministic：同一输入两次 dump 逐字节一致（V7.1/H8）。
func TestPrintDeterministic(t *testing.T) {
	a, b := Print(sampleModule()), Print(sampleModule())
	if a != b {
		t.Fatalf("两次 dump 不一致：\n%s\n---\n%s", a, b)
	}
}

// TestVerifyAcceptsValid：合法模块零违规。
func TestVerifyAcceptsValid(t *testing.T) {
	if vs := Verify(sampleModule()); len(vs) != 0 {
		t.Fatalf("合法模块被拒：%v", vs)
	}
}

// TestVerifyRejects：逐条反例（每条只违反一条规则，期望被拒且规则号正确）。
func TestVerifyRejects(t *testing.T) {
	loc := Loc{File: "t.aic", Line: 1, Col: 1}
	cases := []struct {
		name string
		rule string
		mut  func(m *Module)
	}{
		{"V1.4 字段下标非声明序", "V1.4", func(m *Module) {
			m.Types[0].Fields[1].Idx = 5
		}},
		{"V1.5 变体 tag 非声明序", "V1.5", func(m *Module) {
			m.Types = append(m.Types, &TypeDecl{
				Sym: "aic_main_E", Kind: "enum", Loc: loc,
				Variants: []Variant{{Tag: 3, Name: "A"}, {Tag: 1, Name: "B"}},
			})
		}},
		{"V2.1 块无终结符", "V2.1", func(m *Module) {
			m.Funcs[0].Blocks[0].Term = nil
		}},
		{"V2.2 跳转到不存在的标签", "V2.2", func(m *Module) {
			m.Funcs[0].Blocks[0].Term = &Br{Label: "nope", Loc: loc}
		}},
		{"V2.3 不可达块", "V2.3", func(m *Module) {
			m.Funcs[0].Blocks = append(m.Funcs[0].Blocks, &Block{
				Label: "dead", Loc: loc,
				Insts: []Inst{&Let{Tmp: "t9", Ty: "i32", Rhs: &Const{Lit: "1"}, Loc: loc}},
				Term:  &Ret{Vals: []string{"t9"}, Loc: loc},
			})
		}},
		{"V2.4 未定义先用", "V2.4", func(m *Module) {
			m.Funcs[0].Blocks[0].Insts[0] = &Let{Tmp: "t1", Ty: "i32",
				Rhs: &Binop{Op: "add", A: "zzz", B: "b"}, Loc: loc}
		}},
		{"V2.5 临时量重复定义", "V2.5", func(m *Module) {
			m.Funcs[0].Blocks[0].Insts = append(m.Funcs[0].Blocks[0].Insts,
				&Let{Tmp: "t1", Ty: "i32", Rhs: &Const{Lit: "2"}, Loc: loc})
		}},
		{"V3.1 region exit 无对应 enter", "V3.1", func(m *Module) {
			m.Funcs[0].Blocks[0].Insts = append([]Inst{
				&RegionExit{Loc: loc},
			}, m.Funcs[0].Blocks[0].Insts...)
		}},
		{"V3.1 跨块区域嵌套不结构化", "V3.1", func(m *Module) {
			// enter 后分岔：一支 pop、一支不 pop，汇合点深度不一致。
			// 这正是"region 含循环"的正确口径要**通过**、而漏 pop 要**拒**的分界。
			f := m.Funcs[0]
			f.Blocks[0].Insts = append([]Inst{&RegionEnter{TypeID: "r1", Loc: loc}},
				f.Blocks[0].Insts...)
			f.Blocks[0].Term = &Cbr{Cond: "t1", Then: "b_pop", Else: "b_nopop", Loc: loc}
			f.Blocks = append(f.Blocks,
				&Block{Label: "b_pop", Insts: []Inst{&RegionExit{Loc: loc}},
					Term: &Br{Label: "b_join", Loc: loc}},
				&Block{Label: "b_nopop", Term: &Br{Label: "b_join", Loc: loc}},
				&Block{Label: "b_join", Term: &Ret{Vals: []string{"t1"}, Loc: loc}})
		}},
		{"V4.2 无 Err 位却 checkfail", "V4.2", func(m *Module) {
			m.Funcs[0].Blocks[0].Term = &CheckFail{Err: "t1", Tmp: "t2", Loc: loc}
		}},
		{"V7.1 指令缺 loc", "V7.1", func(m *Module) {
			m.Funcs[0].Blocks[0].Insts[0].(*Let).Loc = Loc{}
		}},
		{"V7.3 @live 不在 region 块内", "V7.3", func(m *Module) {
			m.Funcs[0].Blocks[0].Insts = append([]Inst{
				&Var{Name: "x", Ty: "aic_main_Point*", Live: true, Loc: loc},
			}, m.Funcs[0].Blocks[0].Insts...)
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			m := sampleModule()
			tc.mut(m)
			vs := Verify(m)
			if len(vs) == 0 {
				t.Fatalf("反例未被拒（期望违反 %s）", tc.rule)
			}
			found := false
			for _, v := range vs {
				if v.Rule == tc.rule {
					found = true
				}
			}
			if !found {
				t.Fatalf("拒绝理由不含 %s：%v", tc.rule, vs)
			}
		})
	}
}
