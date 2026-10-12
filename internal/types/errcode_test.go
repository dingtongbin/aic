package types

import (
	"strconv"
	"testing"

	"aic/internal/parse"
)

// TestErrCodePrefixDeterministic：前缀只由包身份决定（H2：同输入两次编译一致）。
func TestErrCodePrefixDeterministic(t *testing.T) {
	a := ErrCodePrefix("app/core")
	b := ErrCodePrefix("app/core")
	if a != b {
		t.Fatalf("前缀不确定：%#x vs %#x", a, b)
	}
	if a&^errCodeMask != 0 {
		t.Fatalf("前缀低 12 位必须为 0（掩码 0xFFFFF000）：%#x", a)
	}
	// 抽两个具体包名做"应当不同"的弱断言（强断言见冲突检测测试）。
	if ErrCodePrefix("app/core") == ErrCodePrefix("app/ui") {
		t.Fatalf("app/core 与 app/ui 前缀相同（不影响正确性，但测试失去意义）")
	}
}

// TestErrCodeValueOrdering：变体序号进低 12 位，且互不相等。
func TestErrCodeValueOrdering(t *testing.T) {
	c0 := ErrCodeValue("app/core", 0)
	c1 := ErrCodeValue("app/core", 1)
	if c0 == c1 {
		t.Fatalf("序号进了码：%d vs %d", c0, c1)
	}
	if uint32(c0)&^errCodeMask != 0 {
		t.Fatalf("序号必须落在低 12 位：%#x", c0)
	}
	if c1-c0 != 1 {
		t.Fatalf("相邻序号应差 1：%d", c1-c0)
	}
}

// TestErrCodeConflictsDetected：暴力找一对**同前缀**的不同包名（20 位 → 生日界
// 期望 ~2^10 次），冲突检测必须报出这一对（C26）。
func TestErrCodeConflictsDetected(t *testing.T) {
	seen := map[uint32]string{}
	var a, b string
	for i := 0; i < 200000 && b == ""; i++ {
		s := "pkg" + strconv.Itoa(i)
		p := ErrCodePrefix(s)
		if prev, ok := seen[p]; ok && prev != s {
			a, b = prev, s
			break
		}
		seen[p] = s
	}
	if b == "" {
		t.Skip("未找到同前缀包名对（哈希不满足生日界，环境异常）")
	}
	owners := []ErrCodeOwner{
		{Pkg: a, Name: "ErrA"},
		{Pkg: b, Name: "ErrB"},
	}
	got := ErrCodeConflicts(owners)
	if len(got) != 1 {
		t.Fatalf("同前缀未报冲突：%+v（a=%s b=%s prefix=%#x）", got, a, b, ErrCodePrefix(a))
	}
	// 同前缀但**同一条**枚举（重复输入）不算冲突。
	if again := ErrCodeConflicts([]ErrCodeOwner{owners[0], owners[0]}); len(again) != 0 {
		t.Fatalf("同一条枚举被当成冲突：%+v", again)
	}
	// 不同前缀不报。
	if none := ErrCodeConflicts([]ErrCodeOwner{{Pkg: "app/core", Name: "E1"}, {Pkg: "app/ui", Name: "E2"}}); len(none) != 0 {
		t.Fatalf("不同前缀被误报：%+v", none)
	}
}

// TestErrCodeAssignedByChecker：端到端 —— 检查 `enum HttpErr: ErrCode { … }`
// 之后 Codes 必须按（包身份哈希, 序号）就位，且 CodeOf 可按变体名取值。
func TestErrCodeAssignedByChecker(t *testing.T) {
	src := `enum HttpErr: ErrCode { Timeout, Connect, Tls }
`
	f, lexErrs, parseErrs := parse.Source("testdata/ok/t.aic", src)
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		t.Fatalf("语料未解析干净：lex=%v parse=%v", lexErrs, parseErrs)
	}
	res := CheckFile(f)
	if len(res.Errors) > 0 {
		t.Fatalf("检查失败：%v", res.Errors)
	}
	en := res.Info.Enums["HttpErr"]
	if en == nil || !en.ErrCode {
		t.Fatalf("HttpErr 未登记为 ErrCode 枚举：%+v", en)
	}
	if len(en.Codes) != 3 {
		t.Fatalf("码数不对：%v", en.Codes)
	}
	for i, want := range []int32{
		ErrCodeValue(en.Pkg, 0), ErrCodeValue(en.Pkg, 1), ErrCodeValue(en.Pkg, 2),
	} {
		if en.Codes[i] != want {
			t.Fatalf("第 %d 个码 = %d，期望 %d", i, en.Codes[i], want)
		}
	}
	if c, ok := en.CodeOf("Tls"); !ok || c != en.Codes[2] {
		t.Fatalf("CodeOf(Tls) = %d,%v", c, ok)
	}
	if _, ok := en.CodeOf("Nope"); ok {
		t.Fatal("未知变体不该有码")
	}
}
