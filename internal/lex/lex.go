package lex

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Scanner walks one source file. It never stops at the first error: every
// failure is recorded and scanning resumes at the next token boundary
// (核心设计 §三 解析重点：多错一次报；红线#16).
type Scanner struct {
	file   string
	src    string
	off    int // byte offset into src
	line   int
	col    int
	tokens []Token
	Errors []LexerError
}

// File lexes one source buffer. It returns the token stream plus every
// lexical error collected (empty slice = clean scan). The stream always ends
// with an EOF token, even when errors occurred.
func File(file, src string) ([]Token, []LexerError) {
	s := &Scanner{file: file, src: src, line: 1, col: 1}
	s.run()
	return s.tokens, s.Errors
}

// --- infrastructure ---------------------------------------------------------

func (s *Scanner) pos() Pos { return Pos{File: s.file, Line: s.line, Col: s.col} }

func (s *Scanner) atEOF() bool { return s.off >= len(s.src) }

// peekAt returns the rune n runes ahead without consuming; -1 at EOF.
func (s *Scanner) peekAt(n int) rune {
	off := s.off
	for i := 0; i < n && off < len(s.src); i++ {
		_, w := utf8.DecodeRuneInString(s.src[off:])
		off += w
	}
	if off >= len(s.src) {
		return -1
	}
	r, _ := utf8.DecodeRuneInString(s.src[off:])
	return r
}

func (s *Scanner) peek() rune { return s.peekAt(0) }

// advance consumes exactly one rune and keeps line/col in sync.
func (s *Scanner) advance() {
	if s.atEOF() {
		return
	}
	r, w := utf8.DecodeRuneInString(s.src[s.off:])
	s.off += w
	if r == '\n' {
		s.line++
		s.col = 1
	} else {
		s.col++
	}
}

func (s *Scanner) emit(kind Kind, text string, at Pos) {
	s.tokens = append(s.tokens, Token{Kind: kind, Text: text, Pos: at})
}

// fail records a three-part error and emits the matching ILL token.
func (s *Scanner) fail(at Pos, desc, context, fix string) {
	s.Errors = append(s.Errors, LexerError{Pos: at, Desc: desc, Context: context, Fix: fix})
	s.emit(ILL, context, at)
}

// --- trivia -----------------------------------------------------------------

func (s *Scanner) skipTrivia() {
	for !s.atEOF() {
		r := s.peek()
		switch {
		case r == ' ' || r == '\t' || r == '\r' || r == '\n':
			s.advance()
		case r == '/' && s.peekAt(1) == '/': // // and /// share the line shape
			for !s.atEOF() && s.peek() != '\n' {
				s.advance()
			}
		case r == '/' && s.peekAt(1) == '*':
			start := s.pos()
			s.advance()
			s.advance()
			closed := false
			for !s.atEOF() {
				if s.peek() == '*' && s.peekAt(1) == '/' { // non-nesting: first */ closes
					s.advance()
					s.advance()
					closed = true
					break
				}
				s.advance()
			}
			if !closed {
				s.fail(start, "unterminated block comment (reached end of file)", "/* … (end of file)",
					"close the comment with */ where the commented text ends")
			}
		default:
			return
		}
	}
}

// --- numbers (D10) ----------------------------------------------------------

// eatDigits consumes a digit body of the given radix plus '_' separators.
// Per D10 a separator must sit exactly between two digits: leading, trailing
// and doubled separators are lexical errors.
func (s *Scanner) eatDigits(at Pos, radix int, b *strings.Builder) (int, bool) {
	valid := func(r rune) bool {
		switch radix {
		case 16:
			return isHexDigit(r)
		case 2:
			return isBinDigit(r)
		default:
			return isDigit(r)
		}
	}
	prevDigit := false
	sepPos := Pos{}
	n := 0
	for !s.atEOF() {
		r := s.peek()
		if valid(r) {
			b.WriteRune(r)
			s.advance()
			prevDigit = true
			n++
			continue
		}
		if r == '_' {
			b.WriteRune('_')
			badPos := s.pos()
			s.advance()
			if !prevDigit {
				s.fail(badPos, "the digit separator _ must sit between digits", "'_' at a digit boundary",
					"write 0x00ff / 0b1010 / 1_000_000; _ may not lead, trail, or repeat")
				return n, false
			}
			sepPos = badPos
			prevDigit = false
			continue
		}
		break
	}
	if n > 0 && !prevDigit && sepPos.File != "" {
		// trailing separator: the body ended right after '_'
		s.fail(sepPos, "the digit separator _ must sit between digits", "'_' at a digit boundary",
			"write 0x00ff / 0b1010 / 1_000_000; _ may not lead, trail, or repeat")
		return n, false
	}
	return n, n > 0
}

func (s *Scanner) scanNumber(at Pos) {
	var b strings.Builder

	// 0x / 0b prefixes (lowercase only — frozen surface, D10).
	if s.peek() == '0' && (s.peekAt(1) == 'x' || s.peekAt(1) == 'b') {
		radix := 16
		if s.peekAt(1) == 'b' {
			radix = 2
		}
		prefix := "0" + string(s.peekAt(1))
		b.WriteString(prefix)
		s.advance()
		s.advance()
		if _, ok := s.eatDigits(at, radix, &b); !ok {
			if len(s.Errors) == 0 || !strings.Contains(s.Errors[len(s.Errors)-1].Desc, "separator") {
				s.fail(at, prefix+" is missing a digit after it", prefix, "write "+prefix+"F or "+prefix+"1, i.e. with digits")
			}
			return
		}
		s.emit(INT, b.String(), at)
		return
	}

	if _, ok := s.eatDigits(at, 10, &b); !ok {
		return
	}

	switch r := s.peek(); {
	case r == '.' && isDigit(s.peekAt(1)):
		b.WriteString(".")
		s.advance()
		if _, ok := s.eatDigits(at, 10, &b); !ok {
			return
		}
		if e := s.peek(); e == 'e' || e == 'E' {
			b.WriteRune(e)
			s.advance()
			if sg := s.peek(); sg == '+' || sg == '-' {
				b.WriteRune(sg)
				s.advance()
			}
			if _, ok := s.eatDigits(at, 10, &b); !ok {
				return
			}
		}
		s.emit(FLOAT, b.String(), at)
	case r == '.' && s.peekAt(1) == '.':
		// `..` (for-in range / str view) may directly follow a number: 0..n.
		s.emit(INT, b.String(), at)
		return
	case r == '.':
		dotPos := s.pos()
		s.advance()
		s.fail(dotPos, "a digit followed by a bare '.' ('1.' is invalid)", b.String()+".",
			"integers need no dot; for a decimal write 1.0")
	case r == 'e' || r == 'E':
		s.fail(at, "an exponent requires a decimal point (D10: float = digits.digits [e|E ±digits])", b.String()+string(r),
			"exponents apply to decimals only: 1.5e5; integers take no exponent")
	default:
		s.emit(INT, b.String(), at)
	}
}

// --- identifiers, keywords, reserved words ------------------------------------

func (s *Scanner) scanIdent(at Pos, first rune) {
	var b strings.Builder
	b.WriteRune(first)
	s.advance()
	for !s.atEOF() {
		r := s.peek()
		if !isIdentCont(r) {
			break
		}
		if r == '_' && strings.HasSuffix(b.String(), "_") {
			// "__" is reserved for symbol mangling (核心设计 §2.1 高危).
			badPos := s.pos()
			b.WriteRune('_')
			s.advance()
			s.fail(badPos, "identifier contains a double underscore __ (reserved for symbol mangling)", b.String(),
				"use a single _ or camelCase; __ is reserved for symbol mangling")
			return
		}
		b.WriteRune(r)
		s.advance()
	}
	name := b.String()
	switch {
	case Keywords[name]:
		s.emit(KW, name, at)
	case name == "true" || name == "false" || name == "nil":
		s.emit(RES, name, at)
	default:
		s.emit(IDENT, name, at)
	}
}

// --- annotations -------------------------------------------------------------

func (s *Scanner) scanAnnotation(at Pos) {
	var b strings.Builder
	b.WriteRune('@')
	s.advance()
	for !s.atEOF() && isIdentCont(s.peek()) {
		b.WriteRune(s.peek())
		s.advance()
	}
	name := b.String()[1:]
	if !Annotations[name] {
		s.fail(at, "unknown annotation @"+name, b.String(),
			"use one of the five annotations: @packed @live @derive @noblock @blocking")
		return
	}
	s.emit(ANNOT, b.String(), at)
}

// --- strings (D10) -----------------------------------------------------------

// scanString consumes a double-quoted string. On an invalid escape it records
// one error and resynchronises to the closing quote (or end of line) so the
// same mistake does not cascade into spurious follow-up errors.
func (s *Scanner) scanString(at Pos) {
	var b strings.Builder
	b.WriteRune('"')
	s.advance()
	for !s.atEOF() {
		r := s.peek()
		switch {
		case r == '"':
			b.WriteRune('"')
			s.advance()
			s.emit(STR, b.String(), at)
			return
		case r == '\\':
			escPos := s.pos()
			s.advance()
			e := s.peek()
			switch e {
			case 'n', 't', 'r', '"', '\\':
				b.WriteRune('\\')
				b.WriteRune(e)
				s.advance()
			case -1:
				s.fail(escPos, "string still open at the end of the file", b.String(), "add the closing double quote")
				return
			default:
				bad := "\\" + string(e)
				s.fail(escPos, "invalid escape", bad,
					"drop the backslash to write the character, or use \\\\ for a literal backslash; valid escapes are \\n \\t \\r \\\" \\\\")
				s.resyncString()
				return
			}
		case r == '\n':
			s.fail(at, "string not closed on the same line", b.String(),
				"add the closing quote; split multi-line text into several strings")
			return
		default:
			b.WriteRune(r)
			s.advance()
		}
	}
	s.fail(at, "unterminated string (reached end of file)", b.String(), "add the closing double quote")
}

// resyncString skips to just past the closing quote, or to end of line.
func (s *Scanner) resyncString() {
	for !s.atEOF() {
		r := s.peek()
		if r == '\n' {
			return
		}
		if r == '"' {
			s.advance()
			return
		}
		if r == '\\' {
			s.advance()
			if !s.atEOF() {
				s.advance()
			}
			continue
		}
		s.advance()
	}
}

// --- operators ------------------------------------------------------------------

func (s *Scanner) scanPunct(at Pos, r rune) {
	if s.off+1 < len(s.src) && s.src[s.off+1] < utf8.RuneSelf {
		two := string(r) + string(s.src[s.off+1])
		if punct[two] { // longest match first
			s.advance()
			s.advance()
			s.emit(PUNCT, two, at)
			return
		}
	}
	if punct[string(r)] {
		s.advance()
		s.emit(PUNCT, string(r), at)
		return
	}
	s.fail(at, "unrecognized character", "'"+string(r)+"'",
		"remove the character, or use an operator frozen in core design §1")
	s.advance()
}

// --- main loop ---------------------------------------------------------------------

func (s *Scanner) run() {
	for {
		s.skipTrivia()
		if s.atEOF() {
			s.emit(EOF, "", s.pos())
			return
		}
		at := s.pos()
		r := s.peek()
		switch {
		case r == '_':
			// Three shapes: `_name` is a legal identifier (private by §4), `__`
			// anywhere is reserved for mangling (scanIdent reports it), and a
			// bare `_` is the blank binding token (parser decides legality).
			// This case must precede isIdentStart, which now admits '_'.
			n := s.peekAt(1)
			if n == '_' || unicode.IsLetter(n) || isDigit(n) {
				s.scanIdent(at, r)
			} else {
				s.scanPunct(at, r)
			}
		case isIdentStart(r):
			s.scanIdent(at, r)
		case isDigit(r):
			s.scanNumber(at)
		case r == '.' && isDigit(s.peekAt(1)):
			s.fail(at, "a float literal may not start with .", "digits after a leading dot",
				"write the integer part first: 0.5, not .5")
			s.advance()
		case r == '"':
			s.scanString(at)
		case r == '@':
			s.scanAnnotation(at)
		case r > 0x7f && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			// 标识符字符集是 ASCII（核心设计 §一，2026-10 收紧）：非 ASCII 的字母/数字
			// 不进标识符 —— 否则「首字符大写 = 跨包可见」在 Unicode 上没有定义。
			// 整串一次报（`变量名` 是**一个**错，不是一个字符一条）。
			start := at
			for !s.atEOF() {
				c := s.peek()
				if c <= 0x7f || !(unicode.IsLetter(c) || unicode.IsDigit(c)) {
					break
				}
				s.advance()
			}
			s.fail(start, "identifiers must use ASCII letters, digits and underscore",
				"non-ASCII identifier",
				"rename with ASCII: A-Z a-z 0-9 _ (core design §1; visibility = first character A-Z)")
		default:
			s.scanPunct(at, r)
		}
	}
}
