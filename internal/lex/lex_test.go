package lex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- helpers (黄金执行规划 §15.2) -----------------------------------------------

type wantTok struct {
	Kind Kind
	Text string
}

func lexAll(t *testing.T, src string) []Token {
	t.Helper()
	toks, errs := File("t.aic", src)
	if len(errs) != 0 {
		for _, e := range errs {
			t.Errorf("期望干净扫描，得到错误：\n%s", e.Format())
		}
		t.FailNow()
	}
	return toks
}

func expectSeq(t *testing.T, toks []Token, want []wantTok) {
	t.Helper()
	if len(toks) != len(want) {
		t.Fatalf("token 数量 = %d，期望 %d；实际序列 = %s", len(toks), len(want), seq(toks))
	}
	for i, w := range want {
		if toks[i].Kind != w.Kind || toks[i].Text != w.Text {
			t.Fatalf("token[%d] = {%s %q} @ %s，期望 {%s %q}", i, toks[i].Kind, toks[i].Text, toks[i].Pos, w.Kind, w.Text)
		}
	}
}

func seq(toks []Token) string {
	var parts []string
	for _, tk := range toks {
		parts = append(parts, tk.String())
	}
	return strings.Join(parts, " ")
}

// normalizeNL 消除尾随换行差异：快照文件以 LF 收尾，报错串不带。
func normalizeNL(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

func descs(errs []LexerError) string {
	var parts []string
	for _, e := range errs {
		parts = append(parts, e.Desc)
	}
	return strings.Join(parts, " | ")
}

// assertThreePart verifies the frozen H5 shape: three lines, the second
// indented by four spaces, the third starting with "    fix: ".
func assertThreePart(t *testing.T, out string) {
	t.Helper()
	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("三段式应为 3 行，实际 %d 行：\n%s", len(lines), out)
	}
	if !strings.Contains(lines[0], ": error: ") || !strings.Contains(lines[0], ":") {
		t.Fatalf("第一段须为 `<file>:<line>:<col>: error:<描述>`：\n%s", out)
	}
	if !strings.HasPrefix(lines[1], "    ") || strings.HasPrefix(lines[1], "     ") {
		t.Fatalf("第二段（上下文）须缩进 4 空格：\n%s", out)
	}
	if !strings.HasPrefix(lines[2], "    fix: ") {
		t.Fatalf("第三段须以 `    fix: ` 开头：\n%s", out)
	}
	if strings.TrimSpace(strings.TrimPrefix(lines[2], "    fix: ")) == "" {
		t.Fatalf("修复段不得为空（三段缺一不可）：\n%s", out)
	}
}

// --- 用例 1–3：锚点与位置 --------------------------------------------------------

func TestHelloAnchor(t *testing.T) {
	src := "import print\n\nfunc main() {\n    print.println(\"hello, aic\")\n}\n"
	expectSeq(t, lexAll(t, src), []wantTok{
		{KW, "import"}, {IDENT, "print"},
		{KW, "func"}, {IDENT, "main"}, {PUNCT, "("}, {PUNCT, ")"}, {PUNCT, "{"},
		{IDENT, "print"}, {PUNCT, "."}, {IDENT, "println"}, {PUNCT, "("},
		{STR, "\"hello, aic\""}, {PUNCT, ")"}, {PUNCT, "}"}, {EOF, ""},
	})
}

func TestPositionsAcrossLines(t *testing.T) {
	src := "import a\n\nfunc main() {\n    a.println(\"x\")\n}\n"
	toks := lexAll(t, src)
	want := []wantTok{
		{KW, "import"}, {IDENT, "a"},
		{KW, "func"}, {IDENT, "main"}, {PUNCT, "("}, {PUNCT, ")"}, {PUNCT, "{"},
		{IDENT, "a"}, {PUNCT, "."}, {IDENT, "println"}, {PUNCT, "("}, {STR, "\"x\""},
		{PUNCT, ")"}, {PUNCT, "}"}, {EOF, ""},
	}
	expectSeq(t, toks, want)
	if toks[2].Pos.Line != 3 || toks[2].Pos.Col != 1 {
		t.Errorf("func 应在 3:1，实际 %s", toks[2].Pos)
	}
	if toks[8].Pos.Line != 4 || toks[8].Pos.Col != 6 {
		t.Errorf("'.' 应在 4:6，实际 %s", toks[8].Pos)
	}
	if toks[11].Pos.Line != 4 || toks[11].Pos.Col != 15 {
		t.Errorf("字符串应在 4:15，实际 %s", toks[11].Pos)
	}
}

func TestCommentOnlyFileYieldsEOF(t *testing.T) {
	expectSeq(t, lexAll(t, "// only a comment\n"), []wantTok{{EOF, ""}})
	expectSeq(t, lexAll(t, "/// doc only\n/* block */\n"), []wantTok{{EOF, ""}})
}

// --- 用例 4–6：标识符 ---------------------------------------------------------------

// v2（核心设计 §一，2026-10 收紧）：标识符字符集是 **ASCII** ——
// 首字符 ASCII 字母或 `_`，后续 ASCII 字母/数字/单个 `_`。
// Unicode 字母**不是**标识符字符（否则「首字符大写 = 跨包可见」在 Unicode 上无定义）。
func TestASCIIIdentifiersOnly(t *testing.T) {
	expectSeq(t, lexAll(t, "Name1 x_ _private aZ9"), []wantTok{
		{IDENT, "Name1"}, {IDENT, "x_"}, {IDENT, "_private"}, {IDENT, "aZ9"}, {EOF, ""},
	})
	// 中文/带音符字母出现在标识符位 = 词法错（而不是"合法标识符"）。
	for _, src := range []string{"变量名1", "中_国", "aéZ", "var 变量 = 1"} {
		_, errs := File("t.aic", src)
		if len(errs) == 0 {
			t.Errorf("Unicode 标识符必须被拒绝：%q", src)
			continue
		}
		assertThreePart(t, errs[0].Format())
	}
}

func TestDoubleUnderscoreRejected(t *testing.T) {
	_, errs := File("t.aic", "var a__b = 1")
	if len(errs) != 1 {
		t.Fatalf("连续下划线必须恰好一条错误，实际 %d 条：%s", len(errs), descs(errs))
	}
	if !strings.Contains(errs[0].Desc, "double underscore") {
		t.Errorf("描述须点名连续下划线：%q", errs[0].Desc)
	}
	if !strings.Contains(errs[0].Fix, "a single _") {
		t.Errorf("修复建议须给出可执行动作：%q", errs[0].Fix)
	}
	assertThreePart(t, errs[0].Format())
}

// v2（核心设计 §一）：`_` 前缀 = 包私有标识符，词法合法；裸 `_` = 弃位 token。
// `__` 相邻仍然拒绝（保留给 mangling），见 TestDoubleUnderscoreRejected。
func TestLeadingUnderscorePrivateIdentifier(t *testing.T) {
	expectSeq(t, lexAll(t, "var _x = 1"), []wantTok{
		{KW, "var"}, {IDENT, "_x"}, {PUNCT, "="}, {INT, "1"}, {EOF, ""},
	})
	expectSeq(t, lexAll(t, "_ _count"), []wantTok{
		{PUNCT, "_"}, {IDENT, "_count"}, {EOF, ""},
	})
}

// --- 用例 7–10：数字 -------------------------------------------------------------------

func TestIntegerForms(t *testing.T) {
	expectSeq(t, lexAll(t, "0 42 0xFF 0xff 0b101 0b1_0 1_000_000"), []wantTok{
		{INT, "0"}, {INT, "42"}, {INT, "0xFF"}, {INT, "0xff"},
		{INT, "0b101"}, {INT, "0b1_0"}, {INT, "1_000_000"}, {EOF, ""},
	})
}

func TestFloatForms(t *testing.T) {
	expectSeq(t, lexAll(t, "0.5 3.14 1.5e-3 2.0E2"), []wantTok{
		{FLOAT, "0.5"}, {FLOAT, "3.14"}, {FLOAT, "1.5e-3"}, {FLOAT, "2.0E2"}, {EOF, ""},
	})
}

func TestNumberSeparatorViolations(t *testing.T) {
	for _, src := range []string{"0x_", "1__2", "1_", "1__"} {
		_, errs := File("t.aic", src)
		if len(errs) != 1 {
			t.Errorf("%q 期望恰好 1 错，实际 %d：%s", src, len(errs), descs(errs))
			continue
		}
		if !strings.Contains(errs[0].Desc, "the digit separator _ must sit between digits") {
			t.Errorf("%q 描述不符：%q", src, errs[0].Desc)
		}
		if errs[0].Fix == "" {
			t.Errorf("%q 修复建议缺失", src)
		}
	}
}

func TestTrailingDotRejected(t *testing.T) {
	_, errs := File("t.aic", "var a = 12.")
	if len(errs) != 1 || !strings.Contains(errs[0].Desc, "bare '.'") {
		t.Fatalf("`12.` 须报『bare dot '.'』：%s", descs(errs))
	}
	assertThreePart(t, errs[0].Format())
}

func TestBareExponentRejected(t *testing.T) {
	_, errs := File("t.aic", "var a = 1e5")
	if len(errs) != 1 || !strings.Contains(errs[0].Desc, "exponent requires a decimal point") {
		t.Fatalf("`1e5` 须报『指数必须带小数点』：%s", descs(errs))
	}
}

func TestLeadingDotFloatRejected(t *testing.T) {
	_, errs := File("t.aic", "var a = .5")
	if len(errs) != 1 || !strings.Contains(errs[0].Desc, "may not start with .") {
		t.Fatalf("`.5` 须报『may not start with .』：%s", descs(errs))
	}
}

// --- 用例 11–13：字符串 ------------------------------------------------------------------

func TestStringEscapesAccepted(t *testing.T) {
	expectSeq(t, lexAll(t, `"a\nb\tc\rd\\q"`), []wantTok{{STR, `"a\nb\tc\rd\\q"`}, {EOF, ""}})
	expectSeq(t, lexAll(t, `"quote: \" inner"`), []wantTok{{STR, `"quote: \" inner"`}, {EOF, ""}})
}

func TestInvalidEscapeRejected(t *testing.T) {
	_, errs := File("t.aic", `var s = "bad\x01"`)
	if len(errs) != 1 {
		t.Fatalf("无效转义须恰好 1 错（须重同步避免连发），实际 %d：%s", len(errs), descs(errs))
	}
	if !strings.Contains(errs[0].Desc, "invalid escape") {
		t.Errorf("描述不符：%q", errs[0].Desc)
	}
	for _, esc := range []string{"\\n", "\\t", "\\r", "\\\\"} {
		if !strings.Contains(errs[0].Fix, esc) {
			t.Errorf("修复建议须枚举合法转义 %s：%q", esc, errs[0].Fix)
		}
	}
}

func TestMultilineStringRejected(t *testing.T) {
	_, errs := File("t.aic", "var s = \"raw\nvar t = 1\n")
	if len(errs) != 1 || !strings.Contains(errs[0].Desc, "not closed on the same line") {
		t.Fatalf("跨行字符串须报且仅报『未在同一行闭合』：%s", descs(errs))
	}
}

func TestUnterminatedStringAtEOF(t *testing.T) {
	_, errs := File("t.aic", "var s = \"raw name")
	if len(errs) != 1 || !strings.Contains(errs[0].Desc, "unterminated") {
		t.Fatalf("EOF 前未闭合须报错：%s", descs(errs))
	}
}

// --- 用例 14–15：注释 -----------------------------------------------------------------------

func TestBlockCommentNonNesting(t *testing.T) {
	src := "/* outer /* inner-looking marker */ var x\n"
	expectSeq(t, lexAll(t, src), []wantTok{{KW, "var"}, {IDENT, "x"}, {EOF, ""}})
}

func TestUnterminatedBlockComment(t *testing.T) {
	_, errs := File("t.aic", "/* open\nvar x = 1\n")
	if len(errs) != 1 || !strings.Contains(errs[0].Desc, "unterminated block comment") {
		t.Fatalf("块注释未闭合须报错：%s", descs(errs))
	}
	if !strings.Contains(errs[0].Fix, "*/") {
		t.Errorf("修复建议须指向补 */：%q", errs[0].Fix)
	}
}

// --- 用例 16：注解 -------------------------------------------------------------------------

func TestAllFrozenAnnotations(t *testing.T) {
	var sb strings.Builder
	for i, a := range AnnotationNames {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteString("@" + a)
	}
	toks := lexAll(t, sb.String())
	for i, a := range AnnotationNames {
		if toks[i].Kind != ANNOT || toks[i].Text != "@"+a {
			t.Errorf("注解 token[%d] = %v，期望 @%s", i, toks[i], a)
		}
	}
	if toks[len(AnnotationNames)].Kind != EOF {
		t.Errorf("注解流须以 EOF 收尾")
	}
}

func TestUnknownAnnotationRejected(t *testing.T) {
	_, errs := File("t.aic", "@inline\nclass X { }\n")
	if len(errs) != 1 || !strings.Contains(errs[0].Desc, "unknown annotation @inline") {
		t.Fatalf("未知注解须报错：%s", descs(errs))
	}
	if !strings.Contains(errs[0].Fix, "@packed") {
		t.Errorf("修复建议须列出合法注解集：%q", errs[0].Fix)
	}
}

// --- 用例 17：多错收集（红线#16） ----------------------------------------------------------------

func TestMultipleErrorsCollectedInOrder(t *testing.T) {
	src := "var a__b = 1\n`tick`\n0x_\n\"unterminated\n"
	_, errs := File("t.aic", src)
	if len(errs) < 4 {
		t.Fatalf("多错语料期望 ≥4 条错误，实际 %d：%s", len(errs), descs(errs))
	}
	prevLine, prevCol := 0, 0
	for _, e := range errs {
		if e.Pos.Line < prevLine || (e.Pos.Line == prevLine && e.Pos.Col < prevCol) {
			t.Fatalf("错误位置顺序倒退：%v", errs)
		}
		prevLine, prevCol = e.Pos.Line, e.Pos.Col
	}
	for _, e := range errs {
		assertThreePart(t, e.Format())
	}
}

// --- 用例 18：运算符（最长匹配）--------------------------------------------------------------------

func TestOperatorsLongestMatch(t *testing.T) {
	src := "== <= >= != << >> -> => .. && || += -= *= /= %= = < > ! & | ^ ~ ( ) { } [ ] , ; . : _ + - * / %"
	var got []string
	for _, tk := range lexAll(t, src) {
		got = append(got, tk.Text)
	}
	want := []string{"==", "<=", ">=", "!=", "<<", ">>", "->", "=>", "..", "&&", "||",
		"+=", "-=", "*=", "/=", "%=", "=", "<", ">", "!", "&", "|", "^", "~",
		"(", ")", "{", "}", "[", "]", ",", ";", ".", ":", "_", "+", "-", "*", "/", "%", ""}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("运算符序列不符\ngot  = %s\nwant = %s", strings.Join(got, " "), strings.Join(want, " "))
	}
}

// --- 用例 19：golden 往返（testdata/err 快照）------------------------------------------------------

func testdataRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Skipf("无法获取工作目录: %v", err)
	}
	root := filepath.Join(wd, "..", "..", "testdata")
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		t.Skipf("testdata 不可定位: %s (%v)", root, err)
	}
	return root
}

// err/ 快照的唯一所有者 = internal/golden（全管线视角，含 ILL 级联的 parse
// 报错）；词法层由 TestFrozenSurface 与上方单测覆盖，不在此重复比对。
