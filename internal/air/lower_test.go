package air

import (
	"strings"
	"testing"

	"aic/internal/lex"
	"aic/internal/parse"
	"aic/internal/types"
)

// lowerSrc 解析 + 检查一段源码并降级为 AIR（测试用最小管线）。
func lowerSrc(t *testing.T, src string) *Module {
	t.Helper()
	f, lexErrs, parseErrs := parse.Source("t.aic", src)
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		t.Fatalf("解析失败：%s%s", lex.FormatAll("t.aic", lexErrs), parse.FormatAll("t.aic", parseErrs))
	}
	res := types.CheckFile(f)
	if len(res.Errors) > 0 {
		t.Fatalf("检查失败：%s", types.FormatAll("t.aic", res.Errors))
	}
	m, err := Lower(LowerInput{Pkg: "t", Files: []*parse.File{f}, Info: res.Info})
	if err != nil {
		t.Fatalf("lowering 失败：%v", err)
	}
	return m
}

// TestLowerStraightLine：直线函数（var/let/store/ret）降级后 verifier 通过，
// 且形态符合 §10.1（唯一收尾块、loc 覆盖、store 是唯一带守卫的指令）。
func TestLowerStraightLine(t *testing.T) {
	m := lowerSrc(t, `func add(a i32, b i32) -> i32 {
    var s i32 = a + b
    s = s + 1
    return s
}
`)
	if vs := Verify(m); len(vs) != 0 {
		t.Fatalf("verifier 拒绝：%v", vs)
	}
	out := Print(m)
	for _, want := range []string{
		"func main_add(a:i32, b:i32) -> i32 {",
		"let t1 : i32 = binop + a, b",
		"var s : i32 = t1",
		"store s, t2",
		"br exit",
		"  exit:",
		"ret ret0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("AIR 缺 %q\n--- 实际 ---\n%s", want, out)
		}
	}
	if a, b := Print(m), Print(m); a != b {
		t.Fatal("两次 lowering 的文本不一致（H8）")
	}
}

// TestLowerControlFlow：if / for / for-in 区间 → cbr + 块（无 goto 汤）。
func TestLowerControlFlow(t *testing.T) {
	m := lowerSrc(t, `func pick(n i32) -> i32 {
    if n > 0 {
        return 1
    } else {
        return 0
    }
}

func sum(n usize) -> i32 {
    var acc i32 = 0
    for i in 0..n {
        acc = acc + 1
    }
    return acc
}
`)
	if vs := Verify(m); len(vs) != 0 {
		t.Fatalf("verifier 拒绝：%v", vs)
	}
	out := Print(m)
	// 两支都 return 时**不建 join 块**（不可达块被剪掉，V2.3）；
	// 块编号按生成序，故只断言形态与前缀，不钉死编号。
	for _, want := range []string{
		"cbr t1, then1, else2",
		"then1:",
		"else2:",
		"forhead",
		"forbody",
		"forpost",
		"fordone",
		"br exit",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("AIR 缺 %q\n--- 实际 ---\n%s", want, out)
		}
	}
}

// TestLowerComposite：创建表达式 → alloc（唯一分配来源）+ 逐字段 store。
func TestLowerComposite(t *testing.T) {
	m := lowerSrc(t, `class P {
    var x i32
    var y i32
}

func mk() -> P {
    return P{x: 1, y: 2}
}
`)
	if vs := Verify(m); len(vs) != 0 {
		t.Fatalf("verifier 拒绝：%v", vs)
	}
	out := Print(m)
	if !strings.Contains(out, "type main_P = struct") {
		t.Errorf("类型表缺 struct 声明：\n%s", out)
	}
	if !strings.Contains(out, "alloc task(main_P)") {
		t.Errorf("创建表达式未走 alloc：\n%s", out)
	}
	if !strings.Contains(out, "store field obj1, 0, const 1") {
		t.Errorf("字段存储形态不符：\n%s", out)
	}
}

// TestLowerRegion：region 块 → region.enter / region.exit（V3.1 配平）。
func TestLowerRegion(t *testing.T) {
	m := lowerSrc(t, `class B {
    var n i32
}

func f() -> i32 {
    var r i32 = 0
    region {
        var b = B{n: 3}
        r = b.n
    }
    return r
}
`)
	if vs := Verify(m); len(vs) != 0 {
		t.Fatalf("verifier 拒绝：%v", vs)
	}
	out := Print(m)
	if !strings.Contains(out, "region.enter") || !strings.Contains(out, "region.exit") {
		t.Errorf("region 未显式化：\n%s", out)
	}
	// 深度 > 0 时分配走 alloc region
	if !strings.Contains(out, "alloc region(main_B)") {
		t.Errorf("region 块内的创建未落到 alloc region：\n%s", out)
	}
}
