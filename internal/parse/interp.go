package parse

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// ---------------------------------------------------------------------------
// N2 字符串插值（核心设计 §16）：`"id={id} name:{u.name} ok:{flag}"`。
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
	// 占位符内容：标识符（可带点），到 `}` 结束。
	end := strings.IndexByte(body[j:], '}')
	if end < 0 {
		p.errorHere(at, "the `{` of an interpolation placeholder is never closed", "{",
			"close it with }: \"{name}\" (core design §16 N2)")
		return nil, len(body), false
	}
	end += j
	inner := body[j:end]
	if inner == "" {
		p.errorHere(at, "interpolation accepts a name or a field only", "{}",
			"write {name} or {obj.field}; expressions and format specifiers are not part of the surface (core design §16 N2)")
		return nil, end + 1, false
	}
	var node Expr
	for k, seg := range strings.Split(inner, ".") {
		if !validIdent(seg) {
			p.errorHere(at, "interpolation accepts a name or a field only", "{"+inner+"}",
				"write {name} or {obj.field}; expressions, calls and format specifiers are not part of the surface (core design §16 N2)")
			return nil, end + 1, false
		}
		if k == 0 {
			node = &Ident{Name: seg, Pos: at}
			continue
		}
		node = &Field{X: node, Name: seg, Pos: at}
	}
	return node, end + 1, true
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
