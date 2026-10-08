package emit

import (
	"strings"
	"testing"

	"aic/internal/parse"
	"aic/internal/types"
)

// emitSrc 走全管线（lex → parse → types → emit），返回 C 文本。
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
	out, err := Emit(f, "t.aic", res.Info)
	if err != nil {
		t.Fatalf("emit 失败：%v", err)
	}
	return out.C
}

// TestEmitIsPure: 红线 12「emit 纯函数，无全局状态」—— 同一 AST 两次 emit 必须
// 逐字节相同（同时也是 H2 构建确定性的第一层）。
func TestEmitIsPure(t *testing.T) {
	src := `import print

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
    print.println(b.n)
    print.println(xs.len())
    region {
        var inner = Box{n: 9}
        print.println(inner.n)
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

// TestManglingTableIsSingle: 红线 10「mangling 单表」—— 函数/方法/泛型/类型
// 四条命名规则各钉一个锚点；改动命名 = 这里的断言先红。
func TestManglingTableIsSingle(t *testing.T) {
	cases := []struct {
		got, want, what string
	}{
		{MangleFunc("main", "", "work"), "aic_main_work", "自由函数"},
		{MangleFunc("main", "Buf", "size"), "aic_main_Buf_size", "方法（this 首位）"},
		{MangleGeneric("main", "Box", []string{"i32"}), "aic_main_Box__i32", "泛型实例"},
		{MangleGeneric("main", "Pair", []string{"i32", "str"}), "aic_main_Pair__i32_str", "多实参双下划线分隔"},
		{mangleType("main", "Buf"), "aic_main_Buf", "具名类型"},
		{ExternName("sqlite3_open"), "sqlite3_open", "extern 原名直出（零 mangle）"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s：mangling 得到 %q，期望 %q", tc.what, tc.got, tc.want)
		}
	}
}

// TestLineDirectivesCoverEverySite: 红线 9「#line 全覆盖，禁裸 emit」—— 每个
// 函数定义与语句块前都必须有回指 .aic 的 #line。
func TestLineDirectivesCoverEverySite(t *testing.T) {
	src := `import print

func helper(v i32) -> i32 {
    if v > 0 {
        return v
    }
    return 0
}

func main() {
    var n i32 = helper(3)
    print.println(n)
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
	// main 的 wrapper 必须建任务根区域并注册 os 参数（§六 main 内逃逸语义的前提）
	if !strings.Contains(out, "aic_task_init(") || !strings.Contains(out, "aic_os_init(") {
		t.Errorf("入口未建任务区域/未注册 os 参数：\n%s", out)
	}
}

// TestGuardEmittedAtStorePoint: 存储点守卫的发射形态与触发条件由端到端语料
// 与 runtime 冒烟测试钉住（runtime/smoke/smoke_deeper.c、smoke_escape.c 已验
// trap 前缀与退出码）；emit 侧这里只钉「守卫发射后仍是 AIC_GUARD 形态」这一
// 事实，避免出现自造的第二套守卫写法。
func TestGuardMacroIsTheOnlyGuardForm(t *testing.T) {
	src := `import print

class Box {
    var n i32
}

func fill(xs Box[]) {
    region {
        var inner = Box{n: 2}
        xs.append(inner)
    }
    print.println(xs.len())
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

// TestProvableDeeperStoreIsCompileError: 「块内创建直接存入更浅存储」是 §五 R5
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
	out, err := Emit(f, "main/t.aic", res.Info)
	if err != nil {
		t.Fatalf("检查期未拒，发射期又失败：%v", err)
	}
	if !strings.Contains(out.C, "AIC_GUARD(") {
		t.Errorf("跨区存储既不拒也不守（§五 R5 破了）：\n%s", out.C)
	}
}

// TestRegionPushPop: region 块必须发射 push/pop（弹出 = bump 复位，§五 R1）。
func TestRegionPushPop(t *testing.T) {
	src := `import print

func main() {
    region {
        print.println("in")
    }
    print.println("out")
}
`
	out := emitSrc(t, src)
	if !strings.Contains(out, "aic_region_push(") || !strings.Contains(out, "aic_region_pop()") {
		t.Errorf("region 块未发射 push/pop：\n%s", out)
	}
}
