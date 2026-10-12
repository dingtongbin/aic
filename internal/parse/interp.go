package parse

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"aic/internal/lex"
)

// ---------------------------------------------------------------------------
// 字符串插值：`"id={id} name:{u.name} ok:{flag}"`。
//
// 规则：
//   - 占位符 = `{` + **名字或字段访问** + `}`；不接受表达式、不接受函数调用、
//     不接受格式说明符；
//   - `{{` / `}}` 是字面 `{` / `}` 的转义；孤立的 `{` / `}` = 解析错；
//   - 只有串里出现 `{` 才进入插值模式（`"a}b"` 仍是普通字面量）。
//
// 解析在**词法之后**做：STR token 已带原文与起始位置，这里按 rune 计数还原
// 每个占位符的精确列号（诊断必须能指到出错的那一个字符）。
// ---------------------------------------------------------------------------

// interpLit 尝试把 STR token 解析成插值字面量；返回 false 表示它是普通字符串。
func (p *Parser) interpLit(t Token) (Expr, bool) {
	raw := t.Text
	if len(raw) < 2 || !strings.Contains(raw, "{") {
		return nil, false
	}
	body := raw[1 : len(raw)-1]
	lit := &InterpLit{Pos: tokPos(t)}
	var text strings.Builder
	i := 0
	for i < len(body) {
		ch := body[i]
		switch ch {
		case '\\':
			// 转义序列原样进文本：解码在降级/发射层（与 StrLit 同源，禁第二份）。
			if i+1 < len(body) {
				text.WriteByte(ch)
				text.WriteByte(body[i+1])
				i += 2
			} else {
				text.WriteByte(ch)
				i++
			}
		case '{':
			if i+1 < len(body) && body[i+1] == '{' {
				text.WriteByte('{')
				i += 2
				continue
			}
			name, used, ok := p.interpName(t, body, i)
			if !ok {
				return nil, true // 已报错
			}
			lit.Texts = append(lit.Texts, text.String())
			text.Reset()
			lit.Values = append(lit.Values, name)
			i = used
		case '}':
			if i+1 < len(body) && body[i+1] == '}' {
				text.WriteByte('}')
				i += 2
				continue
			}
			p.errorHere(p.colAt(t, i), "a lone `}` in an interpolated string must be written `}}`", "}",
				"write }} for a literal }; {{ and }} are the escapes (core design §16 N2)")
			return nil, true
		default:
			text.WriteByte(ch)
			i++
		}
	}
	lit.Texts = append(lit.Texts, text.String())
	if len(lit.Values) == 0 {
		// 串里有 `{` 但没有占位符（例如只有 `{{`）：退化成普通字面量。
		return &StrLit{Text: t.Text, Pos: tokPos(t)}, true
	}
	return lit, true
}

// interpName 读一个占位符：`名字` 或 `名字.字段[.字段…]`。返回构建好的表达式
// 与下一个待扫描的下标（`}` 之后）。
func (p *Parser) interpName(t Token, body string, start int) (Expr, int, bool) {
	at := p.colAt(t, start)
	j := start + 1
	// 占位符内容：一段表达式，到**配平的** `}` 结束。
	// 判据：表达式内可能出现复合字面量的花括号（`{p == Point{x: 1}}`），
	// 用 IndexByte 找第一个 `}` 会把表达式截断 —— 必须按花括号深度配平。
	// 判据二：表达式内可能出现**字符串字面量**（`{s.startsWith("}")}`），
	// 字符串里的 `}` 不参与配平 —— 必须跳过字符串体。
	depth := 1
	k := j
	for k < len(body) {
		// 判据零：`{{` / `}}` 是**字面花括号转义**（§1.4），不参与深度配平。
		// 但仅在**不处于嵌套花括号内**时成立：`{p == Point{x: 1, y: 2}}` 的
		// 结尾 `}}` 中，第一个 `}` 关闭复合字面量（depth 2→1），第二个 `}`
		// 才闭合占位符（depth 1→0）——把它当转义就会永找不到闭合（审计实测）。
		if depth == 1 && body[k] == '{' && k+1 < len(body) && body[k+1] == '{' {
			k += 2
			continue
		}
		if depth == 1 && body[k] == '}' && k+1 < len(body) && body[k+1] == '}' {
			// depth==1 时的 `}}`：第一个 `}` 闭合占位符（depth→0），
			// 第二个 `}` 是字面量。这里只消费第一个。
			depth--
			k++
			break
		}
		if body[k] == '"' {
			k++
			for k < len(body) && body[k] != '"' {
				if body[k] == '\\' {
					k++
				}
				k++
			}
			if k < len(body) {
				k++
			}
			continue
		}
		switch body[k] {
		case '{':
			depth++
		case '}':
			depth--
		}
		if depth == 0 {
			break
		}
		k++
	}
	if depth != 0 {
		p.errorHere(at, "the `{` of an interpolation placeholder is never closed", "{",
			"close it with }: \"{name}\" (AIC language spec §1.4)")
		return nil, len(body), false
	}
	end := k
	inner := body[j:end]
	if strings.TrimSpace(inner) == "" {
		p.errorHere(at, "interpolation placeholder is empty", "{}",
			"write {name} or {obj.field} (AIC language spec §1.4)")
		return nil, end + 1, false
	}
	// 占位符是**表达式**（《AIC 语言规范》§1.4）。只许 name.field 会逼用户把
	// `xs.len()` 拆成临时变量 —— 插值是唯一格式化路径，窄到不可用就会把人
	// 推回 str.fromXxx 噪声。这里用子 parser 解析该段表达式。
	node, ok := p.parseInterpExpr(inner, at)
	if !ok {
		return nil, end + 1, false
	}
	return node, end + 1, true
}

// mustLex 词法分析一小段文本，返回 parse 包的 token 流。
// 只能用于插值占位符这类"已知无语法错误"的短片段 —— 词法错误由调用方转译
// 成"占位符不是合法表达式"的诊断。
func mustLex(src string) []Token {
	toks, _ := lex.File("<interp>", src)
	return FromLex(toks)
}

// parseInterpExpr 解析插值占位符内的一段表达式。
// 做法：把该段文本喂给一个**子 parser**，取其第一个表达式。
// 判据：插值位的表达式与普通表达式位必须是同一套语法（P1 无歧义）——
// 自造一套"占位符迷你语法"会让 agent 学两套规则。
func (p *Parser) parseInterpExpr(text string, at Pos) (Expr, bool) {
	sub := New(p.file, mustLex(text))
	if len(sub.Errors) > 0 {
		p.errorHere(at, "the interpolation placeholder is not a valid expression", "{"+text+"}",
			"write a plain expression: {name} / {obj.field} / {xs.len()} (AIC language spec §1.4)")
		return nil, false
	}
	e := sub.parseExpr()
	if e == nil || len(sub.Errors) > 0 {
		p.errorHere(at, "the interpolation placeholder is not a valid expression", "{"+text+"}",
			"write a plain expression: {name} / {obj.field} / {xs.len()} (AIC language spec §1.4)")
		return nil, false
	}
	return e, true
}

// colAt 把**串体**（不含首尾引号）内的字节偏移换算成源列号（按 rune 计数，
// 与词法器一致）。偏移 +1 补回首引号，否则诊断会左偏一列。
func (p *Parser) colAt(t Token, off int) Pos {
	raw := t.Text
	off++ // 跳过首引号
	if off > len(raw) {
		off = len(raw)
	}
	col := t.Col
	if off > 0 {
		col += utf8.RuneCountInString(raw[:off])
	}
	return Pos{File: t.File, Line: t.Line, Col: col}
}

// tokPos 把词法 token 的位置搬成语法位置。
func tokPos(t Token) Pos { return Pos{File: t.File, Line: t.Line, Col: t.Col} }

// isInterpIdentStart / isInterpIdentCont 与词法器的标识符规则同形
// （首字符字母或 `_`，后续字母数字或 `_`）。
func isInterpIdentStart(r rune) bool { return unicode.IsLetter(r) || r == '_' }

func isInterpIdentCont(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// validIdent 报告一段占位符文本是否是一个合法标识符（与词法器同形：
// 字母/下划线开头，字母数字下划线续，且不含保留的 `__`）。
func validIdent(s string) bool {
	if s == "" || strings.Contains(s, "__") {
		return false
	}
	for i, r := range s {
		if i == 0 {
			if !isInterpIdentStart(r) {
				return false
			}
			continue
		}
		if !isInterpIdentCont(r) {
			return false
		}
	}
	return true
}
