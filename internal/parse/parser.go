package parse

import (
	"strings"
)

// Token kind strings shared with internal/lex.
const (
	KW    = "KW"
	IDENT = "IDENT"
	INT   = "INT"
	FLOAT = "FLOAT"
	STR   = "STR"
	ANNOT = "ANNOT"
	RES   = "RES"
	PUNCT = "PUNCT"
	ILL   = "ILL"
	EOF   = "EOF"
)

// Parser is a hand-written single-pass LL parser (核心设计 §三: 单遍、无回溯).
// On a syntax error it records the diagnostic, resynchronises at the next
// statement or declaration boundary and keeps going, so one run reports all
// errors rather than stopping at the first (红线#16).
type Parser struct {
	file   string
	toks   []Token
	i      int
	Errors []ParseError

	scopeDepth  int // >0 inside `scope { }` — spawn requires it (红线 5)
	noComposite int // >0 inside a condition — `{` opens a body, never C{…}
}

// Token mirrors lex.Token without importing lex internals beyond the kinds.
type Token struct {
	Kind string // "KW" | "IDENT" | "INT" | "FLOAT" | "STR" | "ANNOT" | "RES" | "PUNCT" | "ILL" | "EOF"
	Text string
	Line int
	Col  int
	File string
}

// Pos is a 1-based position used by AST nodes and diagnostics.
type Pos struct {
	File string
	Line int
	Col  int
}

func (p Pos) String() string { return p.posString() }

// exprPos returns the position of an expression node. Every expression node
// embeds Pos; nodePos (expr.go) is the single implementation.
func exprPos(e Expr) Pos { return nodePos(e) }

// ExprPos is the exported accessor for an expression node's position.
func ExprPos(e Expr) Pos { return exprPos(e) }

// TypeExprPos is the exported accessor for a type node's position.
func TypeExprPos(t TypeExpr) Pos { return typePos(t) }

// typePos returns the position of a type expression node.
func typePos(t TypeExpr) Pos {
	switch v := t.(type) {
	case *BasicType:
		return v.Pos
	case *SliceType:
		return v.Pos
	case *SetType:
		return v.Pos
	case *MapType:
		return v.Pos
	case *ArrayType:
		return v.Pos
	case *NamedType:
		return v.Pos
	case *FuncType:
		return v.Pos
	}
	return Pos{}
}

func (p Pos) posString() string {
	return itoa(p.Line) + ":" + itoa(p.Col)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// ParseError is one collected syntax diagnostic in the frozen three-part shape
// (H5 / 核心设计 §六).
type ParseError struct {
	Pos     Pos
	Desc    string
	Context string
	Fix     string
}

// Format renders `<file>:<line>:<col>: error:…` + context + fix.
func (e ParseError) Format() string {
	var b strings.Builder
	b.WriteString(e.Pos.File)
	b.WriteString(":")
	b.WriteString(itoa(e.Pos.Line))
	b.WriteString(":")
	b.WriteString(itoa(e.Pos.Col))
	b.WriteString(": error: ")
	b.WriteString(e.Desc)
	b.WriteString("\n    ")
	b.WriteString(e.Context)
	b.WriteString("\n    fix: ")
	b.WriteString(e.Fix)
	return b.String()
}

// FormatAll renders all diagnostics in source order.
func FormatAll(path string, errs []ParseError) string {
	var blocks []string
	for _, e := range errs {
		if e.Pos.File == "" {
			e.Pos.File = path
		}
		blocks = append(blocks, e.Format())
	}
	return strings.Join(blocks, "\n")
}

// --- token cursor -------------------------------------------------------------

func (p *Parser) cur() Token { return p.toks[p.i] }

func (p *Parser) atEOF() bool { return p.cur().Kind == EOF }

func (p *Parser) peekKind() string { return p.toks[min(p.i+1, len(p.toks)-1)].Kind }

func (p *Parser) peekText() string { return p.toks[min(p.i+1, len(p.toks)-1)].Text }

func (p *Parser) peekIs(kind, text string) bool {
	t := p.toks[min(p.i+1, len(p.toks)-1)]
	return t.Kind == kind && t.Text == text
}

func (p *Parser) at(kind, text string) bool {
	t := p.cur()
	return t.Kind == kind && t.Text == text
}

// lookahead returns the token n positions ahead of the cursor.
func (p *Parser) lookahead(n int) Token {
	return p.toks[min(p.i+n, len(p.toks)-1)]
}

// atKind reports whether the current token has the given kind, whatever its
// text (used for IDENT/RES positions where the spelling does not matter).
func (p *Parser) atKind(kind string) bool { return p.cur().Kind == kind }

func (p *Parser) atAnyPUNCT(texts ...string) bool {
	t := p.cur()
	if t.Kind != "PUNCT" {
		return false
	}
	for _, x := range texts {
		if t.Text == x {
			return true
		}
	}
	return false
}

func (p *Parser) next() Token {
	t := p.toks[p.i]
	if p.i < len(p.toks)-1 {
		p.i++
	}
	return t
}

func (p *Parser) pos() Pos {
	t := p.cur()
	return Pos{File: p.file, Line: t.Line, Col: t.Col}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// errorHere records a diagnostic at an explicit position.
//
// **同一位置只留一条**（恢复窗口去重）：一处语法错常被后续恢复路径重复报
// （审计实测：一处错 3–5 条、多在同一位置），级联噪音会把首因埋掉。
// 判据是"位置完全相同"——同一 token 上的第二条诊断几乎必然是同一根因的余波；
// 不同位置的诊断照常报（真正的多处错误不会被吞）。
func (p *Parser) errorHere(at Pos, desc, context, fix string) {
	if n := len(p.Errors); n > 0 {
		last := p.Errors[n-1].Pos
		if last.Line == at.Line && last.Col == at.Col {
			return
		}
	}
	p.Errors = append(p.Errors, ParseError{Pos: at, Desc: desc, Context: context, Fix: fix})
}

// errorCur records a diagnostic at the current token.
func (p *Parser) errorCur(desc, context, fix string) {
	p.errorHere(p.pos(), desc, context, fix)
}

func describe(t Token) string {
	if t.Kind == "EOF" {
		return "end of file (EOF)"
	}
	return t.Kind + " " + t.Text
}

// expect consumes a specific token or records an error naming what was found.
// ILL tokens are exempt: the lexer already reported them (报错不重复计因).
func (p *Parser) expect(kind, text, fix string) bool {
	if p.at(kind, text) {
		p.next()
		return true
	}
	if p.cur().Kind == ILL {
		return false
	}
	t := p.cur()
	p.errorCur("missing `"+text+"`", "got "+describe(t), fix)
	return false
}

// synchronize skips to the next statement or declaration boundary so parsing
// can continue after an error (核心设计 §三: 跳到下一个语句边界继续报).
func (p *Parser) synchronize(stopAtBrace bool) {
	start := p.i
	p.synchronizeInner(stopAtBrace)
	if p.i == start && p.cur().Kind != EOF {
		// 已停在块边界时不得吞掉 `}`：上层块还指望它收尾。
		if stopAtBrace && p.at(PUNCT, "}") {
			return
		}
		p.next()
	}
}

func (p *Parser) synchronizeInner(stopAtBrace bool) {
	depth := 0
	for {
		t := p.cur()
		if t.Kind == "EOF" {
			return
		}
		if stopAtBrace && t.Kind == "PUNCT" {
			if t.Text == "}" {
				if depth == 0 {
					return
				}
				depth--
			} else if t.Text == "{" {
				depth++
			}
		}
		if depth == 0 && isBoundary(t) {
			return
		}
		p.next()
	}
}

// isBoundary marks tokens that start a fresh statement or declaration (v2 表面).
func isBoundary(t Token) bool {
	if t.Kind == "KW" {
		switch t.Text {
		case "var", "const", "func", "class", "interface", "enum", "import",
			"export", "extern", "if", "else", "for", "return",
			"break", "continue", "match", "defer", "check",
			"spawn", "scope", "region":
			return true
		}
	}
	return t.Kind == "ANNOT"
}
