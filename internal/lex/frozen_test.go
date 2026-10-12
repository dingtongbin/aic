package lex

import (
	"testing"
)

// TestFrozenSurface is the H7 gate (《AIC 语言规范》§1.3/§1.5): the literal
// surface tables must match the frozen lists exactly — no additions, no
// removals.
// v7 = 36 keywords + 0 contextual words + 0 annotations.
func TestFrozenSurface(t *testing.T) {
	t.Run("keywords", func(t *testing.T) {
		want := []string{
			"class", "interface", "enum", "struct", "var", "const", "live", "func",
			"if", "else", "for", "in",
			"return", "break", "continue",
			"match", "defer", "errdefer", "check", "catch",
			"spawn", "scope", "region", "this", "into",
			"select", "case", "use", "implements", "include", "derive",
			"comptime", "assert",
			"import", "export", "extern",
		}
		if len(want) != 36 {
			t.Fatalf("冻结清单自身出错：期望 36 个关键字，清单含 %d 个", len(want))
		}
		if len(Keywords) != 36 {
			t.Fatalf("Keywords 表大小 = %d，期望 36（H7：表面不许增减）", len(Keywords))
		}
		if len(KeywordNames) != len(want) {
			t.Fatalf("KeywordNames 长度 = %d，期望 %d", len(KeywordNames), len(want))
		}
		for i, k := range want {
			if KeywordNames[i] != k {
				t.Errorf("KeywordNames[%d] = %q，期望 %q（字面量表逐字一致）", i, KeywordNames[i], k)
			}
			if !Keywords[k] {
				t.Errorf("Keywords 缺少冻结关键字 %q", k)
			}
		}
		for k := range Keywords { // 名单 → 表 的反向覆盖
			found := false
			for _, w := range want {
				if w == k {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Keywords 含冻结表外条目 %q（H7 违规）", k)
			}
		}
	})

	t.Run("plain identifiers", func(t *testing.T) {
		// 《AIC 语言规范》§1.3：while/elseif/handle/await/del/mut 是**普通标识符**，
		// 不在关键字表里；词法一律以 IDENT 流出（解析层不为它们定制任何诊断）。
		words := []string{"while", "elseif", "handle", "await", "del", "mut"}
		for _, w := range words {
			if Keywords[w] {
				t.Errorf("%q 不得出现在关键字表（H7）", w)
			}
		}
		toks, errs := File("t.aic", "while elseif handle await del mut")
		if len(errs) != 0 {
			t.Fatalf("这些拼写词法必须干净（作为标识符流出）: %v", errs)
		}
		for i := range words {
			if toks[i].Kind != IDENT || toks[i].Text != words[i] {
				t.Errorf("token[%d] = %s %q，期望 IDENT %q", i, toks[i].Kind, toks[i].Text, words[i])
			}
		}
		// errdefer/select/case/use/comptime/assert/catch/into/include/derive
		// 全部从上下文词升为**硬关键字**（上下文关键字归零，P4 要求零猜测）。
		// 它们必须词法为 KW，不得作标识符。
		for _, k := range []string{"errdefer", "select", "case", "use", "comptime",
			"assert", "catch", "into", "include", "derive"} {
			if !Keywords[k] {
				t.Errorf("%q 必须是硬关键字（v7 上下文词归零）", k)
			}
			ktoks, kerrs := File("t.aic", k)
			if len(kerrs) != 0 || ktoks[0].Kind != KW {
				t.Errorf("%q 应词法为 KW，实际 %v %v", k, ktoks[0], kerrs)
			}
		}
	})

	t.Run("reserved", func(t *testing.T) {
		want := []string{"true", "false", "nil"}
		if len(ReservedNames) != len(want) {
			t.Fatalf("保留字数量 = %d，期望 %d", len(ReservedNames), len(want))
		}
		for i, r := range want {
			if ReservedNames[i] != r {
				t.Errorf("ReservedNames[%d] = %q，期望 %q", i, ReservedNames[i], r)
			}
			if Keywords[r] {
				t.Errorf("保留字 %q 不得同时列为关键字", r)
			}
		}
	})

	t.Run("no-annotations", func(t *testing.T) {
		// 语言没有注解机制：`@` 不是词法可接受字符，出现即恰好一个词法错误。
		// 这里不拿任何具体注解当语料 —— 注解形态从来不在语言里，不存在
		// "某注解拼写被废弃"这回事：值类型是 struct、能力是 derive、存储层级是
		// live、C ABI 是 extern，全是硬关键字。
		for _, src := range []string{"@annot x", "@x = 1", "var @y i32"} {
			if _, errs := File("t.aic", src); len(errs) != 1 {
				t.Fatalf("%q 应恰好一个词法错误（@ 非法），实际 %d 个", src, len(errs))
			}
		}
	})

	t.Run("no-question-operator", func(t *testing.T) {
		// `?` 不是运算符：唯一传播形态是 check（《AIC 语言规范》§5.6）。
		_, errs := File("t.aic", "var v = f()?")
		if len(errs) == 0 {
			t.Fatalf("`?` 应报词法错误（`?` 不在运算符表）")
		}
	})

	t.Run("basic-type-names", func(t *testing.T) {
		// 《AIC 语言规范》§2.1：15 个基本类型名禁作用户标识符（C2）。
		if len(BasicTypeNames) != 15 {
			t.Fatalf("基本类型名数量 = %d，期望 15", len(BasicTypeNames))
		}
		for _, b := range BasicTypeNames {
			if b == "" {
				t.Errorf("基本类型名清单含空条目")
			}
			if Keywords[b] {
				t.Errorf("基本类型名 %q 不得同时列为关键字", b)
			}
		}
	})

	t.Run("live-is-a-keyword", func(t *testing.T) {
		// `live` 是变量存储层级修饰关键字（第 7 个），是标识符位上的硬关键字，
		// 与 `var`/`func` 同类。
		toks, errs := File("t.aic", "var live x")
		if len(errs) != 0 {
			t.Fatalf("`var live x` 应词法干净: %v", errs)
		}
		wantKinds := []Kind{KW, KW, IDENT, EOF}
		for i, k := range wantKinds {
			if toks[i].Kind != k {
				t.Errorf("token[%d] kind = %s，期望 %s", i, toks[i].Kind, k)
			}
		}
	})

	t.Run("reserved-not-keyword-behavior", func(t *testing.T) {
		// nil/true/false must lex as RES, never as IDENT (
		// nil = 任意类型零值字面量，词法层必须先认出它是保留字).
		toks, errs := File("t.aic", "nil true false")
		if len(errs) != 0 {
			t.Fatalf("保留字应词法干净: %v", errs)
		}
		wantKinds := []Kind{RES, RES, RES, EOF}
		for i, k := range wantKinds {
			if toks[i].Kind != k {
				t.Errorf("token[%d] kind = %s，期望 %s", i, toks[i].Kind, k)
			}
		}
	})

	t.Run("blank-and-private-underscore", func(t *testing.T) {		// `_` 单独 = 弃位 token；`_name` = 包私有标识符；
		// `__` 相邻拒绝（保留给 mangling）；`0..n` 的 `..` 不得吞进数字。
		toks, errs := File("t.aic", "_ _x x_ a__ 0..n 1_000")
		if len(errs) != 1 {
			t.Fatalf("`__` 应产生唯一词法错误，实际 %d 个: %v", len(errs), errs)
		}
		want := []struct {
			kind Kind
			text string
		}{
			{PUNCT, "_"}, {IDENT, "_x"}, {IDENT, "x_"}, {INT, "0"}, {PUNCT, ".."}, {IDENT, "n"}, {INT, "1_000"},
		}
		// tokens: _ _x x_ a(ILL) 0 .. n 1_000 — the ILL token replaces `a__b`'s tail
		if toks[0].Kind != PUNCT || toks[0].Text != "_" {
			t.Errorf("裸 _ 应为 PUNCT，实际 %s %q", toks[0].Kind, toks[0].Text)
		}
		if toks[1].Kind != IDENT || toks[1].Text != "_x" {
			t.Errorf("_x 应为私有 IDENT，实际 %s %q", toks[1].Kind, toks[1].Text)
		}
		if toks[2].Kind != IDENT || toks[2].Text != "x_" {
			t.Errorf("x_ 应为 IDENT，实际 %s %q", toks[2].Kind, toks[2].Text)
		}
		for i, w := range want[3:] {
			got := toks[4+i] // token 3 is the ILL from `a__b`
			if got.Kind != w.kind || got.Text != w.text {
				t.Errorf("token[%d] = %s %q，期望 %s %q", 4+i, got.Kind, got.Text, w.kind, w.text)
			}
		}
	})

	// H7 缺口补齐（L4-h）：冻结面不止关键字与注解 —— **运算符表**与**类型构造子**
	// 同样是"使用者会当成语法"的表面，H7 必须四张表一起钉 —— 只钉前两张表，`?`/`or`/`catch`/后缀 `!`
	// 四个 E1–E4 运算符与 `bytes`/`chan[T]`/`map[K]V`/`set[T]`/`[T;N]`/`func(T)->R` 六个
	// 类型构造子都没有断言（文档与实现脱节时门禁看不出来）。
	t.Run("operators", func(t *testing.T) {
		// 冻结运算符表（《AIC 语言规范》§1.5）——逐条必须能词法为 PUNCT。
		// v7 删除：`?`（与 check 同语义）、`::`、`...`。
		// v7 未采用：复合位运算赋值（`&=` 等）—— 写 `x = x & m` 零歧义。
		ops := []string{
			"+", "-", "*", "/", "%",
			"==", "!=", "<", "<=", ">", ">=",
			"&&", "||", "!",
			"&", "|", "^", "<<", ">>",
			"=", "+=", "-=", "*=", "/=", "%=",
			".", ",", ";", ":", "->", "=>", "..", "[", "]", "(", ")", "{", "}",
		}
		for _, op := range ops {
			toks, errs := File("t.aic", op)
			if len(errs) != 0 {
				t.Errorf("冻结运算符 %q 词法报错: %v", op, errs)
				continue
			}
			if toks[0].Kind != PUNCT {
				t.Errorf("冻结运算符 %q 应词法为 PUNCT，实际 %s", op, toks[0].Kind)
			}
		}
		// 未采用的拼写：不得成为**单一** token（被切成两个 token 后由解析层
		// 报错，同样是拒绝；这里钉住的是"词法层不认这个拼写"）。
		for _, bad := range []string{"?", "::", "...", "&=", "|=", "^=", "<<=", ">>="} {
			toks, errs := File("t.aic", bad)
			if len(errs) != 0 {
				continue // 词法直接报错，也是拒绝
			}
			if len(toks) > 0 && toks[0].Kind == PUNCT && toks[0].Text == bad {
				t.Errorf("未采用的拼写 %q 不应成为单一 token", bad)
			}
		}
		// 上下文关键字 0 个。catch/errdefer/comptime 等都是硬关键字（KW）。
		for _, k := range []string{"catch", "errdefer", "comptime"} {
			toks, errs := File("t.aic", k)
			if len(errs) != 0 || toks[0].Kind != KW {
				t.Errorf("%q 必须是硬关键字（上下文词归零），实际 %v %v", k, toks[0], errs)
			}
		}
	})

	t.Run("type-constructors", func(t *testing.T) {
		// 类型构造子：预声明类型名 + 容器/通道/数组/函数类型的**前缀与后缀**写法。
		// 判据 = 词法干净（名必须是 IDENT 或关键字位）；语义解析由 parse/types 负责。
		// `bytes` 不是类型名（u8 才是）；`rune` 在册。
		idents := []string{"f32", "f64", "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize", "bool", "str", "rune", "map", "set", "chan"}
		for _, id := range idents {
			toks, errs := File("t.aic", id)
			if len(errs) != 0 {
				t.Errorf("类型构造子 %q 词法报错: %v", id, errs)
				continue
			}
			if toks[0].Kind != IDENT {
				t.Errorf("类型构造子 %q 应为 IDENT（上下文关键字/预声明名），实际 %s", id, toks[0].Kind)
			}
		}
		// 写法形态必须词法干净：T[]、[T;N]、map[K]V、set[T]、chan[T]、(T) -> R
		for _, src := range []string{"i32[]", "[i32; 4]", "map[str]i32", "set[i32]", "chan[i32]", "(i32) -> i32"} {
			if _, errs := File("t.aic", src); len(errs) != 0 {
				t.Errorf("类型写法 %q 词法报错: %v", src, errs)
			}
		}
	})
}
