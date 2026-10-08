package types

import (
	"testing"

	"aic/internal/parse"
)

// checkSrc 走一遍全管线，返回诊断文本列表。
func checkSrc(t *testing.T, src string) []string {
	t.Helper()
	f, lexErrs, parseErrs := parse.Source("t.aic", src)
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		t.Fatalf("语料本身不干净：lex=%d parse=%d", len(lexErrs), len(parseErrs))
	}
	res := CheckFile(f)
	out := make([]string, 0, len(res.Errors))
	for _, e := range res.Errors {
		out = append(out, e.Format())
	}
	return out
}

func assertClean(t *testing.T, name, src string) {
	t.Helper()
	if errs := checkSrc(t, src); len(errs) > 0 {
		t.Errorf("%s：应编译干净，实际报 %d 条：\n%s", name, len(errs), errs[0])
	}
}

// TestMultiBindResultIsUnproven: 多返回绑定的接收位来自调用 = 「未证」，
// 不是「必然 nil」（核心设计 §九）。把它当成必 nil 会让合法程序在解引用位
// 被判编译错——nil 教条只允许「可证必然为 nil」时报错，误报即教条失效。
func TestMultiBindResultIsUnproven(t *testing.T) {
	assertClean(t, "多返回绑定后解引用", `class Box {
    var n i32
}

func make() -> (Box, Err) {
    return Box{n: 7}, nil
}

func main() {
    var b, err = make()
    if err != nil {
        return
    }
    var x i32 = b.n
    var y i32 = x + 1
    y = y - 1
}
`)
}

// TestSingleTargetWithoutInitIsMustNil: 单目标无初值（var p C）必然 nil，
// 由 declareVar 保持 mustNil（§九 的第一条），不能随多返回绑定一起放松。
func TestSingleTargetWithoutInitIsMustNil(t *testing.T) {
	src := `class Box {
    var n i32
}

func main() {
    var b Box
    if b == nil {
        return
    }
}
`
	if errs := checkSrc(t, src); len(errs) > 0 {
		t.Errorf("与 nil 比较应当合法，实际报错：\n%s", errs[0])
	}
}

// TestCallResultDerefIsNotCompileError: 调用产物 = 未证（§九），解引用位
// 只「运行时守」，不得因未证而报编译错（宁保守插检查，禁错误消除的反面：
// 不得把未证当必错）。
func TestCallResultDerefIsNotCompileError(t *testing.T) {
	assertClean(t, "调用产物解引用", `class Box {
    var n i32
}

func make() -> Box {
    return Box{n: 7}
}

func main() {
    var b = make()
    var x i32 = b.n
    var y i32 = x + 1
    y = y - 1
}
`)
}
