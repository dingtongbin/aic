package parse

// ---------------------------------------------------------------------------
// N6 `select` / `scope.timeout`（核心设计 §16 N6）。
//
//	select {
//	    case v, ok = ch.recv() { … }
//	    case ch.send(v) { … }
//	    case time.after(ms) { … }
//	}
//	scope.timeout(ms) { … }
//
// 语法要点：
//   - `select` 与 `case` 都是**上下文关键字**（标识符身份，H7 的 23 关键字不变）；
//   - 分支头 = 「绑定目标 = 调用」或「调用」：前者是 recv 形，后者按被调名判定
//     （`x.recv()` = 收，`x.send(v)` = 发，`time.after(ms)` = 超时）；
//   - 时间分支写作 `case time.after(ms) { … }`（**不用 `<-`**：AIC 没有通道运算符，
//     多一个记号只为一种形态不值当；语义与设计 §16 N6 完全一致）。
// ---------------------------------------------------------------------------

// atCase 报告游标是否在上下文关键字 `case` 上。
func (p *Parser) atCase() bool { return p.atIdent("case") }

// parseSelect 读 `select { case … }`。
func (p *Parser) parseSelect(at Pos) Stmt {
	p.next() // select
	s := &SelectStmt{Pos: at}
	if !p.expect(PUNCT, "{", "select is missing `{`") {
		return s
	}
	for {
		p.skipSemis()
		if p.at(PUNCT, "}") || p.cur().Kind == EOF {
			break
		}
		if !p.atCase() {
			p.errorCur("a select body holds `case` arms only", "got "+describe(p.cur()),
				"write case ch.recv() { … } / case ch.send(v) { … } / case time.after(ms) { … } (core design §16 N6)")
			p.synchronize(true)
			continue
		}
		armAt := p.pos()
		p.next() // case
		arm := SelectArm{Pos: armAt}
		// 分支头形：`名字 [, 名字] = <调用>`（绑定形）或 `<调用>`（表达式形）。
		// 判定用前瞻：单个名字后面紧跟 `=`（不是 `==`）或 `,` 才是绑定形 ——
		// 否则那个名字属于表达式（如 `time.after(ms)` 的 `time`）。
		if p.bindingAhead() {
			for {
				tgt, ok := p.parseRangeName()
				if !ok {
					break
				}
				arm.Targets = append(arm.Targets, tgt)
				if p.at(PUNCT, ",") {
					p.next()
					continue
				}
				break
			}
			p.expect(PUNCT, "=", "a select binding needs `=` before the call")
		}
		arm.Expr = p.parseExpr()
		arm.Kind = selectArmKind(arm)
		if !p.at(PUNCT, "{") {
			p.errorCur("a select arm needs a block", "got "+describe(p.cur()),
				"write case ch.recv() { … }; every arm is a block (core design §16 N6)")
			p.synchronize(true)
			continue
		}
		arm.Block = p.parseBlock()
		s.Arms = append(s.Arms, arm)
	}
	p.expect(PUNCT, "}", "select is missing the closing `}`")
	if len(s.Arms) == 0 {
		p.errorHere(at, "select needs at least one arm", "select is empty",
			"write case ch.recv() { … } or case time.after(ms) { … } (core design §16 N6)")
	}
	return s
}

// selectArmKind 按被调名判定分支种类（检查器做最终裁决：类型不对就是编译错）。
func selectArmKind(arm SelectArm) string {
	call, ok := arm.Expr.(*Call)
	if !ok {
		return ""
	}
	field, ok := call.Fn.(*Field)
	if !ok {
		return ""
	}
	if id, isID := field.X.(*Ident); isID && id.Name == "time" && field.Name == "after" {
		return "time"
	}
	switch field.Name {
	case "recv":
		return "recv"
	case "send":
		return "send"
	}
	return ""
}

// bindingAhead 报告分支头是否是绑定形：单个名字后紧跟 `=`（非 `==`）或 `,`。
// 纯前瞻，游标不动。
func (p *Parser) bindingAhead() bool {
	if !p.atKind(IDENT) && !p.at(PUNCT, "_") {
		return false
	}
	j := min(p.i+1, len(p.toks)-1)
	nx := p.toks[j]
	if nx.Kind != PUNCT {
		return false
	}
	return nx.Text == "=" || nx.Text == ","
}

// parseScopeTail 处理 `scope { … }` 与 `scope.timeout(ms) { … }`。
func (p *Parser) parseScopeTail(at Pos) Stmt {
	if p.at(PUNCT, ".") && p.peekText() == "timeout" {
		p.next() // .
		p.next() // timeout
		s := &ScopeStmt{Pos: at, IsTimeout: true}
		if p.expect(PUNCT, "(", "scope.timeout is missing `(`") {
			p.enterNoComposite()
			s.Timeout = p.parseExpr()
			p.leaveNoComposite()
			p.expect(PUNCT, ")", "scope.timeout is missing the closing `)`")
		}
		p.scopeDepth++
		s.Body = p.parseBlock()
		p.scopeDepth--
		return s
	}
	p.scopeDepth++
	body := p.parseBlock()
	p.scopeDepth--
	return &ScopeStmt{Body: body, Pos: at}
}
