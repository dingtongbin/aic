package lex

// ---------------------------------------------------------------------------
// H7 frozen surface (核心设计 §一). These tables are the single source of
// truth for the literal surface. TestFrozenSurface asserts them against the
// frozen lists in both directions.
//
// v2 换基（2026-10-06）：23 关键字 + 5 注解；v1 的 implements/elseif/while/
// errdefer/handle/await/del 全部退场（附录 A），region 入场。
// ---------------------------------------------------------------------------

// Keywords holds exactly the 23 frozen keywords — no more, no fewer.
var Keywords = map[string]bool{
	"class": true, "interface": true, "enum": true, "var": true,
	"const": true, "func": true,
	"if": true, "else": true,
	"for": true, "in": true,
	"return": true, "break": true, "continue": true,
	"match": true, "defer": true,
	"check": true,
	"spawn": true, "scope": true, "region": true, "this": true,
	"import": true, "export": true, "extern": true,
}

// KeywordNames lists the frozen spellings in the order of 核心设计 §一.
var KeywordNames = []string{
	"class", "interface", "enum", "var", "const", "func",
	"if", "else", "for", "in",
	"return", "break", "continue",
	"match", "defer", "check",
	"spawn", "scope", "region", "this",
	"import", "export", "extern",
}

// RetiredNames are v1 keywords that are plain identifiers in v2. The parser
// gives them a targeted retirement diagnostic at statement position (附录 A /
// 防坑清单 1); the lexer stays neutral.
//
// 注意：`errdefer` **不在**此列 —— N8 已把它收回（核心设计 §16 N8），
// 以**上下文关键字**身份在语句位识别（H7：关键字恒为 23 个）。
var RetiredNames = []string{
	"while", "elseif", "implements", "handle", "await", "del", "mut",
}

// ReservedNames: true / false / nil. "nil = 全类型零值" is a checker rule
// (核心设计 §二.3), not a lexical one.
var ReservedNames = []string{"true", "false", "nil"}

// Annotations is the complete frozen set (核心设计 §一: 5 注解).
var Annotations = map[string]bool{
	"packed":   true,
	"live":     true,
	"derive":   true,
	"noblock":  true,
	"blocking": true,
}

// AnnotationNames in the order they appear in the core design text.
var AnnotationNames = []string{
	"packed", "live", "derive", "noblock", "blocking",
}

// isIdentStart: an identifier begins with an **ASCII letter or `_`**
// (核心设计 §一，2026-10 收紧：Unicode 字母不再作首字符 —— 否则「首字符大写 =
// 跨包可见」在 Unicode 上没有定义，`Ä`/`中` 这类字符的"大小写"不可判定)。
// A leading '_' is routed here too: `_name` is a legal identifier
// (包私有：首字符非 A-Z 即私有，核心设计 §四); the bare `_` alone is the blank
// token, not an identifier.
func isIdentStart(r rune) bool { return isASCIILetter(r) || r == '_' }

// isIdentCont: ASCII letters, ASCII digits, or a single underscore; "__" adjacent
// is rejected while scanning (reserved for mangling).
func isIdentCont(r rune) bool {
	return isASCIILetter(r) || isASCIIDigit(r) || r == '_'
}

// isASCIILetter / isASCIIDigit：标识符字符集是 **ASCII**（可移植、可与 C 符号
// 一一对应；mangling 也只允许 ASCII）。
func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isHexDigit(r rune) bool {
	return isDigit(r) || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
}

func isBinDigit(r rune) bool { return r == '0' || r == '1' }

// punct holds every frozen operator and delimiter. Two-character entries are
// tried first (longest match); a byte sequence absent from this table is a
// lexical error. `..` exists only in the for-in range and str-view positions
// (parser-enforced, 核心设计 §一/§二.3).
var punct = map[string]bool{
	"==": true, "!=": true, "<=": true, ">=": true,
	"&&": true, "||": true, "<<": true, ">>": true,
	"+=": true, "-=": true, "*=": true, "/=": true, "%=": true,
	"->": true, "=>": true, "..": true,
	"(": true, ")": true, "{": true, "}": true, "[": true, "]": true,
	",": true, ";": true, ".": true, ":": true, "_": true,
	"+": true, "-": true, "*": true, "/": true, "%": true,
	"=": true, "<": true, ">": true, "!": true,
	"?": true, // E1 传播的后缀形 `expr?`（核心设计 §15.7）
	"&": true, "|": true, "^": true, "~": true,
}
