package lex

import "fmt"

// Kind classifies a lexical token. The set is frozen together with the
// surface itself (核心设计 §2.1 / H7 / D9-D10).
type Kind int

const (
	ILL   Kind = iota // illegal lexeme; a LexerError was recorded, scanning continued
	EOF               // end of file — always the last token of a stream
	IDENT             // ASCII letter or `_` start (top-level visibility: A-Z = public, lowercase/_ = package-private); ASCII digits and single _ may follow
	INT               // decimal, 0x hex, 0b binary; _ separators allowed
	FLOAT             // digits '.' digits with optional exponent
	STR               // "..." with escapes \n \t \r \" \\
	ANNOT             // @name — one of the five frozen annotations
	KW                // one of the 23 keywords
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
	case ANNOT:
		return "ANNOT"
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

func (t Token) String() string {
	return fmt.Sprintf("%s %q @ %s", t.Kind, t.Text, t.Pos)
}
