package parse

// ---------------------------------------------------------------------------
// N3 泛型形参表（核心设计 §16）：`[T]` / `[T: Ord]` / `[T: Hash + Eq, U: Shape]` /
// `[T: value]` / `[T: ref]`。
//
// 语法要点：
//   - 约束写在形参名之后，冒号分隔；多个约束用 `+` 连接；
//   - 约束名是普通标识符（能力名 / 接口名 / value / ref），解析层不裁决其含义
//     （检查器才是唯一裁决者，这样诊断能落在声明处）；
//   - 形参之间仍用 `,` 分隔 —— 与 `+` 不冲突。
// ---------------------------------------------------------------------------

// parseTypeParamList 读 `[ ... ]` 里的泛型形参表，返回名字表与约束表。
func (p *Parser) parseTypeParamList(what string) ([]string, []TypeParamConstraint) {
	var names []string
	var cons []TypeParamConstraint
	if !p.expect(PUNCT, "[", what+" is missing `[`") {
		return names, cons
	}
	for {
		if !p.atKind(IDENT) {
			p.errorCur("generic parameter name is missing", "got "+describe(p.cur()),
				"generics look like `"+what+"[T]` or `"+what+"[T: Ord]`; a parameter name starts with a letter")
			break
		}
		nameAt := p.pos()
		name := p.next().Text
		names = append(names, name)
		tpc := TypeParamConstraint{Name: name, Pos: nameAt}
		if p.at(PUNCT, ":") {
			p.next()
			for {
				if !p.atKind(IDENT) {
					p.errorCur("constraint name is missing after `:`", "got "+describe(p.cur()),
						"write the constraint as a capability (Eq/Ord/Hash/Print/Zero), an interface name, or value/ref (core design §16 N3)")
					break
				}
				cAt := p.pos()
				cname := p.next().Text
				// 跨包接口约束：pkg.Shape
				if p.at(PUNCT, ".") && p.peekKind() == IDENT {
					p.next()
					cname = cname + "." + p.next().Text
				}
				tpc.Constraints = append(tpc.Constraints, cname)
				_ = cAt
				if p.at(PUNCT, "+") {
					p.next()
					continue
				}
				break
			}
		}
		cons = append(cons, tpc)
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
		break
	}
	p.expect(PUNCT, "]", "generic parameter list is missing the closing `]`")
	return names, cons
}

// constraintsOf 取某个泛型形参的约束列表（无约束 = nil）。
func constraintsOf(cons []TypeParamConstraint, name string) []string {
	for i := range cons {
		if cons[i].Name == name {
			return cons[i].Constraints
		}
	}
	return nil
}
