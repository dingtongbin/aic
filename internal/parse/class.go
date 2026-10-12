package parse

import "strings"

// parseFunc reads a function or method. recv is empty for free functions.
// 方法一律可读写 this，无 `var func` 之分。
func (p *Parser) parseFunc(recv string, inClass bool, export, extern bool, bodyOptional bool) Decl {
	at := p.pos()
	p.next() // func
	d := &FuncDecl{
		Recv:        recv,
		Export:      export,
		Extern:      extern,
		Pos:         at,
	}
	// `init` is a method name, not one of the 23 frozen keywords ,
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

	// Return shape: none / single / `(A, B, Err)` 列表。Err must be last .
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

// checkErrLast enforces at parse time: Err only in the final slot.
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
// 组合 + 接口。
func (p *Parser) parseClass(export bool) Decl {
	at := p.pos()
	p.next() // class
	if !p.atKind(IDENT) {
		p.errorHere(at, "class is missing a name", "got "+describe(p.cur()),
			"write `class Name { ... }`; a class name starts with an ASCII letter or `_` (core design §1)")
		p.synchronize(false)
		return nil
	}
	d := &ClassDecl{Name: p.next().Text, Pos: at}
	// 《AIC 语言规范》§3.2：`derive A, B` 是声明修饰词（非注解）。
	d.Derives = p.parseDerives()
	if export {
		p.errorHere(at, "export is only for exporting functions to C", "export class "+d.Name,
			"capitalise the first letter to make the type visible across packages; a `_` prefix makes it package-private, so drop export")
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
	// `implements` 是接口满足的唯一途径（硬关键字，C10）。
	// 判据：大项目里"谁实现了这个接口"必须可 grep；结构式满足让 grep 失效。
	if p.at(KW, "implements") {
		p.parseImplements(d)
	}
	if p.expect(PUNCT, "{", "class body is missing `{`") {
		p.parseClassBody(d)
		p.expect(PUNCT, "}", "class body is missing the closing `}`")
	}
	return d
}

// parseStruct 读 `struct Name derive ... implements ... { fields methods }`。
// struct 是值类型拼写（《AIC 语言规范》§2.3），与 class 的引用语义相对。
// 实现上复用 class 的解析机构，语义由类型的值/引用二分决定（Struct = 值类型）。
func (p *Parser) parseStruct(export bool) Decl {
	at := p.pos()
	p.next() // struct
	if !p.atKind(IDENT) {
		p.errorHere(at, "struct is missing a name", "got "+describe(p.cur()),
			"write `struct Name { ... }`; a struct name starts with an ASCII letter or `_`")
		p.synchronize(false)
		return nil
	}
	d := &ClassDecl{Name: p.next().Text, Pos: at, Struct: true}
	d.Derives = p.parseDerives()
	if export {
		p.errorHere(at, "export is only for exporting functions to C", "export struct "+d.Name,
			"capitalise the first letter to make the type visible across packages; a `_` prefix makes it package-private, so drop export")
	}
	if p.at(PUNCT, "[") {
		d.TypeParams, d.Constraints = p.parseTypeParamList("a generic struct")
	}
	if p.at(KW, "implements") {
		p.parseImplements(d)
	}
	if p.expect(PUNCT, "{", "struct body is missing `{`") {
		p.parseClassBody(d)
		p.expect(PUNCT, "}", "struct body is missing the closing `}`")
	}
	return d
}

// parseDerives 读声明修饰词 `derive A, B`（封闭集，见标准库规范 §3.2）。
// parseImplements 读 `implements I, J` 或 `implements Iterator[i64]`。
// 存 TypeExpr 而非 string：泛型接口实例 `I[T]` 必须能在语法层表达，
// 否则 for-in 协议（Iterator[T]）写不出来。满足检查（C10）由检查器做。
func (p *Parser) parseImplements(d *ClassDecl) {
	p.next() // implements
	for {
		if !p.atKind(IDENT) {
			p.errorHere(p.pos(), "implements needs an interface name", "got " + describe(p.cur()),
				"write `implements I, J` or `implements Iterator[i64]`; the names must be interfaces declared in this package or imported")
			p.synchronize(false)
			return
		}
		d.Implements = append(d.Implements, p.parseType())
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
		return
	}
}

func (p *Parser) parseDerives() []string {
	if !p.at(KW, "derive") {
		return nil
	}
	p.next()
	var out []string
	for {
		if !p.atKind(IDENT) {
			p.errorHere(p.pos(), "derive needs a capability name", "got "+describe(p.cur()),
				"the closed set is ToString / Compare / Hash / Clone / Json")
			return out
		}
		out = append(out, p.next().Text)
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
		break
	}
	return out
}

func (p *Parser) parseClassBody(d *ClassDecl) {
	for {
		p.skipSemis()
		if p.at(PUNCT, "}") || p.cur().Kind == EOF {
			return
		}
		switch {
		case p.at(KW, "var") && p.peekIs(KW, "func"):
			// `func` 是关键字，做不了字段名 ⇒ 这不是字段，也不是方法。
			at := p.pos()
			p.errorHere(at, "a method must start with `func`", "var func",
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
		case p.at(KW, "use"):
			// `use b B` —— 把 B 的方法提升到本类（上下文关键字，非 24th 关键字）。
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

// parseMethod reads `func m(...)` inside a class body.
func (p *Parser) parseMethod(class string) *FuncDecl {
	if !p.at(KW, "func") {
		at := p.pos()
		p.errorHere(at, "a method must start with `func`", "got "+describe(p.cur()),
			"write a method as `func name(...)` and a field as `var name Type`")
		p.synchronize(true)
		return nil
	}
	d, _ := p.parseFunc(class, true, false, false, false).(*FuncDecl)
	return d
}

// parseInterface reads `interface` with signature-only methods (结构化满足).
// parseIfaceIncludes 读 `include I, J`（《AIC 语言规范》§3.1）。游标须停在
// `include` 关键字上。同名同签名的嵌入合一槽，同名不同签名由检查器报错。
func (p *Parser) parseIfaceIncludes(d *InterfaceDecl) {
	p.next() // include
	for {
		if !p.atKind(IDENT) {
			p.errorHere(p.pos(), "include needs an interface name", "got "+describe(p.cur()),
				"write `include Reader, Closer`; the names must be interfaces declared in this package or imported")
			return
		}
		eAt := p.pos()
		nm := p.next().Text
		if p.at(PUNCT, ".") && p.peekKind() == IDENT {
			p.next()
			nm = nm + "." + p.next().Text
		}
		d.Embeds = append(d.Embeds, IfaceEmbed{Name: nm, AfterMethod: len(d.Methods), Pos: eAt})
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
		return
	}
}

func (p *Parser) parseInterface(export bool) Decl {
	at := p.pos()
	p.next() // interface
	if !p.atKind(IDENT) {
		p.errorHere(at, "interface is missing a name", "got "+describe(p.cur()),
			"write `interface Name { ... }`")
		p.synchronize(false)
		return nil
	}
	d := &InterfaceDecl{Name: p.next().Text, Pos: at}
	if export {
		p.errorHere(at, "export is only for exporting functions to C", "export interface "+d.Name,
			"capitalise the first letter to make the type visible across packages (core design §4); a `_` prefix makes it package-private, so drop export")
	}
	// `interface Sink include Flush { }`：include 紧跟接口名，在 `{` 之前
	// （《AIC 语言规范》§3.1）。显式拼写可 grep——大项目里"这个接口包含谁"
	// 必须可回答。
	if p.at(KW, "include") {
		p.parseIfaceIncludes(d)
	}
	if p.expect(PUNCT, "{", "interface body is missing `{`") {
		for {
			p.skipSemis()
			if p.at(PUNCT, "}") || p.cur().Kind == EOF {
				break
			}
			if !p.at(KW, "func") {
				// `include I, J` = 接口并集（显式拼写，《AIC 语言规范》§3.1）。
				// 判据：显式拼写可 grep——大项目里"这个接口包含谁"必须可回答。
				// 接口体内的 `include I, J`（兼容位置；规范推荐接口名后）。
				if p.at(KW, "include") {
					p.parseIfaceIncludes(d)
					continue
				}
				// 裸名字 = 嵌入一个接口（兼容形态；include 是推荐拼写）。
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
			m, _ := p.parseFunc(d.Name, true, false, false, true).(*FuncDecl)
			if m != nil {
				d.Methods = append(d.Methods, m)
			}
		}
		p.expect(PUNCT, "}", "interface body is missing the closing `}`")
	}
	return d
}

// parseEnum reads `enum` with one-level variant names and optional payloads.
// 无方法、不得递归含自身（checker 执行）。
func (p *Parser) parseEnum() Decl {
	at := p.pos()
	p.next() // enum
	if !p.atKind(IDENT) {
		p.errorHere(at, "enum is missing a name", "got "+describe(p.cur()),
			"write `enum Name { A, B }`, with payloads `enum Name { A(i32), B }`, or as an error code `enum Name: ErrCode { A, B }`")
		p.synchronize(false)
		return nil
	}
	d := &EnumDecl{Name: p.next().Text, Pos: at}
	// `enum X: ErrCode { … }`：错误码约束枚举（§7.2）。
	// 码 = (fnv1a32(模块规范路径) & 0xFFFFF000) | 变体序号，构建期冲突检测。
	if p.at(PUNCT, ":") {
		p.next()
		if !p.atKind(IDENT) || p.cur().Text != "ErrCode" {
			p.errorHere(p.pos(), "the only enum constraint is ErrCode", "got "+describe(p.cur()),
				"write `enum HttpErr: ErrCode { Timeout, Connect }`; other constraints are not part of the surface")
		} else {
			p.next()
		}
		d.ErrCode = true
	}
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
				// （使用者会以为只是括号写错，见语言规范反例）。
				if p.at(PUNCT, ",") {
					at := p.pos()
					for p.at(PUNCT, ",") {
						p.next()
						_ = p.parseType()
					}
					p.errorHere(at, "an enum variant takes exactly one payload type", "variant "+v.Name+"(…)",
						"wrap several fields in a value type: `struct P { var a i32  var b i32 }`, then write "+v.Name+"(P) (language spec §2.2: an enum payload is a single value)")
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
