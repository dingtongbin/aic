package parse

// Expression parsing with explicit precedence climbing (单遍，无回溯).
// Precedence, low to high: assignment < lambda < || < && < | < ^ < &
// < equality < relational < shift < additive < multiplicative < unary <
// postfix (call / index / field / composite literal).
//
// v2：`..` 只存在于 for-in 位与 str 视图位（核心设计 §一/§二.3）；复合字面量
// C{f: v} 合法化，但条件位禁写（与块体 `{` 歧义，Go 同款规则）。

// parseExpr parses one expression including assignment forms and the
// lowest-precedence error path `or` (核心设计 §15.7 E2).
func (p *Parser) parseExpr() Expr {
	e := p.parseAssign()
	// `expr or <默认值>`：优先级最低（低于赋值右侧的一切），左结合链式。
	for p.atIdent("or") && p.sameLine(e) {
		at := p.pos()
		p.next()
		def := p.parseAssign()
		e = &OrExpr{X: e, Default: def, Pos: at}
	}
	return e
}

// atIdent reports whether the cursor sits on a plain identifier with the given
// spelling. `or` / `catch` 保持**上下文关键字**身份（H7：关键字恒为 23 个），
// 只在运算符位被识别。
func (p *Parser) atIdent(name string) bool {
	t := p.cur()
	return t.Kind == IDENT && t.Text == name
}

func (p *Parser) parseAssign() Expr {
	lhs := p.parseBinary(0)
	// Multi-target form: `a, err = f()` / `a, _ = f()`. Detected by looking for
	// a comma followed by further assignment targets and then an assignment op.
	if op, ok := p.assignOp(); ok {
		at := p.pos()
		p.next()
		rhs := p.parseAssign()
		return &Assign{Op: op, LHS: lhs, Right: rhs, Pos: at}
	}
	if p.at(PUNCT, ",") && p.multiAssignAhead() && p.multiTargetsAhead() {
		return p.parseMultiAssignTail(lhs)
	}
	// Pure lambda with an unparenthesised parameter: x => e
	if id, ok := lhs.(*Ident); ok && p.at(PUNCT, "=>") {
		p.next()
		return p.parseLambdaTail([]string{id.Name}, id.Pos)
	}
	return lhs
}

// parseMultiAssignTail finishes `…, name = expr` after the first target is
// already parsed. Compound operators are rejected here so the diagnostic is
// precise rather than a downstream cascade.
func (p *Parser) parseMultiAssignTail(first Expr) Expr {
	at := p.pos()
	targets := []Expr{first}
	for p.at(PUNCT, ",") {
		p.next()
		if !p.atKind(IDENT) && !p.at(PUNCT, "_") {
			p.errorHere(p.pos(), "a multi-target assignment needs variable names as targets", "got "+describe(p.cur()),
				"write `name, name = f()`; `_` discards a slot; assign fields and indices separately")
			p.synchronize(true)
			return &MultiAssign{Op: "=", LHS: targets, Right: nil, Pos: at}
		}
		targets = append(targets, p.parseBinary(0))
	}
	op, _ := p.assignOp()
	p.next()
	rhs := p.parseAssign()
	return &MultiAssign{Op: op, LHS: targets, Right: rhs, Pos: at}
}

// multiAssignAhead scans forward for `… , name, name =` so a plain argument
// list `f(a, b)` is never mistaken for a multi-target assignment.
func (p *Parser) multiAssignAhead() bool {
	depth := 0
	for j := p.i; j < len(p.toks); j++ {
		t := p.toks[j]
		if t.Kind == EOF {
			return false
		}
		if t.Kind == PUNCT {
			switch t.Text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				depth--
			case ",":
				// keep scanning at this depth
			case "=":
				return depth == 0
			default:
				if _, isAssign := assignOpText(t.Text); isAssign {
					return depth == 0
				}
			}
			continue
		}
		if depth == 0 && t.Kind == KW && t.Text == "in" {
			return false
		}
	}
	return false
}

// multiTargetsAhead requires every comma-separated target before the assignment
// operator to be a plain identifier or `_`. `return 1, nil` and `f(a, b)` both
// fail this test, which separates them from `a, err = f()`.
func (p *Parser) multiTargetsAhead() bool {
	for j := p.i; j < len(p.toks); j++ {
		t := p.toks[j]
		if t.Kind == EOF {
			return false
		}
		if t.Kind == PUNCT && t.Text == "," {
			// Every comma must be followed by another target name.
			if j+1 >= len(p.toks) {
				return false
			}
			n := p.toks[j+1]
			if n.Kind != IDENT && !(n.Kind == PUNCT && n.Text == "_") {
				return false
			}
			continue
		}
		if t.Kind == PUNCT && t.Text == "_" {
			continue // `_` is a legal target name
		}
		if t.Kind == PUNCT {
			_, isAssign := assignOpText(t.Text)
			return isAssign
		}
		// A bare name is a target; anything else ends the scan.
		if t.Kind == IDENT {
			continue
		}
		return false
	}
	return false
}

func assignOpText(s string) (string, bool) {
	switch s {
	case "=", "+=", "-=", "*=", "/=", "%=":
		return s, true
	}
	return "", false
}

// assignOp reports the current token when it is an assignment operator.
func (p *Parser) assignOp() (string, bool) {
	t := p.cur()
	if t.Kind != PUNCT {
		return "", false
	}
	switch t.Text {
	case "=", "+=", "-=", "*=", "/=", "%=":
		return t.Text, true
	}
	return "", false
}

// binary levels; each level lists its operators left to right. 短路求值 for
// &&/|| is a checker/emitter property; precedence is lexical (核心设计 §一).
var binaryLevels = [][]string{
	{"||"},
	{"&&"},
	{"|"},
	{"^"},
	{"&"},
	{"==", "!="},
	{"<", "<=", ">", ">="},
	{"<<", ">>"},
	{"+", "-"},
	{"*", "/", "%"},
}

func (p *Parser) parseBinary(level int) Expr {
	if level >= len(binaryLevels) {
		return p.parseUnary()
	}
	left := p.parseBinary(level + 1)
	for {
		t := p.cur()
		if t.Kind != PUNCT {
			return left
		}
		matched := ""
		for _, op := range binaryLevels[level] {
			if t.Text == op {
				matched = op
				break
			}
		}
		if matched == "" {
			return left
		}
		p.next()
		right := p.parseBinary(level + 1)
		left = &Binary{Op: matched, Left: left, Right: right, Pos: Pos{File: p.file, Line: t.Line, Col: t.Col}}
	}
}

// parseUnary reads `-x`, `!x`, `~x`, `check x`, and the primary forms.
// 无一元 `+`（核心设计 §一）。
func (p *Parser) parseUnary() Expr {
	at := p.pos()
	switch {
	case p.at(PUNCT, "-"), p.at(PUNCT, "!"), p.at(PUNCT, "~"):
		op := p.next().Text
		return &Unary{Op: op, X: p.parseUnary(), Pos: at}
	case p.at(KW, "check"):
		p.next()
		return &CheckExpr{X: p.parseUnary(), Pos: at}
	case p.at(KW, "match"):
		return p.parseMatchExpr()
	}
	return p.parsePostfix()
}

// parseMatchExpr reads the expression-position match (核心设计 §三):
// 全分支为表达式，穷尽性保证必有值 — var x = match e { A => 1, B => 2 }.
func (p *Parser) parseMatchExpr() Expr {
	at := p.pos()
	p.next() // match
	p.enterNoComposite()
	subject := p.parseExpr()
	p.leaveNoComposite()
	m := &MatchExpr{Subject: subject, Pos: at}
	if !p.expect(PUNCT, "{", "match is missing `{`") {
		p.synchronize(false)
		return m
	}
	for {
		p.skipSemis()
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
		if p.at(PUNCT, "}") || p.cur().Kind == EOF {
			break
		}
		armAt := p.pos()
		patterns := p.parsePatterns()
		if !p.expect(PUNCT, "=>", "match arm is missing `=>`") {
			p.synchronize(true)
			continue
		}
		if p.at(PUNCT, "{") {
			p.errorHere(p.pos(), "an arm of a match expression is an expression, not a `{ ... }` block",
				"the arm's `{`",
				"use a block arm in statement position: match e { A => { ... } }; or make the arm produce a value")
			p.skipBraces()
			continue
		}
		val := p.parseExpr()
		m.Arms = append(m.Arms, MatchArm{Patterns: patterns, Value: val, Pos: armAt})
		if !p.at(PUNCT, ",") && !p.at(PUNCT, "}") {
			p.errorCur("match expression arms need `,` between them", "got "+describe(p.cur()),
				"separate expression arms with commas: var x = match e { A => 1, B => 2 }")
			p.synchronize(true)
		}
	}
	p.expect(PUNCT, "}", "match is missing the closing `}`")
	if len(m.Arms) == 0 {
		p.errorHere(at, "match needs at least one arm", "match "+describeAt(subject)+" is empty",
			"write every enum variant: var x = match e { A => 1, B => 2 }")
	}
	return m
}

// skipBraces consumes a balanced `{ … }` region so recovery lands after the
// literal instead of EOF (which would fabricate follow-on errors).
func (p *Parser) skipBraces() {
	depth := 0
	for !p.atEOF() {
		switch {
		case p.at(PUNCT, "{"):
			depth++
		case p.at(PUNCT, "}"):
			depth--
			p.next()
			if depth <= 0 {
				return
			}
			continue
		}
		p.next()
	}
}

// parsePostfix reads the primary expression then any chain of calls, indexes,
// field accesses and composite literals.
func (p *Parser) parsePostfix() Expr {
	x := p.parsePrimary()
	for {
		at := p.pos()
		switch {
		case p.at(PUNCT, "("):
			p.next()
			call := &Call{Fn: x, Pos: at}
			if !p.at(PUNCT, ")") {
				for {
					arg := p.parseExpr()
					call.Args = append(call.Args, arg)
					if p.at(PUNCT, ",") {
						p.next()
						continue
					}
					break
				}
			}
			p.expect(PUNCT, ")", "call is missing the closing `)`")
			x = call
		case p.at(PUNCT, "["):
			p.next()
			idx := p.parseExpr()
			var end Expr
			var typeArgs []Expr
			if p.at(PUNCT, "..") { // s[i..j] str 视图（核心设计 §二.3）
				p.next()
				end = p.parseExpr()
			} else if p.at(PUNCT, ",") {
				// `Name[T, U]`：表达式位的多实参泛型实例化（单实参走 Index 本身，
				// 与下标读写同形，由 checker 按头部名字区分）。
				for p.at(PUNCT, ",") {
					p.next()
					typeArgs = append(typeArgs, p.parseExpr())
				}
			}
			p.expect(PUNCT, "]", "index is missing the closing `]`")
			x = &Index{X: x, Index: idx, End: end, TypeArgs: typeArgs, Pos: at}
		case p.at(PUNCT, "."):
			p.next()
			if !p.atKind(IDENT) && p.cur().Kind != KW && p.cur().Kind != RES {
				p.errorHere(at, "member name is missing after the dot", "got "+describe(p.cur()),
					"write `.member`; a member name starts with an ASCII letter or `_` (this.x / sync.Mutex)")
				p.synchronize(false)
				return x
			}
			x = &Field{X: x, Name: p.next().Text, Pos: at}
		case p.at(PUNCT, "?"):
			// `expr?` = E1 传播（与 `check expr` 等价；后缀形位置无关，推荐）。
			p.next()
			x = &CheckExpr{X: x, Postfix: true, Pos: at}
		case p.at(PUNCT, "!"):
			// `expr!` = E4 断言（出错即 trap）。注意与一元 `!x` 的区别：
			// 这一支只在**表达式之后**到达，故不会与前缀取反冲突。
			p.next()
			x = &AssertExpr{X: x, Pos: at}
		case p.atIdent("catch") && p.sameLine(x):
			// `expr catch e { … }` = E3 就地处理。
			x = p.parseCatchTail(x)
		case p.at(PUNCT, "{") && p.noComposite == 0 && p.compositeHead(x) && p.sameLine(x):
			// C{f: v}（核心设计 §四）。条件位 noComposite > 0：`{` 归块体，
			// 不组字面量（`if ready {` 因此不受影响）。
			x = p.parseCompositeTail(x)
		default:
			return x
		}
	}
}

// compositeHead reports whether x can introduce a composite literal: a plain
// type name (Ident), a package-qualified one (Field on Ident), or a generic
// instantiation Box[i32] (Index over Ident/Field).
func (p *Parser) compositeHead(x Expr) bool {
	switch v := x.(type) {
	case *Ident:
		return true
	case *Field:
		_, ok := v.X.(*Ident)
		return ok
	case *Index:
		return p.compositeHead(v.X)
	}
	return false
}

// parseCompositeTail reads `{ name: expr, … }` after the type head.
func (p *Parser) parseCompositeTail(head Expr) Expr {
	at := nodePos(head)
	p.next() // {
	c := &CompositeLit{TypeName: head, Pos: at}
	if !p.at(PUNCT, "}") {
		for {
			fAt := p.pos()
			if !p.atKind(IDENT) && p.cur().Kind != KW && p.cur().Kind != RES {
				p.errorHere(fAt, "composite literal field name is missing", "got "+describe(p.cur()),
					"write `C{field: value}`; unlisted fields take their zero value")
				p.synchronize(true)
				break
			}
			name := p.next().Text
			p.expect(PUNCT, ":", "composite literal field name is missing its `:`")
			v := p.parseExpr()
			c.Fields = append(c.Fields, CompositeField{Name: name, Value: v, Pos: fAt})
			if p.at(PUNCT, ",") {
				p.next()
				continue
			}
			break
		}
	}
	p.expect(PUNCT, "}", "composite literal is missing the closing `}`")
	return c
}

// parseLambdaTail finishes a lambda after its parameter list: `=> body`.
// Body form: expression, or `{ …; return r }` block (核心设计 §三).
func (p *Parser) parseLambdaTail(params []string, at Pos) Expr {
	if p.at(PUNCT, "{") {
		// Block-body lambda. Disambiguated from a composite literal by the
		// arrow: `x => { … }` is always a body, never a literal.
		return &LambdaExpr{Params: params, Block: p.parseBlock(), Pos: at}
	}
	body := p.parseAssign()
	return &LambdaExpr{Params: params, Body: body, Pos: at}
}

// lambdaHeadAhead reports whether the `(` at the cursor opens a parameter
// list: elements are exactly `IDENT (, IDENT)*` (or `_`), and the closing `)`
// is followed by `=>`. Typed "parameters" like `(p str)` do not match — 参数
// 不带类型 (核心设计 §三). Pure lookahead — the cursor does not move.
func (p *Parser) lambdaHeadAhead() bool {
	depth := 0
	seenInElem := 0 // identifiers seen since the last top-level comma
	for j := p.i + 1; j < len(p.toks); j++ {
		t := p.toks[j]
		if t.Kind == EOF {
			return false
		}
		if t.Kind == PUNCT {
			switch t.Text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if t.Text == ")" && depth == 0 {
					if seenInElem == 0 {
						return false // empty list () — not a lambda head
					}
					nx := p.toks[min(j+1, len(p.toks)-1)]
					return nx.Kind == PUNCT && nx.Text == "=>"
				}
				depth--
			case ",":
				if depth == 0 {
					// element separator: both neighbours must be plain names
					prev, nxt := p.toks[j-1], p.toks[min(j+1, len(p.toks)-1)]
					prevOK := prev.Kind == IDENT || (prev.Kind == PUNCT && prev.Text == "_")
					nextOK := nxt.Kind == IDENT || (nxt.Kind == PUNCT && nxt.Text == "_")
					if !prevOK || !nextOK || seenInElem > 1 {
						return false
					}
					seenInElem = 0
				}
			case "_":
				if depth == 0 {
					seenInElem++
				}
			default:
				if depth == 0 {
					return false // operators/types inside → not a parameter list
				}
			}
			continue
		}
		if depth == 0 {
			if t.Kind != IDENT || seenInElem > 1 {
				return false
			}
			seenInElem++
		}
	}
	return false
}

// parenGroupFollowedByArrow 报告当前 `(` 的配对 `)` 之后是否紧跟 `->`。
// 用途：`-> (i32) -> i32` 里的括号是**函数类型的形参表**，不是结果列表。
// 纯前瞻，游标不动。
func (p *Parser) parenGroupFollowedByArrow() bool {
	depth := 0
	for j := p.i; j < len(p.toks); j++ {
		t := p.toks[j]
		if t.Kind == EOF {
			return false
		}
		if t.Kind != PUNCT {
			continue
		}
		switch t.Text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			depth--
			if depth == 0 && t.Text == ")" {
				nx := p.toks[min(j+1, len(p.toks)-1)]
				return nx.Kind == PUNCT && nx.Text == "->"
			}
		}
	}
	return false
}

// parseCatchTail reads `catch <名字> { … }` after the guarded expression
// (核心设计 §15.7 E3). 绑定的名字是块内的 Err；块的值规则由检查器裁决。
func (p *Parser) parseCatchTail(x Expr) Expr {
	at := p.pos()
	p.next() // catch
	c := &CatchExpr{X: x, Pos: at}
	if p.atKind(IDENT) || p.at(PUNCT, "_") {
		c.Name = p.next().Text
	} else {
		p.errorHere(p.pos(), "catch is missing the name it binds the error to", "got "+describe(p.cur()),
			"write `expr catch e { ... }`; e is the Err value inside the block")
	}
	p.enterNoComposite()
	c.Block = p.parseBlock()
	p.leaveNoComposite()
	return c
}

// parseFuncLit reads the N1 closure literal (核心设计 §16 N1):
//
//	func(x i32) -> i32 { return x + base }
//	func(c Ctx, req Request) -> (Response, Err) { … }
//	func() { … }
//
// 形参带类型、返回位照写函数签名；与 `x => e` 的区别 = 它**可以按值捕获**外层局部。
func (p *Parser) parseFuncLit() Expr {
	at := p.pos()
	p.next() // func
	lit := &LambdaExpr{Pos: at, Func: true}
	// `func name(...)` 在表达式位 = 把函数声明写进了函数体（v2：函数声明只在顶层）。
	if p.atKind(IDENT) {
		p.errorHere(p.pos(), "a function declaration cannot appear inside a function body", "func "+p.cur().Text,
			"move it to the top level of the file; a func literal in an expression position has no name: func(x i32) -> i32 { ... }")
		p.synchronize(true)
		return lit
	}
	if !p.expect(PUNCT, "(", "a func literal is missing `(` after func") {
		return lit
	}
	for !p.at(PUNCT, ")") && p.cur().Kind != EOF {
		pn := p.pos()
		if !p.atKind(IDENT) {
			p.errorHere(pn, "parameter name is missing or invalid", "got "+describe(p.cur()),
				"a parameter is `name Type`, e.g. `func(x i32) -> i32 { ... }`")
			break
		}
		lit.Params = append(lit.Params, p.next().Text)
		lit.Ptypes = append(lit.Ptypes, p.parseType())
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
	}
	p.expect(PUNCT, ")", "a func literal is missing the closing `)`")
	if p.at(PUNCT, "->") {
		p.next()
		if p.at(PUNCT, "(") && !p.parenGroupFollowedByArrow() {
			p.next()
			for {
				rtAt := p.pos()
				lit.Results = append(lit.Results, Result{Type: p.parseType(), Pos: rtAt}.Type)
				if p.at(PUNCT, ",") {
					p.next()
					continue
				}
				break
			}
			p.expect(PUNCT, ")", "result list is missing the closing `)`")
		} else {
			lit.Results = append(lit.Results, p.parseType())
		}
	}
	if !p.at(PUNCT, "{") {
		p.errorHere(at, "a func literal needs a body", "got "+describe(p.cur()),
			"write func(params) -> R { ... }; the expression form `x => e` takes no types")
		return lit
	}
	lit.Block = p.parseBlock()
	return lit
}

// parsePrimary reads literals, names, and bracketed forms.
func (p *Parser) parsePrimary() Expr {
	at := p.pos()
	t := p.cur()
	switch t.Kind {
	case INT:
		p.next()
		return &IntLit{Text: t.Text, Pos: at}
	case FLOAT:
		p.next()
		return &FloatLit{Text: t.Text, Pos: at}
	case STR:
		p.next()
		if e, handled := p.interpLit(t); handled {
			return e
		}
		return &StrLit{Text: t.Text, Pos: at}
	case RES:
		p.next()
		switch t.Text {
		case "true", "false":
			return &BoolLit{Value: t.Text == "true", Pos: at}
		case "nil":
			return &NilLit{Pos: at}
		}
	case IDENT:
		// 退役词在名字使用位 = 事故（防坑清单 1）：给出定向迁移报错而不是
		// 让它流入后续阶段产生级联误报。
		if isRetiredKeyword(t.Text) {
			p.errorHere(at, retiredDesc(t.Text), "got "+t.Text, retiredFix(t.Text))
			p.next()
			p.synchronize(false)
			return &Ident{Name: "?", Pos: at}
		}
		p.next()
		return &Ident{Name: t.Text, Pos: at}
	case PUNCT:
		switch t.Text {
		case "_": // 裸 `_` = 绑定弃位；是否合法由所在位置决定（stmt/var 目标位）
			p.next()
			return &Ident{Name: "_", Pos: at}
		case "(":
			if p.lambdaHeadAhead() { // (a, b) => e
				return p.parseParenLambda(at)
			}
			p.next()
			saved := p.noComposite
			p.noComposite = 0 // 括号内复合字面量恢复合法：if (C{a:1}.x > 0)
			x := p.parseExpr()
			p.noComposite = saved
			p.expect(PUNCT, ")", "parenthesised expression is missing the closing `)`")
			if p.at(PUNCT, "=>") {
				p.errorHere(nodePos(x), "lambda parameters must be plain names without operators", "got "+describeAt(x)+"'s `=>`",
					"write (a, b) => expression; assign complex arguments to locals first")
			}
			return x
		case "[":
			p.next()
			arr := &ArrayLit{Pos: at}
			if !p.at(PUNCT, "]") {
				for {
					el := p.parseExpr()
					arr.Elems = append(arr.Elems, el)
					if p.at(PUNCT, ",") {
						p.next()
						continue
					}
					break
				}
			}
			p.expect(PUNCT, "]", "list literal is missing the closing `]`")
			return arr
		}
	case KW:
		if t.Text == "this" {
			p.next()
			return &ThisExpr{Pos: at}
		}
		if t.Text == "func" {
			return p.parseFuncLit()
		}
	}
	if t.Kind == ILL {
		// 词法器已报错（报错不重复计因）：吞掉 ILL，向上返回占位表达式。
		p.next()
		return &Ident{Name: "?", Pos: at}
	}
	p.errorHere(at, "expression is missing or incomplete", "got "+describe(t),
		"add an expression: a name, a literal, or a call; keep `()` and `[]` balanced")
	p.synchronize(false)
	return &Ident{Name: "?", Pos: at}
}

// parseParenLambda consumes `(a, b) => body`. The caller has proven via
// lambdaHeadAhead that the parenthesised list is a parameter list.
func (p *Parser) parseParenLambda(at Pos) Expr {
	p.next() // (
	params := []string{}
	for {
		if p.at(PUNCT, "_") {
			p.errorHere(p.pos(), "lambda parameters must be fresh names", "a _",
				"a lambda captures nothing (core design §3); name unused parameters anyway and simply do not reference them")
			p.next()
		} else if p.atKind(IDENT) {
			params = append(params, p.next().Text)
		}
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
		break
	}
	p.expect(PUNCT, ")", "lambda parameter list is missing the closing `)`")
	p.expect(PUNCT, "=>", "lambda is missing `=>`")
	return p.parseLambdaTail(params, at)
}

// --- composite literal gating --------------------------------------------------

// noComposite counts condition contexts (if/for/match subject) where a `{`
// after an expression is the body, never a composite literal (Go 同款规则).
func (p *Parser) enterNoComposite() { p.noComposite++ }
func (p *Parser) leaveNoComposite() { p.noComposite-- }

// sameLine reports whether the `{` sits on the line of the expression it
// would follow. AIC 语句按行分隔：下一行的 `{` 永远开块，不组成字面量。
func (p *Parser) sameLine(e Expr) bool {
	return p.cur().Line == nodePos(e).Line
}

// nodePos gives nodes a uniform way to report their own position.
func nodePos(e Expr) Pos {
	switch v := e.(type) {
	case *Ident:
		return v.Pos
	case *IntLit:
		return v.Pos
	case *FloatLit:
		return v.Pos
	case *StrLit:
		return v.Pos
	case *InterpLit:
		return v.Pos
	case *BoolLit:
		return v.Pos
	case *NilLit:
		return v.Pos
	case *ThisExpr:
		return v.Pos
	case *Unary:
		return v.Pos
	case *Binary:
		return v.Pos
	case *Assign:
		return v.Pos
	case *MultiAssign:
		return v.Pos
	case *Call:
		return v.Pos
	case *Index:
		return v.Pos
	case *Field:
		return v.Pos
	case *CheckExpr:
		return v.Pos
	case *OrExpr:
		return v.Pos
	case *CatchExpr:
		return v.Pos
	case *AssertExpr:
		return v.Pos
	case *ArrayLit:
		return v.Pos
	case *CompositeLit:
		return v.Pos
	case *LambdaExpr:
		return v.Pos
	case *MatchExpr:
		return v.Pos
	}
	return Pos{}
}
