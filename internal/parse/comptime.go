package parse

// ---------------------------------------------------------------------------
// N4 编译期反射的**语言级落点**（核心设计 §16 N4）：
//
//	comptime {
//	    const SIZE = sizeOf(Point)
//	    assert SIZE == 16
//	    const N = fieldsOf(Point).len
//	    assert N == 3
//	}
//
// 要点：
//   - `comptime` 与 `assert` 都是**上下文关键字**（标识符身份，H7 的 23 关键字不变）；
//   - 体里只有两种语句：`const 名 = <可折叠表达式>` 与 `assert <可折叠 bool>`；
//   - 整块在编译期求值，**不生成任何运行期代码**（emit/air 按未知 Decl 跳过）——
//     这是「运行期零元数据」承诺的语言面表达；
//   - 常量的作用域 = **包级**（与普通 `const` 同一张表、同一套重名检查），
//     所以 `comptime { const N = … }` 之后的代码可以直接用 `N`。
// ---------------------------------------------------------------------------

// parseComptimeBlock 读包级 `comptime { … }`。
func (p *Parser) parseComptimeBlock() Decl {
	at := p.pos()
	p.next() // comptime
	blk := &ComptimeBlock{Pos: at}
	if !p.expect(PUNCT, "{", "comptime is missing `{`") {
		return blk
	}
	reported := false
	for {
		p.skipSemis()
		if p.at(PUNCT, "}") || p.cur().Kind == EOF {
			break
		}
		switch {
		case p.at(KW, "const"):
			if d := p.parseConst(nil); d != nil {
				blk.Body = append(blk.Body, d)
			}
		case p.atIdent("assert"):
			a := &ComptimeAssert{Pos: p.pos()}
			p.next() // assert
			a.Cond = p.parseExpr()
			blk.Body = append(blk.Body, a)
		default:
			line := p.pos().Line
			p.errorCur("a comptime block holds `const` declarations and `assert` statements only",
				"got "+describe(p.cur()),
				"write const NAME = <foldable expression> or assert <foldable bool> (core design §16 N4)")
			reported = true
			// 体里不用分号 ⇒ **换行即语句边界**：只跳过本行，别的错行各自报一条，
			// 既不刷屏也不吞掉后面的真错（曾用 synchronize 一次性吞到 `}`）。
			for p.cur().Kind != EOF && !p.at(PUNCT, "}") && p.pos().Line == line {
				p.next()
			}
		}
	}
	p.expect(PUNCT, "}", "comptime is missing the closing `}`")
	if len(blk.Body) == 0 && !reported {
		p.errorHere(at, "an empty comptime block does nothing", "comptime { }",
			"add a const declaration or an assert, or delete the block (core design §16 N4)")
	}
	return blk
}
