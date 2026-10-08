package parse

import "aic/internal/lex"

// FromLex converts a lexer token stream into parser tokens. Keeping the
// conversion here means internal/parse does not depend on lex internals and
// tests can drive the parser with hand-written token slices.
func FromLex(toks []lex.Token) []Token {
	out := make([]Token, 0, len(toks)+1)
	for _, t := range toks {
		out = append(out, Token{
			Kind: t.Kind.String(),
			Text: t.Text,
			Line: t.Pos.Line,
			Col:  t.Pos.Col,
			File: t.Pos.File,
		})
	}
	if len(out) == 0 || out[len(out)-1].Kind != EOF {
		out = append(out, Token{Kind: EOF, File: ""})
	}
	return out
}

// Source is the one-call entry: lex then parse a file buffer. Both stages'
// diagnostics are returned separately; the caller decides how to merge them.
func Source(path, src string) (*File, []lex.LexerError, []ParseError) {
	toks, lexErrs := lex.File(path, src)
	p := New(path, FromLex(toks))
	f := p.ParseFile()
	return f, lexErrs, p.Errors
}
