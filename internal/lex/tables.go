package lex

// ---------------------------------------------------------------------------
// AIC v7 frozen surface (《AIC 语言规范》§1). These tables are the single
// source of truth for the literal surface. TestFrozenSurface asserts them
// against the frozen lists in both directions.
//
// v7 换基：36 硬关键字，**0 个上下文关键字**，**0 个注解**。
//   - 相对 v2 的 24 关键字，新增 struct/errdefer/select/case/use/implements/
//     include/derive/comptime/assert/catch/into；
//   - `@packed` 由 struct 取代，`@derive(A,B)` 成为声明修饰词，
//     `@noblock`/`@blocking` 成为 extern 修饰词 —— 注解机制整体删除；
//   - `catch` 从上下文词升为硬关键字（错误处理必经之路，不容猜测）；
//   - `?` 与 `or` 从运算符表删除（同一语义两种拼写，P1 违反）。
// ---------------------------------------------------------------------------

// Keywords holds exactly the 36 frozen keywords — no more, no fewer.
var Keywords = map[string]bool{
	// 类型与声明
	"class": true, "interface": true, "enum": true, "struct": true,
	"var": true, "const": true, "live": true, "func": true,
	// 控制流
	"if": true, "else": true,
	"for": true, "in": true,
	"return": true, "break": true, "continue": true,
	// 错误处理
	"match": true, "defer": true, "errdefer": true, "check": true, "catch": true,
	// 并发
	"spawn": true, "scope": true, "region": true, "this": true, "into": true,
	// 接口与派生
	"select": true, "case": true, "use": true, "implements": true,
	"include": true, "derive": true,
	// 编译期
	"comptime": true, "assert": true,
	// 包与 ABI
	"import": true, "export": true, "extern": true,
}

// KeywordNames lists the frozen spellings in the order of 《AIC 语言规范》§1.3.
var KeywordNames = []string{
	"class", "interface", "enum", "struct", "var", "const", "live", "func",
	"if", "else", "for", "in",
	"return", "break", "continue",
	"match", "defer", "errdefer", "check", "catch",
	"spawn", "scope", "region", "this", "into",
	"select", "case", "use", "implements", "include", "derive",
	"comptime", "assert",
	"import", "export", "extern",
}

// ReservedNames: true / false / nil.
var ReservedNames = []string{"true", "false", "nil"}

// BasicTypeNames are the 15 reserved type names (《AIC 语言规范》§2.1).
// They may not be used as identifiers (C2).
var BasicTypeNames = []string{
	"i8", "i16", "i32", "i64",
	"u8", "u16", "u32", "u64", "usize",
	"f32", "f64",
	"bool", "str", "rune", "Err",
}

// isIdentStart: an identifier begins with an **ASCII letter or `_`**
// (《AIC 语言规范》§1.1 — Unicode 字母不作首字符，否则「首字符大写 =
// 跨包可见」在 Unicode 上没有定义). A leading '_' is routed here too:
// `_name` is a legal identifier (包私有); the bare `_` alone is the blank
// token, not an identifier.
func isIdentStart(r rune) bool { return isASCIILetter(r) || r == '_' }

// isIdentCont: ASCII letters, ASCII digits, or a single underscore; "__"
// adjacent is rejected while scanning (reserved for mangling).
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
// lexical error.
//
// v7 删除：`?`（与 check 同语义）、`::`、`...`。
// 未采用：复合位运算赋值（`&=` 等）—— 写 `x = x & m` 零歧义。
var punct = map[string]bool{
	"==": true, "!=": true, "<=": true, ">=": true,
	"&&": true, "||": true, "<<": true, ">>": true,
	"+=": true, "-=": true, "*=": true, "/=": true, "%=": true,
	"->": true, "=>": true, "..": true,
	"(": true, ")": true, "{": true, "}": true, "[": true, "]": true,
	",": true, ";": true, ".": true, ":": true, "_": true,
	"+": true, "-": true, "*": true, "/": true, "%": true,
	"=": true, "<": true, ">": true, "!": true,
	"&": true, "|": true, "^": true, "~": true,
}
