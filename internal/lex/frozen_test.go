package lex

import (
	"strings"
	"testing"
)

// TestFrozenSurface is the H7 gate (核心设计 §一/§十三): the literal surface
// tables must match the frozen lists exactly — no additions, no removals.
// v2 = 24 keywords + 4 annotations (R0 换基 + R20：@live 退役为 live 关键字;
// v1 的 29+8 全部随附录 A 退役).
func TestFrozenSurface(t *testing.T) {
	t.Run("keywords", func(t *testing.T) {
		want := []string{
			"class", "interface", "enum", "var", "const", "live", "func",
			"if", "else", "for", "in",
			"return", "break", "continue",
			"match", "defer", "check",
			"spawn", "scope", "region", "this",
			"import", "export", "extern",
		}
		if len(want) != 24 {
			t.Fatalf("冻结清单自身出错：期望 24 个关键字，清单含 %d 个", len(want))
		}
		if len(Keywords) != 24 {
			t.Fatalf("Keywords 表大小 = %d，期望 24（H7：表面不许增减）", len(Keywords))
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

	t.Run("retired", func(t *testing.T) {
		// 防坑清单 1: while/elseif/implements/handle/await/del/mut
		// 均非关键字——出现了就是事故。它们必须以 IDENT 身份流出，由解析层
		// 给出定向的退役报错。
		// 注意：`errdefer` 已由 N8 收回（核心设计 §16），以**上下文关键字**身份
		// 在语句位识别，故不在此列（H7：关键字与注解表不变）。
		for _, r := range RetiredNames {
			if Keywords[r] {
				t.Errorf("退役词 %q 不得出现在关键字表（H7）", r)
			}
		}
		if len(RetiredNames) != 7 {
			t.Fatalf("退役名单数量 = %d，期望 7", len(RetiredNames))
		}
		toks, errs := File("t.aic", "while elseif implements handle await del mut")
		if len(errs) != 0 {
			t.Fatalf("退役词应词法干净（作为标识符流出）: %v", errs)
		}
		for i := range RetiredNames {
			if toks[i].Kind != IDENT || toks[i].Text != RetiredNames[i] {
				t.Errorf("token[%d] = %s %q，期望 IDENT %q", i, toks[i].Kind, toks[i].Text, RetiredNames[i])
			}
		}
		// errdefer 同样是 IDENT（不是关键字），但属于**合法**表面。
		if Keywords["errdefer"] {
			t.Errorf("errdefer 不得成为第 24 个关键字（H7）")
		}
		etoks, eerrs := File("t.aic", "errdefer")
		if len(eerrs) != 0 || etoks[0].Kind != IDENT {
			t.Errorf("errdefer 必须是 IDENT（上下文关键字），实际 %v %v", etoks[0], eerrs)
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

	t.Run("annotations", func(t *testing.T) {
		want := []string{"packed", "derive", "noblock", "blocking"}
		if len(want) != 4 {
			t.Fatalf("冻结清单自身出错：期望 4 个注解，清单含 %d 个", len(want))
		}
		if len(Annotations) != len(want) {
			t.Fatalf("Annotations 表大小 = %d，期望 %d（H7 = 4 注解）", len(Annotations), len(want))
		}
		if len(AnnotationNames) != len(want) {
			t.Fatalf("AnnotationNames 长度 = %d，期望 %d", len(AnnotationNames), len(want))
		}
		for i, a := range want {
			if AnnotationNames[i] != a {
				t.Errorf("AnnotationNames[%d] = %q，期望 %q", i, AnnotationNames[i], a)
			}
			if !Annotations[a] {
				t.Errorf("Annotations 缺少冻结注解 %q", a)
			}
		}
		for a := range Annotations {
			found := false
			for _, w := range want {
				if w == a {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Annotations 含冻结名单外条目 %q（H7 违规）", a)
			}
		}
	})

	t.Run("live-is-a-keyword", func(t *testing.T) {
		// R20：`live` 是第 24 个关键字（变量存储层级修饰），不是注解、不是标识符。
		// 旧形态 @live 必须有迁移提示（不是裸"未知注解"）。
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
		if toks[0].Text != "var" || toks[1].Text != "live" {
			t.Errorf("token 文本 = %q %q，期望 \"var\" \"live\"", toks[0].Text, toks[1].Text)
		}
		if _, errs := File("t.aic", "@live x"); len(errs) != 1 {
			t.Fatalf("旧 @live 应恰好一个词法错误，实际 %d 个: %v", len(errs), errs)
		} else if !strings.Contains(errs[0].Fix, "live is a keyword now") {
			t.Errorf("旧 @live 的错误缺迁移提示：%q", errs[0].Fix)
		}
	})

	t.Run("reserved-not-keyword-behavior", func(t *testing.T) {
		// nil/true/false must lex as RES, never as IDENT (核心设计 §一:
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

	t.Run("blank-and-private-underscore", func(t *testing.T) {		// 核心设计 §一: `_` 单独 = 弃位 token；`_name` = 包私有标识符；
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
	// 同样是"使用者会当成语法"的表面。此前 H7 只钉前两张表，`?`/`or`/`catch`/后缀 `!`
	// 四个 E1–E4 运算符与 `bytes`/`chan[T]`/`map[K]V`/`set[T]`/`[T;N]`/`func(T)->R` 六个
	// 类型构造子都没有断言（文档与实现脱节时门禁看不出来）。
	t.Run("operators", func(t *testing.T) {
		// 冻结运算符表（核心设计 §一 + §16 N5 的四个错误通路运算符）。
		// 逐条**必须能词法成 PUNCT 或 IDENT**（`or`/`catch`/`check`/`in` 是上下文关键字，
		// 词法层按 IDENT 流出、由解析层在语句/表达式位认出）。
		ops := []string{
			"+", "-", "*", "/", "%",
			"==", "!=", "<", "<=", ">", ">=",
			"&&", "||", "!",
			"&", "|", "^", "<<", ">>",
			"=", "+=", "-=", "*=", "/=", "%=", "&=", "|=", "^=", "<<=", ">>=",
			".", ",", ";", ":", "::", "->", "=>", "?", "...", "..", "[", "]", "(", ")", "{", "}",
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
		// 上下文关键字（词法层 = IDENT；解析层在对应位置认出）。
		// 注意 `check` **是** 23 关键字之一（不是上下文关键字），故不在此列。
		for _, ck := range []string{"or", "catch", "errdefer", "comptime"} {
			toks, errs := File("t.aic", ck)
			if len(errs) != 0 || toks[0].Kind != IDENT {
				t.Errorf("上下文关键字 %q 必须是 IDENT（不占 23 关键字表），实际 %v %v", ck, toks[0], errs)
			}
			if Keywords[ck] {
				t.Errorf("上下文关键字 %q 不得进关键字表（H7）", ck)
			}
		}
	})

	t.Run("type-constructors", func(t *testing.T) {
		// 类型构造子：预声明类型名 + 容器/通道/数组/函数类型的**前缀与后缀**写法。
		// 判据 = 词法干净（名必须是 IDENT 或关键字位）；语义解析由 parse/types 负责。
		idents := []string{"bytes", "f32", "f64", "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize", "bool", "str", "map", "set", "chan"}
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
