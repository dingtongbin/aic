package emit

import (
	"fmt"
	"strings"
	"testing"

	"aic/internal/air"
	"aic/internal/cir"
	"aic/internal/parse"
	"aic/internal/types"
)

// emitIRFrom 跑 **§13.1 唯一管线**：降级 → 单态化 → 去糖 → 剪枝 → C 形态 →
// IR 后端发 C。这与 cmd/aic 的 emitIRUnit 是同一条路 —— 测试与生产走两条路时，
// 任何一边的断言都证明不了另一边（所以这里不复用任何 AST 直译入口）。
func emitIRFrom(t *testing.T, f *parse.File, info *types.Info, path string) (*Result, error) {
	t.Helper()
	var imports []string
	for im := range info.Imported {
		imports = append(imports, im)
	}
	m, err := air.Lower(air.LowerInput{Pkg: info.Pkg, Files: []*parse.File{f}, Info: info, Imports: imports, Mono: true})
	if err != nil {
		return nil, err
	}
	if vs := air.Verify(m); len(vs) > 0 {
		return nil, fmt.Errorf("IR verification failed: %s", vs[0].String())
	}
	if m, err = air.Monomorphize(m, info); err != nil {
		return nil, err
	}
	if vs := air.VerifyMono(m, info); len(vs) > 0 {
		return nil, fmt.Errorf("mair verification failed: %s", vs[0])
	}
	if m, err = air.Desugar(m); err != nil {
		return nil, err
	}
	if air.HasPrim(m) {
		return nil, fmt.Errorf("IR-1 primitives survived desugaring in package %s", info.Pkg)
	}
	for _, fn := range m.Funcs {
		air.PruneUnreachable(fn)
	}
	cp, err := cir.Build(m)
	if err != nil {
		return nil, err
	}
	ir := NewIRProg()
	for _, fn := range cp.Funcs {
		if err := ir.Add(fn.Sym, fn); err != nil {
			return nil, err
		}
	}
	return EmitIRUnit(Single(f, path, info), ir)
}

// emitSrc 走全管线（lex → parse → types → 唯一 IR 管线 → C），返回 C 文本。
func emitSrc(t *testing.T, src string) string {
	t.Helper()
	f, lexErrs, parseErrs := parse.Source("t.aic", src)
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		t.Fatalf("语料本身不干净：lex=%d parse=%d", len(lexErrs), len(parseErrs))
	}
	res := types.CheckFile(f)
	if len(res.Errors) > 0 {
		t.Fatalf("检查器报错：\n%s", res.Errors[0].Format())
	}
	out, err := emitIRFrom(t, f, res.Info, "t.aic")
	if err != nil {
		t.Fatalf("emit 失败：%v", err)
	}
	return out.C
}

// TestEmitIsPure:「emit 纯函数，无全局状态」—— 同一 AST 两次 emit 必须
// 逐字节相同（同时也是 H2 构建确定性的第一层）。
func TestEmitIsPure(t *testing.T) {
	src := `import fmt
import str

class Box {
    var n i32
}

func make(v i32) -> Box {
    return Box{n: v}
}

func main() {
    var xs i32[] = [1, 2, 3]
    var m map[str]i32 = map.new()
    m.put("k", 7)
    var b = make(5)
    fmt.println(str.fromI64(i64(b.n)))
    fmt.println(str.fromI64(i64(xs.len())))
    region {
        var inner = Box{n: 9}
        fmt.println(str.fromI64(i64(inner.n)))
    }
}
`
	first := emitSrc(t, src)
	second := emitSrc(t, src)
	if first != second {
		t.Errorf("两次 emit 文本不一致（emit 非纯函数？红线 12）\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s",
			first, second)
	}
}

// TestManglingTableIsSingle:「mangling 单表」—— 内部符号是**哈希定长**的，
// 断言的是规则的四条不变量（不是具体字面量：哈希会随版本变，但不变量不会）：
//
//	① 前缀恒为 aic_，长度恒为 aic_ + 16 hex；
//	② 同名不同包（路径不同）必须得到不同符号 —— 这条是大项目不撞名的底线；
//	③ 同输入恒等（确定性，H2）；
//	④ 实参集不同 → 泛型实例符号不同；
//	⑤ export/extern 原名直出，零 mangle。
func TestManglingTableIsSingle(t *testing.T) {
	hexOK := func(s, what string) {
		t.Helper()
		if len(s) != len("aic_")+16 || s[:4] != "aic_" {
			t.Errorf("%s：形态应为 aic_<16 hex>，得到 %q", what, s)
			return
		}
		for _, r := range s[4:] {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Errorf("%s：哈希位必须是小写十六进制，得到 %q", what, s)
				return
			}
		}
	}
	free := MangleFunc("app/core", "", "work")
	hexOK(free, "自由函数")
	hexOK(MangleFunc("app/core", "Buf", "size"), "方法（this 首位）")
	hexOK(MangleGeneric("app/core", "Box", []string{"i32"}), "泛型实例")
	hexOK(mangleType("app/core", "Buf"), "具名类型")

	if free != MangleFunc("app/core", "", "work") {
		t.Error("同输入必须恒等（H2 确定性）")
	}
	if free == MangleFunc("app/ui", "", "work") {
		t.Error("同名不同包（app/core vs app/ui）撞了符号 —— 多模块工程直接不可用")
	}
	if MangleFunc("app/core", "", "work") == MangleFunc("app/core", "Buf", "work") {
		t.Error("自由函数与同名方法撞了符号")
	}
	if MangleGeneric("app/core", "Box", []string{"i32"}) == MangleGeneric("app/core", "Box", []string{"str"}) {
		t.Error("不同泛型实例撞了符号")
	}
	if got := ExternName("sqlite3_open"); got != "sqlite3_open" {
		t.Errorf("extern 必须原名直出（零 mangle），得到 %q", got)
	}
}

// TestLineDirectivesCoverEverySite:「#line 全覆盖，禁裸 emit」—— 每个
// 函数定义与语句块前都必须有回指 .aic 的 #line。
func TestLineDirectivesCoverEverySite(t *testing.T) {
	src := `import fmt
import str

func helper(v i32) -> i32 {
    if v > 0 {
        return v
    }
    return 0
}

func main() {
    var n i32 = helper(3)
    fmt.println(str.fromI64(i64(n)))
}
`
	out := emitSrc(t, src)
	if !strings.Contains(out, `#line 4 "t.aic"`) {
		t.Errorf("#line 未覆盖函数体起始行：\n%s", out)
	}
	if !strings.Contains(out, `#line 5 "t.aic"`) {
		t.Errorf("#line 未覆盖 if 语句行：\n%s", out)
	}
	if !strings.Contains(out, `#line 11 "t.aic"`) {
		t.Errorf("#line 未覆盖变量声明的下一行语句：\n%s", out)
	}
	// main 的 wrapper 必须建任务根区域并注册 os 参数（main 内逃逸语义的前提）
	if !strings.Contains(out, "aic_task_init(") || !strings.Contains(out, "aic_os_init(") {
		t.Errorf("入口未建任务区域/未注册 os 参数：\n%s", out)
	}
}

// TestGuardEmittedAtStorePoint: 存储点守卫的发射形态与触发条件由端到端语料
// 与 runtime 冒烟测试钉住（runtime/smoke/smoke_deeper.c、smoke_escape.c 已验
// trap 前缀与退出码）；emit 侧这里只钉「守卫发射后仍是 AIC_GUARD 形态」这一
// 事实，避免出现自造的第二套守卫写法。
func TestGuardMacroIsTheOnlyGuardForm(t *testing.T) {
	src := `import fmt
import str

class Box {
    var n i32
}

func fill(xs Box[]) {
    region {
        var inner = Box{n: 2}
        xs.append(inner)
    }
    fmt.println(str.fromI64(i64(xs.len())))
}

func main() {
    var xs Box[] = []
    fill(xs)
}
`
	out := emitSrc(t, src)
	if strings.Contains(out, "aic_guard(") && !strings.Contains(out, "AIC_GUARD(") {
		t.Errorf("直接调用了 aic_guard 而未走 AIC_GUARD 宏（守卫形态必须单一）：\n%s", out)
	}
}

// TestProvableDeeperStoreIsCompileError: 「块内创建直接存入更浅存储」是
// 里句法可判的一类，只有两种合法结局，二者必居其一且互斥：
//
//	a) 检查期拒（可证必违 = 编译错），或
//	b) 发射期带守卫（不可证 = 运行期 trap）。
//
// 绝不允许「既不拒也不守」——那才是真正的漏洞（写而不验的反面：漏而不报）。
func TestProvableDeeperStoreIsCompileError(t *testing.T) {
	src := `class Box {
    var n i32
}

func main() {
    var xs Box[] = []
    region {
        var inner = Box{n: 1}
        xs.append(inner)
    }
}
`
	f, lexErrs, parseErrs := parse.Source("main/t.aic", src)
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		t.Fatalf("语料本身不干净：lex=%d parse=%d", len(lexErrs), len(parseErrs))
	}
	res := types.CheckFile(f)
	if len(res.Errors) > 0 {
		return // (a) 检查期已拒
	}
	out, err := emitIRFrom(t, f, res.Info, "main/t.aic")
	if err != nil {
		t.Fatalf("检查期未拒，发射期又失败：%v", err)
	}
	if !strings.Contains(out.C, "AIC_GUARD(") {
		t.Errorf("跨区存储既不拒也不守（§五 R5 破了）：\n%s", out.C)
	}
}

// TestRegionPushPop: region 块必须发射 push/pop（弹出 = bump 复位）。
func TestRegionPushPop(t *testing.T) {
	src := `import fmt
import str

func main() {
    region {
        fmt.println("in")
    }
    fmt.println("out")
}
`
	out := emitSrc(t, src)
	if !strings.Contains(out, "aic_region_push(") || !strings.Contains(out, "aic_region_pop()") {
		t.Errorf("region 块未发射 push/pop：\n%s", out)
	}
}
