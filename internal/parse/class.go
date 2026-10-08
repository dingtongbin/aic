package parse

import "strings"

// parseFunc reads a function or method. recv is empty for free functions.
// v2：方法一律可读写 this，无 `var func` 之分（核心设计 §四）。
func (p *Parser) parseFunc(recv string, inClass bool, export, extern bool, annotations []string, bodyOptional bool) Decl {
	at := p.pos()
	p.next() // func
	d := &FuncDecl{
		Recv:        recv,
		Export:      export,
		Extern:      extern,
		Annotations: annotations,
		Pos:         at,
	}
	// `init` is a method name, not one of the 23 frozen keywords (核心设计 §一),
	// so it arrives as an IDENT. init：至多一个、无重载（checker 执行）。
	if inClass && p.atKind(IDENT) && p.cur().Text == "init" {
		d.IsInit = true
		d.Name = "init"
		p.next()
	} else if !p.atKind(IDENT) {
		p.errorHere(at, "function is missing a name", "got "+describe(p.cur()),
			"write `func name(...)`; methods live inside a class body and start with `func`")
		p.synchronize(false)
		return nil
	} else {
		d.Name = p.next().Text
	}

	// Generic type parameters: func f[T](...) / func f[T: Ord](...)
	if p.at(PUNCT, "[") {
		d.TypeParams, d.Constraints = p.parseTypeParamList("a generic function")
	}

	p.expect(PUNCT, "(", "function parameter list is missing `(`")
	for !p.at(PUNCT, ")") && p.cur().Kind != EOF {
		pn := p.pos()
		if !p.atKind(IDENT) {
			p.errorHere(pn, "parameter name is missing or invalid", "got "+describe(p.cur()),
				"a parameter is `name Type`, e.g. `func f(p i32)`")
			break
		}
		pn = p.pos()
		name := p.next().Text
		ty := p.parseType()
		d.Params = append(d.Params, Param{Name: name, Type: ty, Pos: pn})
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
	}
	p.expect(PUNCT, ")", "function parameter list is missing the closing `)`")

	// Return shape: none / single / `(A, B, Err)` 列表。Err must be last (红线 2).
	// 注意 `-> (i32) -> i32`：括号可能是**函数类型的形参表**，不是结果列表 ——
	// 判定 = 配对 `)` 之后紧跟 `->`（此时按单个类型解析）。
	if p.at(PUNCT, "->") {
		p.next()
		if p.at(PUNCT, "(") && !p.parenGroupFollowedByArrow() {
			p.next()
			for {
				rtAt := p.pos()
				rt := p.parseType()
				d.Results = append(d.Results, Result{Type: rt, Pos: rtAt})
				if p.at(PUNCT, ",") {
					p.next()
					continue
				}
				break
			}
			p.expect(PUNCT, ")", "result list is missing the closing `)`")
		} else {
			rtAt := p.pos()
			rt := p.parseType()
			d.Results = append(d.Results, Result{Type: rt, Pos: rtAt})
		}
		p.checkErrLast(d.Results, at)
	}

	if extern {
		// extern bindings never carry a body; `export` 与 `extern` 同时出现 =
		// 方向矛盾（extern 导入 C，export 导出给 C）。
		if export {
			p.errorHere(at, "export and extern cannot be combined", "export extern func "+d.Name,
				"import a C function with extern func; export one to C with export func")
		}
		return d
	}
	if p.at(PUNCT, "{") {
		d.Body = p.parseBlock()
	} else if !bodyOptional {
		p.errorHere(at, "function is missing its body", "got "+describe(p.cur()),
			"add `{ ... }`; only extern bindings and interface method signatures may omit the body")
		p.synchronize(false)
	}
	return d
}

// checkErrLast enforces 红线 2 at parse time: Err only in the final slot.
func (p *Parser) checkErrLast(results []Result, at Pos) {
	for i, r := range results {
		if !IsErrType(r.Type) {
			continue
		}
		if i != len(results)-1 {
			p.errorHere(r.Pos, "Err may only appear in the last result position",
				"in results "+resultText(results)+" Err sits at position "+itoa(i+1),
				"move Err to the last position (e.g. `-> (File, Err)`), or replace it with an ordinary result")
			return
		}
	}
}

func resultText(results []Result) string {
	var parts []string
	for _, r := range results {
		parts = append(parts, typeText(r.Type))
	}
	return "-> (" + strings.Join(parts, ", ") + ")"
}

// parseClass reads `class 名字[T] { … }`. v2 无继承、无 implements——
// 组合 + 接口（核心设计 §四）。
func (p *Parser) parseClass(export bool, annotations []string) Decl {
	at := p.pos()
	p.next() // class
	if !p.atKind(IDENT) {
		p.errorHere(at, "class is missing a name", "got "+describe(p.cur()),
			"write `class Name { ... }`; a class name starts with an ASCII letter or `_` (core design §1)")
		p.synchronize(false)
		return nil
	}
	d := &ClassDecl{Name: p.next().Text, Annotations: annotations, Pos: at}
	for _, a := range annotations {
		if a == "@packed" {
			d.Packed = true
		}
	}
	if export {
		p.errorHere(at, "export is only for exporting functions to C", "export class "+d.Name,
			"capitalise the first letter to make the type visible across packages (core design §4); a `_` prefix makes it package-private, so drop export")
	}
	// 泛型类 class Box[T] / class Box[T: Ord]
	if p.at(PUNCT, "[") {
		d.TypeParams, d.Constraints = p.parseTypeParamList("a generic class")
	}
	if t := p.cur(); t.Kind == PUNCT && t.Text == ":" {
		p.errorHere(p.pos(), "a class has no inheritance: composition plus interfaces (core design §4)", "`:` after "+d.Name,
			"embed the base class as a field, or express the shared ability with an interface")
		p.next()
		p.synchronize(false)
		return nil
	}
	if t := p.cur(); t.Kind == IDENT && t.Text == "implements" {
		p.errorHere(p.pos(), "`implements` is retired: an interface is satisfied structurally by the method signature set", "implements",
			"drop the implements clause: a class that matches the interface signatures satisfies it automatically")
		p.next()
		p.synchronize(false)
		return nil
	}
	if p.expect(PUNCT, "{", "class body is missing `{`") {
		p.parseClassBody(d)
		p.expect(PUNCT, "}", "class body is missing the closing `}`")
	}
	return d
}

func (p *Parser) parseClassBody(d *ClassDecl) {
	for {
		p.skipSemis()
		if p.at(PUNCT, "}") || p.cur().Kind == EOF {
			return
		}
		switch {
		case p.at(KW, "var") && p.peekIs(KW, "func"):
			// `var func` 是 v1 的可变方法标注，v2 已退役（附录 A）
			at := p.pos()
			p.errorHere(at, "`var func` is retired: every method may read and write this", "var func",
				"write `func name(...)`")
			p.next()
			p.next()
			p.synchronize(true)
		case p.at(KW, "var"):
			// `var 名字(...)` 是漏写 func 头的方法；但 `var fn (T) -> R` 是
			// 合法字段（函数类型）。区分：扫到配对 `)` 后跟 `{` 才是方法。
			if p.lookahead(1).Kind == IDENT && p.lookahead(2).Kind == PUNCT && p.lookahead(2).Text == "(" && p.closesToBody(p.i+2) {
				at := p.pos()
				p.errorHere(at, "a method must start with `func`",
					"var "+p.lookahead(1).Text+"(...)",
					"write a method as `func name(...)` and a field as `var name Type`")
				p.synchronize(true)
				continue
			}
			f := p.parseField()
			if f != nil {
				d.Fields = append(d.Fields, f)
			}
		case p.at(KW, "func"):
			m := p.parseMethod(d.Name)
			if m != nil {
				d.Methods = append(d.Methods, m)
			}
		case p.atIdent("use"):
			// N10：`use b B` —— 把 B 的方法提升到本类（上下文关键字，非 24th 关键字）。
			uat := p.pos()
			p.next()
			u := UseDecl{Pos: uat}
			if p.atKind(IDENT) {
				u.Field = p.next().Text
			} else {
				p.errorHere(p.pos(), "use is missing the field name", "use",
					"write `use field Type`: the field holds the embedded object whose methods are promoted (core design §16 N10)")
			}
			u.Type = p.parseType()
			d.Uses = append(d.Uses, u)
		default:
			at := p.pos()
			if t := p.cur(); t.Kind == IDENT && isRetiredKeyword(t.Text) {
				p.errorHere(at, retiredDesc(t.Text), "got "+t.Text, retiredFix(t.Text))
				p.next()
				p.synchronize(true)
				continue
			}
			p.errorHere(at, "a class body holds fields and methods only", "got "+describe(p.cur()),
				"write a field as `var name Type`, a method as `func name(...)`, a constructor as `func init(...)`")
			p.synchronize(true)
		}
	}
}

// closesToBody reports whether the `(` at token index open closes and is then
// followed by `{` — the shape of a method missing its `func` head. A function
// TYPE `(T) -> R` returns false. Pure lookahead; the cursor does not move.
func (p *Parser) closesToBody(open int) bool {
	depth := 0
	for j := open; j < len(p.toks); j++ {
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
				return nx.Kind == PUNCT && nx.Text == "{"
			}
		}
	}
	return false
}

// parseField reads a class field declaration with an optional default
// initializer: `var items map[i64]T = map.new()`.
func (p *Parser) parseField() *FieldDecl {
	at := p.pos()
	d := p.parseVar(true)
	if d == nil {
		p.synchronize(true)
		return nil
	}
	return &FieldDecl{Targets: d.Targets, Type: d.Type, Init: d.Init, Pos: at}
}

// parseMethod reads `func m(...)` inside a class body, with annotations.
func (p *Parser) parseMethod(class string) *FuncDecl {
	var annotations []string
	for p.cur().Kind == ANNOT {
		annotations = append(annotations, p.parseAnnotationUse())
	}
	if !p.at(KW, "func") {
		at := p.pos()
		p.errorHere(at, "a method must start with `func`", "got "+describe(p.cur()),
			"write a method as `func name(...)` and a field as `var name Type`")
		p.synchronize(true)
		return nil
	}
	d, _ := p.parseFunc(class, true, false, false, annotations, false).(*FuncDecl)
	return d
}

// parseInterface reads `interface` with signature-only methods (结构化满足).
func (p *Parser) parseInterface(export bool, annotations []string) Decl {
	at := p.pos()
	p.next() // interface
	if !p.atKind(IDENT) {
		p.errorHere(at, "interface is missing a name", "got "+describe(p.cur()),
			"write `interface Name { ... }`")
		p.synchronize(false)
		return nil
	}
	d := &InterfaceDecl{Name: p.next().Text, Annotations: annotations, Pos: at}
	if export {
		p.errorHere(at, "export is only for exporting functions to C", "export interface "+d.Name,
			"capitalise the first letter to make the type visible across packages (core design §4); a `_` prefix makes it package-private, so drop export")
	}
	if p.expect(PUNCT, "{", "interface body is missing `{`") {
		for {
			p.skipSemis()
			if p.at(PUNCT, "}") || p.cur().Kind == EOF {
				break
			}
			var ann []string
			for p.cur().Kind == ANNOT {
				ann = append(ann, p.parseAnnotationUse())
			}
			if !p.at(KW, "func") {
				// N9：裸名字 = 嵌入一个接口（`interface ReadWriter { Reader; Writer }`）。
				if p.atKind(IDENT) {
					eAt := p.pos()
					e := IfaceEmbed{Name: p.next().Text, AfterMethod: len(d.Methods), Pos: eAt}
					if p.at(PUNCT, ".") && p.peekKind() == IDENT {
						p.next()
						e.Name = e.Name + "." + p.next().Text
					}
					d.Embeds = append(d.Embeds, e)
					continue
				}
				p.errorCur("an interface holds method signatures and embedded interfaces only", "got "+describe(p.cur()),
					"write `func name(...) -> T` (no body), or embed another interface by name (core design §16 N9)")
				p.synchronize(true)
				continue
			}
			// A body is parsed here on purpose: the checker owns the
			// "interface 方法不能有函数体" rule so the diagnostic names the
			// interface and the method, rather than a generic parse error.
			m, _ := p.parseFunc(d.Name, true, false, false, ann, true).(*FuncDecl)
			if m != nil {
				d.Methods = append(d.Methods, m)
			}
		}
		p.expect(PUNCT, "}", "interface body is missing the closing `}`")
	}
	return d
}

// parseEnum reads `enum` with one-level variant names and optional payloads.
// 无方法、不得递归含自身（checker 执行，核心设计 §二.6/§四）。
func (p *Parser) parseEnum(annotations []string) Decl {
	at := p.pos()
	p.next() // enum
	if !p.atKind(IDENT) {
		p.errorHere(at, "enum is missing a name", "got "+describe(p.cur()),
			"write `enum Name { A, B }` or with payloads `enum Name { A(i32), B }`")
		p.synchronize(false)
		return nil
	}
	d := &EnumDecl{Name: p.next().Text, Annotations: annotations, Pos: at}
	if p.expect(PUNCT, "{", "enum body is missing `{`") {
		for {
			p.skipSemis()
			if p.at(PUNCT, "}") || p.cur().Kind == EOF {
				break
			}
			vat := p.pos()
			if !p.atKind(IDENT) {
				p.errorHere(vat, "enum variant name is missing", "got "+describe(p.cur()),
					"a variant is `A` or `A(i32)` with a payload")
				p.synchronize(true)
				continue
			}
			v := &EnumVariant{Name: p.next().Text, Pos: vat}
			if p.at(PUNCT, "(") {
				p.next()
				v.Payload = p.parseType()
				// **多负载不是本语言表面**（设计定案：负载是单值；元组类型不在类型构造子表里）。
				// 在这里定向报错并给可照做的替代写法，而不是让后面的 expect 报 "missing `)`"
				// （使用者会以为只是括号写错，见语言规范 §2.6 的反例）。
				if p.at(PUNCT, ",") {
					at := p.pos()
					for p.at(PUNCT, ",") {
						p.next()
						_ = p.parseType()
					}
					p.errorHere(at, "an enum variant takes exactly one payload type", "variant "+v.Name+"(…)",
						"wrap several fields in a value type: `@packed class P { var a i32  var b i32 }`, then write "+v.Name+"(P) (core design §2: an enum payload is a single value)")
				}
				p.expect(PUNCT, ")", "enum variant payload is missing the closing `)`")
			}
			d.Variants = append(d.Variants, v)
			if p.at(PUNCT, ",") {
				p.next()
				continue
			}
			if !p.at(PUNCT, "}") {
				p.errorCur("enum variants need `,` or `}` between them", "got "+describe(p.cur()),
					"separate variants with commas: enum E { A, B, C }")
			}
		}
		p.expect(PUNCT, "}", "enum body is missing the closing `}`")
	}
	if len(d.Variants) == 0 {
		p.errorHere(at, "an enum needs at least one variant", "enum "+d.Name+" is empty",
			"add variants: enum "+d.Name+" { A, B }")
	}
	return d
}
