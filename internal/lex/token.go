package lex

import "fmt"

// Kind classifies a lexical token. The set is frozen together with the
// surface itself (H7 / D9-D10).
type Kind int

const (
	ILL   Kind = iota // illegal lexeme; a LexerError was recorded, scanning continued
	EOF               // end of file — always the last token of a stream
	IDENT             // ASCII letter or `_` start (top-level visibility: A-Z = public, lowercase/_ = package-private); ASCII digits and single _ may follow
	INT               // decimal, 0x hex, 0b binary; _ separators allowed
	FLOAT             // digits '.' digits with optional exponent
	STR               // "..." with escapes \n \t \r \" \\
	KW                // one of the 36 keywords
	RES               // reserved word: true / false / nil
	PUNCT             // operators and delimiters (a lone `_` = blank binding token)
)

func (k Kind) String() string {
	switch k {
	case ILL:
		return "ILL"
	case EOF:
		return "EOF"
	case IDENT:
		return "IDENT"
	case INT:
		return "INT"
	case FLOAT:
		return "FLOAT"
	case STR:
		return "STR"
	case KW:
		return "KW"
	case RES:
		return "RES"
	case PUNCT:
		return "PUNCT"
	}
	return "UNKNOWN"
}

// Pos is a 1-based source position carried by every token and error.
type Pos struct {
	File string
	Line int
	Col  int
}

func (p Pos) String() string { return fmt.Sprintf("%s:%d:%d", p.File, p.Line, p.Col) }

// Token is one lexical unit. For STR, Text is the raw source slice including
// the surrounding quotes; decoding happens in a later stage.
type Token struct {
	Kind Kind
	Text string
	Pos  Pos
}

// String 用 `at` 而不是 `@` 连接位置：`@` 不是词法可接受字符（§1.3），
// 让它出现在任何输出里都会让 grep `@` 的门禁条件变得无法字面成立。
func (t Token) String() string {
	return fmt.Sprintf("%s %q at %s", t.Kind, t.Text, t.Pos)
}
