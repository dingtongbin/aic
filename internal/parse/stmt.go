package parse

// parseBlock reads `{ stmts }`.
func (p *Parser) parseBlock() *Block {
	at := p.pos()
	if !p.expect(PUNCT, "{", "statement block is missing `{`") {
		return &Block{Pos: at}
	}
	b := &Block{Pos: at}
	for {
		p.skipSemis()
		if p.at(PUNCT, "}") || p.cur().Kind == EOF {
			break
		}
		before := p.i
		s := p.parseStmt()
		if s != nil {
			b.Stmts = append(b.Stmts, s)
		}
		if p.i == before {
			p.next()
			p.synchronize(true)
		}
	}
	p.expect(PUNCT, "}", "statement block is missing the closing `}`")
	return b
}

// parseStmt reads one statement. The dispatch covers every statement keyword
// of the surface (核心设计 §三); everything else reaches the expression path
// as a plain identifier (《AIC 语言规范》§1.3).
func (p *Parser) parseStmt() Stmt {
	at := p.pos()
	switch {
	// A bare `{ … }` is a nested block.
	case p.at(PUNCT, "{"):
		return p.parseBlock()

	case p.at(KW, "var"):
		d := p.parseVar(false)
		if d == nil {
			p.synchronize(true)
			return nil
		}
		return d

	case p.at(KW, "return"):
		p.next()
		r := &ReturnStmt{Pos: at}
		if !p.at(PUNCT, "}") && !p.at(PUNCT, ";") && !p.endOfStmt() {
			first := p.parseExpr()
			r.Results = []Expr{first}
			for p.at(PUNCT, ",") { // `return a, b` — 多返回值形
				p.next()
				r.Results = append(r.Results, p.parseExpr())
			}
		}
		return r

	case p.at(KW, "break"), p.at(KW, "continue"):
		kw := p.next().Text
		return &BranchStmt{Keyword: kw, Pos: at}

	case p.at(KW, "if"):
		return p.parseIf()

	case p.at(KW, "for"):
		return p.parseFor()

	case p.at(KW, "match"):
		return p.parseMatchStmt()

	case p.at(KW, "defer"):
		p.next()
		d := &DeferStmt{Pos: at}
		if p.at(PUNCT, "{") { // defer { … } 块形（核心设计 §三）
			d.Block = p.parseBlock()
			return d
		}
		d.Call = p.parseExpr()
		if d.Call != nil {
			p.validateCheckPosition(d.Call, at)
		}
		return d

	case p.at(KW, "errdefer"):
		// errdefer 是硬关键字（上下文词归零，《AIC 语言规范》§1.3）。
		p.next()
		d := &ErrDeferStmt{Pos: at}
		if p.at(PUNCT, "{") {
			d.Block = p.parseBlock()
			return d
		}
		d.Call = p.parseExpr()
		if d.Call != nil {
			p.validateCheckPosition(d.Call, at)
		}
		return d

	case p.at(KW, "region"):
		p.next()
		return &RegionStmt{Body: p.parseBlock(), Pos: at}

	case p.at(KW, "spawn"):
		return p.parseSpawn(at)

	case p.at(KW, "scope"):
		p.next()
		return p.parseScopeTail(at)

	case p.at(KW, "live"):
		// R20：`live` 是变量目标修饰语，不是语句。定向报错而不是
		// "unexpected token"（防坑清单：错误必须可照做）。
		p.errorHere(at, "live is a variable modifier, not a statement",
			"got live",
			"write a declaration: var live x = C{...} (live raises the allocation to the outer region, core design §5 R4)")
		p.synchronize(true)
		return nil

	case p.at(KW, "select"):
		// select 是硬关键字（上下文词归零）。
		return p.parseSelect(at)
	}

	// Top-level modifiers are not statements.
	if p.at(KW, "export") || p.at(KW, "extern") || p.at(KW, "import") ||
		p.at(KW, "class") || p.at(KW, "interface") || p.at(KW, "enum") || p.at(KW, "const") {
		at := p.pos()
		p.errorHere(at, "a top-level declaration cannot appear inside a function body", "got "+describe(p.cur()),
			"move it to the top level of the file; a function body holds statements only")
		p.synchronize(true)
		return nil
	}

	// Expression or assignment statement.
	x := p.parseExpr()
	if x != nil {
		p.validateCheckPosition(x, at)
		p.validateBlankValues(x, false)
	}
	if p.at(PUNCT, ":") && p.sameLine(x) {
		p.errorHere(p.pos(), "this looks like a composite literal, but a composite literal cannot appear in a condition (it is ambiguous with the block body)",
			"got "+describeAt(x)+"'s `:`",
			"assign the literal to a local first: var c = C{field: value}, then write the condition")
		p.next()
		p.synchronize(true)
		return nil
	}
	return &ExprStmt{X: x, Pos: at}
}

// endOfStmt reports whether the current token ends a statement implicitly.
func (p *Parser) endOfStmt() bool {
	t := p.cur()
	if t.Kind == "EOF" {
		return true
	}
	if t.Kind != "PUNCT" {
		return false
	}
	switch t.Text {
	case ")", "]", "}", ",", ";", "=>":
		return true
	}
	return false
}

// parseIf reads if / else if / else. `else if` 是 else 块内的单个 IfStmt；
// 递归下降天然实现「else 配对最近未配对的 if」（核心设计 §三）。
func (p *Parser) parseIf() Stmt {
	at := p.pos()
	p.next() // if
	p.enterNoComposite()
	cond := p.parseExpr()
	p.leaveNoComposite()
	s := &IfStmt{Cond: cond, Then: p.parseBlock(), Pos: at}
	if p.at(KW, "else") {
		elseAt := p.pos()
		p.next()
		if p.at(KW, "if") { // else if → 嵌套
			inner := p.parseIf()
			s.Else = &Block{Stmts: []Stmt{inner}, Pos: elseAt}
		} else {
			s.Else = p.parseBlock()
		}
	}
	return s
}

// parseFor reads the three loop forms (核心设计 §三):
//
//	for cond { } / for init; cond; post { } / for i, x in xs { } / for i in a..b { }
func (p *Parser) parseFor() Stmt {
	at := p.pos()
	p.next() // for
	switch p.forForm() {
	case forRange:
		var names []VarTarget
		for {
			tgt, ok := p.parseRangeName()
			if ok {
				names = append(names, tgt)
			}
			if p.at(PUNCT, ",") {
				p.next()
				continue
			}
			break
		}
		if len(names) == 0 {
			p.errorHere(at, "for-in is missing its loop variable", "for",
				"write `for x in xs`, `for i, x in xs`, or `for i in a..b`")
		}
		if !p.at(KW, "in") {
			p.errorCur("for-in is missing `in`", "got "+describe(p.cur()),
				"write `in` after the loop variables: for i, x in xs { ... }")
		} else {
			p.next()
		}
		f := &ForStmt{IsRange: true, Names: names, Pos: at}
		p.enterNoComposite()
		f.RangeX = p.parseExpr()
		if p.at(PUNCT, "..") { // for i in a..b（半开 [a, b)）
			p.next()
			f.RangeEnd = p.parseExpr()
		}
		p.leaveNoComposite()
		f.Body = p.parseBlock()
		return f
	case forC:
		var init Stmt
		if !p.at(PUNCT, ";") {
			init = p.parseSimpleStmtNoBlock()
		}
		p.expect(PUNCT, ";", "three-part for is missing its first `;`")
		var cond Expr
		if !p.at(PUNCT, ";") {
			p.enterNoComposite()
			cond = p.parseExpr()
			p.leaveNoComposite()
		}
		p.expect(PUNCT, ";", "three-part for is missing its second `;`")
		var post Stmt
		if !p.at(PUNCT, "{") {
			post = p.parseSimpleStmtNoBlock()
		}
		return &ForStmt{Init: init, Cond: cond, Post: post, Body: p.parseBlock(), Pos: at}
	default: // for cond { }
		var cond Expr
		if !p.at(PUNCT, "{") {
			p.enterNoComposite()
			cond = p.parseExpr()
			p.leaveNoComposite()
		}
		return &ForStmt{Cond: cond, Body: p.parseBlock(), Pos: at}
	}
}

// parseRangeName reads one for-in binding: a name or `_`.
func (p *Parser) parseRangeName() (VarTarget, bool) {
	at := p.pos()
	if p.at(PUNCT, "_") {
		p.next()
		return VarTarget{Name: "_", Blank: true, Pos: at}, true
	}
	if !p.atKind(IDENT) {
		return VarTarget{Pos: at}, false
	}
	return VarTarget{Name: p.next().Text, Pos: at}, true
}

type forForm int

const (
	forWhile forForm = iota
	forC
	forRange
)

// forForm inspects upcoming tokens at bracket depth zero: a name list followed
// by `in` means range form, a `;` before the body means the three-part form,
// and a `{` at depth 0 means the condition form — the body begins there, so
// the scan must not look past it (修复：v1 扫描会穿透循环体误判后续语句的 `;`).
func (p *Parser) forForm() forForm {
	depth := 0
	for j := p.i; j < len(p.toks); j++ {
		t := p.toks[j]
		if t.Kind == EOF {
			break
		}
		if t.Kind == PUNCT {
			switch t.Text {
			case "(", "[":
				depth++
			case ")", "]":
				if depth > 0 {
					depth--
				}
			case "{":
				if depth == 0 {
					return forWhile // 体开始：条件已完整
				}
				depth++
			case "}":
				if depth == 0 {
					return forWhile // 防御：结构错乱时不越过外层块
				}
				depth--
			case ";":
				if depth == 0 {
					return forC
				}
			}
			continue
		}
		if depth == 0 && t.Kind == KW && t.Text == "in" && j > p.i && p.isNameToken(p.toks[j-1]) {
			return forRange
		}
	}
	return forWhile
}

func (p *Parser) isNameToken(t Token) bool {
	return t.Kind == IDENT || (t.Kind == PUNCT && t.Text == "_")
}

// parseSimpleStmtNoBlock parses a statement that is not a block-bodied form.
func (p *Parser) parseSimpleStmtNoBlock() Stmt {
	at := p.pos()
	if p.at(KW, "var") {
		return p.parseVar(false)
	}
	if p.at(KW, "spawn") {
		return p.parseSpawn(at)
	}
	x := p.parseExpr()
	return &ExprStmt{X: x, Pos: at}
}

// parseSpawn reads `spawn f(args)` 以及 `spawn f(args) into ch`（《AIC 语言规范》§9.1）。
// into 子句把任务返回值包装成 Result[T] 送进 ch —— 这是 E-9"失败通道标准形态
// = chan[Result[T]]"的落地语法；无 into = 结果丢弃。
// 词法级强制 scope 包裹。
func (p *Parser) parseSpawn(at Pos) Stmt {
	p.next() // spawn
	if p.scopeDepth == 0 {
		p.errorHere(at, "spawn may only appear inside `scope { }`",
			"outside any scope", "wrap the spawn in a scope: scope { spawn f() ... } (the scope joins on exit)")
	}
	x := p.parseExpr()
	s := &SpawnStmt{Call: x, Pos: at}
	// `into ch`：into 是硬关键字，紧跟 spawn 的调用表达式。
	if p.at(KW, "into") {
		p.next()
		s.Into = p.parseExpr()
	}
	return s
}

// parseMatchStmt reads the statement-position match (核心设计 §三):
// subject { pattern => { … } / pattern => expr(,)? } — 强制穷尽由检查器执行。
func (p *Parser) parseMatchStmt() Stmt {
	at := p.pos()
	p.next() // match
	p.enterNoComposite()
	subject := p.parseExpr()
	p.leaveNoComposite()
	s := &MatchStmt{Subject: subject, Pos: at}
	if !p.expect(PUNCT, "{", "match is missing `{`") {
		p.synchronize(false)
		return s
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
		arm := MatchArm{Patterns: patterns, Pos: armAt}
		if p.at(PUNCT, "{") { // 块形分支
			arm.Block = p.parseBlock()
		} else {
			arm.Value = p.parseExpr()
		}
		s.Arms = append(s.Arms, arm)
		// 表达式形分支之间必须有 `,`；块形分支 `,` 可选。
		if arm.Block == nil && !p.at(PUNCT, ",") && !p.at(PUNCT, "}") {
			p.errorCur("match expression arms need `,` between them", "got "+describe(p.cur()),
				"separate expression arms with commas: match e { A => 1, B => 2 }")
			p.synchronize(true)
		}
	}
	p.expect(PUNCT, "}", "match is missing the closing `}`")
	if len(s.Arms) == 0 {
		p.errorHere(at, "match needs at least one arm", "match "+describeAt(subject)+" is empty",
			"write every enum variant: match x { A => { ... } B => { ... } }")
	}
	return s
}

func describeAt(e Expr) string {
	switch v := e.(type) {
	case *Ident:
		return v.Name
	case *Field:
		return describeAt(v.X) + "." + v.Name
	}
	return "expression"
}
