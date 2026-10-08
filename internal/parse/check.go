package parse

// match 模式与 check 位置规则（核心设计 §三/§六）。

// parsePatterns reads the single pattern of a match arm. 模式一层、无通配符
// (核心设计 §三)；`|` 或模式不在 v2 表面，遇到即定向报错（更严方向）。
func (p *Parser) parsePatterns() []MatchPattern {
	pat := p.parsePattern()
	if p.at(PUNCT, "|") {
		p.errorHere(p.pos(), "`|` and or-patterns are not part of the v2 surface", "`|` after a pattern",
			"give every pattern its own arm: A => ..., B => ... (exhaustiveness already requires one arm per variant)")
		for p.at(PUNCT, "|") {
			p.next()
			p.parsePattern()
		}
	}
	return []MatchPattern{pat}
}

func (p *Parser) parsePattern() MatchPattern {
	at := p.pos()
	t := p.cur()
	if t.Kind != IDENT {
		if t.Kind == KW || t.Kind == RES {
			p.errorHere(at, "invalid match pattern", "got "+describe(t),
				"a pattern is an enum variant name or a payload binding such as Some(x); true/false/nil are not patterns")
		} else {
			p.errorHere(at, "invalid match pattern", "got "+describe(t),
				"a pattern is an enum variant name or a payload binding such as Some(x); wildcards and guards are not allowed (exhaustiveness is the point)")
		}
		p.next()
		return MatchPattern{Kind: "value", Name: "?", Pos: at}
	}
	p.next()
	mp := MatchPattern{Kind: "value", Name: t.Text, Pos: at}
	// 限定写法 Color.Red 与裸变体名 Red 等价（解析层不裁决，检查器按主体枚举解析）。
	if p.at(PUNCT, ".") && p.peekKind() == IDENT {
		p.next()
		mp.Name = t.Text + "." + p.next().Text
	}
	if p.at(PUNCT, "(") { // 带负载变体：Some(binding) — 绑定名 = 分支新声明
		p.next()
		pat := p.pos()
		if p.at(PUNCT, "_") {
			p.next()
			mp.Binding = "_"
		} else if p.atKind(IDENT) {
			mp.Binding = p.next().Text
		} else {
			p.errorHere(pat, "a pattern payload must be a single binding name", "got "+describe(p.cur()),
				"write Some(x) to bind a new name x; to compare against a literal, bind first and test afterwards")
		}
		mp.Kind = "payload"
		p.expect(PUNCT, ")", "pattern payload is missing the closing `)`")
	}
	return mp
}

// hasCheckExpr reports whether e contains a check expression at any depth —
// const 初始化禁 check (核心设计 §六).
func hasCheckExpr(e Expr) bool {
	found := false
	walkCheckCollect(e, &found)
	return found
}

func walkCheckCollect(e Expr, found *bool) {
	if e == nil || *found {
		return
	}
	switch v := e.(type) {
	case *CheckExpr:
		*found = true
	case *OrExpr:
		walkCheckCollect(v.X, found)
		walkCheckCollect(v.Default, found)
	case *CatchExpr:
		walkCheckCollect(v.X, found)
	case *AssertExpr:
		walkCheckCollect(v.X, found)
	case *Call:
		walkCheckCollect(v.Fn, found)
		for _, a := range v.Args {
			walkCheckCollect(a, found)
		}
	case *Unary:
		walkCheckCollect(v.X, found)
	case *Binary:
		walkCheckCollect(v.Left, found)
		walkCheckCollect(v.Right, found)
	case *Index:
		walkCheckCollect(v.X, found)
		walkCheckCollect(v.Index, found)
		if v.End != nil {
			walkCheckCollect(v.End, found)
		}
	case *Field:
		walkCheckCollect(v.X, found)
	case *ArrayLit:
		for _, el := range v.Elems {
			walkCheckCollect(el, found)
		}
	case *InterpLit:
		for _, el := range v.Values {
			walkCheckCollect(el, found)
		}
	case *CompositeLit:
		for _, f := range v.Fields {
			walkCheckCollect(f.Value, found)
		}
	}
}

// validateBlankValues rejects `_` outside binding-target positions: `_` 不是
// 值 (核心设计 §六). inTarget marks the LHS targets of a multi-assign.
func (p *Parser) validateBlankValues(e Expr, inTarget bool) {
	if e == nil {
		return
	}
	switch v := e.(type) {
	case *Ident:
		if v.Name == "_" && !inTarget {
			p.errorHere(v.Pos, "`_` cannot be used as a value (it only discards a binding)", "a standalone _",
				"discard a multi-return slot in the target list: var a, _ = f(); to drop a single result, call it as a statement: f()")
		}
	case *Assign:
		if id, ok := v.LHS.(*Ident); ok && id.Name == "_" {
			p.errorHere(id.Pos, "`_` cannot be a standalone assignment target", "here: _ "+v.Op+" ...",
				"discard a multi-return slot with a, _ = f(); to drop a single result, call it as a statement: f()")
		} else {
			p.validateBlankValues(v.LHS, false)
		}
		p.validateBlankValues(v.Right, false)
	case *MultiAssign:
		for _, t := range v.LHS {
			p.validateBlankValues(t, true)
		}
		p.validateBlankValues(v.Right, false)
	case *Unary:
		p.validateBlankValues(v.X, false)
	case *Binary:
		p.validateBlankValues(v.Left, false)
		p.validateBlankValues(v.Right, false)
	case *Call:
		p.validateBlankValues(v.Fn, false)
		for _, a := range v.Args {
			p.validateBlankValues(a, false)
		}
	case *Index:
		p.validateBlankValues(v.X, false)
		p.validateBlankValues(v.Index, false)
		if v.End != nil {
			p.validateBlankValues(v.End, false)
		}
	case *Field:
		p.validateBlankValues(v.X, false)
	case *ArrayLit:
		for _, el := range v.Elems {
			p.validateBlankValues(el, false)
		}
	case *InterpLit:
		for _, el := range v.Values {
			p.validateBlankValues(el, false)
		}
	case *CompositeLit:
		for _, f := range v.Fields {
			p.validateBlankValues(f.Value, false)
		}
	case *CheckExpr:
		p.validateBlankValues(v.X, false)
	case *OrExpr:
		p.validateBlankValues(v.X, false)
		p.validateBlankValues(v.Default, false)
	case *CatchExpr:
		p.validateBlankValues(v.X, false)
		if v.Block != nil {
			for _, s := range v.Block.Stmts {
				if es, ok := s.(*ExprStmt); ok {
					p.validateBlankValues(es.X, false)
				}
			}
		}
	case *AssertExpr:
		p.validateBlankValues(v.X, false)
	case *MatchExpr:
		p.validateBlankValues(v.Subject, false)
		for _, arm := range v.Arms {
			p.validateBlankValues(arm.Value, false)
		}
	case *LambdaExpr:
		p.validateBlankValues(v.Body, false)
	}
}

// validateCheckPosition enforces the v2 check position rule (核心设计 §六):
// `check` legal at the outermost position of a var initializer or a standalone
// statement, and anywhere inside a lambda body (check 在 lambda 内 = 从
// lambda 返回). Nested inside a larger expression otherwise = 编译错误.
func (p *Parser) validateCheckPosition(root Expr, declAt Pos) {
	p.walkCheck(root, 0, false, declAt)
}

// walkCheck walks the expression tree; depth 0 means the outermost position.
// inLambda records that the walk is inside a lambda body.
func (p *Parser) walkCheck(e Expr, depth int, inLambda bool, declAt Pos) {
	if e == nil {
		return
	}
	switch v := e.(type) {
	case *CheckExpr:
		// 后缀形 `expr?` 位置无关（推荐写法）；前缀 `check` 才受位置规则约束。
		if depth > 0 && !inLambda && !v.Postfix {
			p.errorHere(v.Pos, "check may only appear in a var initialiser, a statement of its own, or at the top level of a lambda body",
				"check nested inside an expression", "bind the result first: var x = check f(), then use x in the expression; or use the postfix form f()?, which is position independent")
		}
		p.walkCheck(v.X, depth, inLambda, declAt)
	case *OrExpr:
		p.walkCheck(v.X, depth, inLambda, declAt)
		p.walkCheck(v.Default, depth, inLambda, declAt)
	case *CatchExpr:
		p.walkCheck(v.X, depth, inLambda, declAt)
		if v.Block != nil {
			p.walkBlockCheck(v.Block, declAt)
		}
	case *AssertExpr:
		p.walkCheck(v.X, depth, inLambda, declAt)
	case *Call:
		for _, a := range v.Args {
			p.walkCheck(a, depth+1, inLambda, declAt)
		}
		p.walkCheck(v.Fn, depth, inLambda, declAt)
	case *Binary:
		p.walkCheck(v.Left, depth+1, inLambda, declAt)
		p.walkCheck(v.Right, depth+1, inLambda, declAt)
	case *Unary:
		p.walkCheck(v.X, depth+1, inLambda, declAt)
	case *Assign:
		p.walkCheck(v.LHS, depth+1, inLambda, declAt)
		p.walkCheck(v.Right, depth+1, inLambda, declAt)
	case *MultiAssign:
		for _, t := range v.LHS {
			p.walkCheck(t, depth+1, inLambda, declAt)
		}
		p.walkCheck(v.Right, depth+1, inLambda, declAt)
	case *Index:
		p.walkCheck(v.X, depth+1, inLambda, declAt)
		p.walkCheck(v.Index, depth+1, inLambda, declAt)
		if v.End != nil {
			p.walkCheck(v.End, depth+1, inLambda, declAt)
		}
	case *Field:
		p.walkCheck(v.X, depth+1, inLambda, declAt)
	case *ArrayLit:
		for _, el := range v.Elems {
			p.walkCheck(el, depth+1, inLambda, declAt)
		}
	case *InterpLit:
		for _, el := range v.Values {
			p.walkCheck(el, depth+1, inLambda, declAt)
		}
	case *CompositeLit:
		for _, f := range v.Fields {
			p.walkCheck(f.Value, depth+1, inLambda, declAt)
		}
	case *LambdaExpr:
		if v.Body != nil {
			p.walkCheck(v.Body, depth+1, true, declAt)
		}
		if v.Block != nil {
			p.walkBlockCheck(v.Block, declAt)
		}
	case *MatchExpr:
		p.walkCheck(v.Subject, depth+1, inLambda, declAt)
		for _, arm := range v.Arms {
			if arm.Value != nil {
				p.walkCheck(arm.Value, depth+1, inLambda, declAt)
			}
		}
	}
}

// walkBlockCheck walks statements inside lambda blocks for nested checks.
func (p *Parser) walkBlockCheck(b *Block, declAt Pos) {
	for _, s := range b.Stmts {
		switch v := s.(type) {
		case *ExprStmt:
			p.walkCheck(v.X, 0, true, declAt)
		case *VarDecl:
			if v.Init != nil {
				p.walkCheck(v.Init, 0, true, declAt)
			}
		case *ReturnStmt:
			for _, r := range v.Results {
				p.walkCheck(r, 0, true, declAt)
			}
		case *Block:
			p.walkBlockCheck(v, declAt)
		}
	}
}
