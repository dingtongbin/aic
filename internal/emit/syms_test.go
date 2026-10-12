package emit

import (
	"strings"
	"testing"

	"aic/internal/parse"
	"aic/internal/types"
)

// TestSymTableIsReversible 钉住「哈希符号可回查」：内部符号是 sha256 前 8 字节，
// 不可逆 —— 崩溃现场只看到 `aic_9f3a...`。`Result.Syms` 就是那张对照表，
// 断言它：非空、按 AIR 名有序（H2）、AIR 名唯一、C 名形态合法（aic_+16 hex）。
func TestSymTableIsReversible(t *testing.T) {
	src := `import fmt
import str

class Box {
    var n i32
}

func area(b Box) -> i32 {
    return b.n
}

enum Color { Red, Green }

func main() {
    var b = Box{n: 3}
    fmt.println(str.fromI64(i64(area(b))))
}
`
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
	if len(out.Syms) == 0 {
		t.Fatal("符号对照表为空 —— 哈希符号将无法回查（aic dump syms 没有数据源）")
	}
	seen := map[string]bool{}
	prev := ""
	for _, e := range out.Syms {
		if e.AIR == "" || e.C == "" {
			t.Fatalf("对照项不完整：%+v", e)
		}
		if seen[e.AIR] {
			t.Fatalf("AIR 名重复：%s（两张表/两个来源没有汇合）", e.AIR)
		}
		seen[e.AIR] = true
		if prev != "" && e.AIR < prev {
			t.Fatalf("对照表未按 AIR 名排序：%s 在 %s 之后", e.AIR, prev)
		}
		prev = e.AIR
		if !strings.HasPrefix(e.C, "aic_") || len(e.C) != len("aic_")+16 {
			t.Fatalf("内部符号形态应为 aic_<16 hex>：%s（AIR %s，kind %s）", e.C, e.AIR, e.Kind)
		}
	}
	// 必须既有函数也有类型：只有一半 = 枚举漏了。
	var hasFunc, hasType bool
	for _, e := range out.Syms {
		if e.Kind == "func" || e.Kind == "method" {
			hasFunc = true
		}
		if e.Kind == "type" || e.Kind == "enum" || e.Kind == "iface" {
			hasType = true
		}
	}
	if !hasFunc || !hasType {
		t.Fatalf("对照表缺项：func=%v type=%v（共 %d 条）", hasFunc, hasType, len(out.Syms))
	}
}
