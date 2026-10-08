package parse

import "strings"

// New builds a Parser over an already-lexed token stream. Tokens must end
// with the EOF token produced by the lexer.
func New(path string, toks []Token) *Parser {
	if len(toks) == 0 {
		toks = []Token{{Kind: "EOF", File: path}}
	}
	return &Parser{file: path, toks: toks}
}

// ParseFile parses a whole translation unit. The returned File is always
// non-nil; syntax problems are reported through p.Errors with one run
// collecting every diagnostic (红线 14).
func (p *Parser) ParseFile() *File {
	f := &File{Path: p.file}
	for {
		p.skipSemis()
		if p.cur().Kind == "EOF" {
			return f
		}
		before := p.i
		d := p.parseDecl()
		if d != nil {
			f.Decls = append(f.Decls, d)
		}
		if p.i == before { // no progress: force resync to guarantee termination
			p.next()
			p.synchronize(false)
		}
	}
}

func (p *Parser) skipSemis() {
	for p.at(PUNCT, ";") {
		p.next()
	}
}

// parseDecl reads one top-level declaration. Annotations and the export /
// extern modifiers may precede it.
// 顶层只许 const 与类型/函数声明：顶层 `var` = 编译错（红线 21）。
func (p *Parser) parseDecl() Decl {
	var annotations []string
	for p.cur().Kind == ANNOT {
		annotations = append(annotations, p.parseAnnotationUse())
	}
	export, extern := false, false
	for {
		switch {
		case p.at(KW, "export"):
			export = true
			p.next()
			continue
		case p.at(KW, "extern"):
			extern = true
			p.next()
			continue
		}
		break
	}
	var d Decl
	handled := true
	switch {
	case p.at(KW, "import"):
		d = p.parseImport()
	case p.at(KW, "const"):
		if export {
			p.errorHere(p.pos(), "export is only for exporting functions to C (core design §8)", "export const",
				"capitalise the first letter to make the constant visible across packages (core design §4); a `_` prefix makes it package-private, so drop export")
			export = false
		}
		d = p.parseConst(annotations)
	case p.at(KW, "enum"):
		if export {
			p.errorHere(p.pos(), "export is only for exporting functions to C (core design §8)", "export enum",
				"capitalise the first letter to make the type visible across packages (core design §4); a `_` prefix makes it package-private, so drop export")
			export = false
		}
		d = p.parseEnum(annotations)
	case p.at(KW, "var"):
		at := p.pos()
		p.errorHere(at, "a top-level var declaration is not allowed (red line 21)", "a top-level var",
			"mutable global state means: create objects inside main and pass them as arguments; for constants write const")
		p.synchronize(false)
		return nil
	case p.at(KW, "func"):
		d = p.parseFunc("", false, export, extern, annotations, false)
	case p.at(KW, "class"):
		d = p.parseClass(export, annotations)
	case p.at(KW, "interface"):
		d = p.parseInterface(export, annotations)
	default:
		handled = false
	}
	if handled {
		// 注解落点白名单（@packed 只能挂类、@noblock 只能挂 extern func…）。
		if d != nil {
			p.checkAnnotationPlacement(d, annotations)
		}
		return d
	}
	// N4 `comptime { … }`：上下文关键字（`comptime` 自己仍是合法标识符）。
	if p.atIdent("comptime") && p.peekIs(PUNCT, "{") {
		if export || extern {
			p.errorHere(p.pos(), "comptime cannot be exported or extern", "comptime { … }",
				"a comptime block is compile-time only: it emits no code, so there is nothing to export (core design §16 N4)")
		}
		if len(annotations) > 0 {
			p.errorHere(p.pos(), "annotations are not allowed on comptime", "comptime { … }",
				"annotations belong to classes/interfaces/enums/methods/local variables; drop it from comptime")
		}
		return p.parseComptimeBlock()
	}

	at := p.pos()
	if p.cur().Kind == ILL {
		// 词法器已报错：不再叠加「顶层出现非声明语句」。
		p.next()
		p.synchronize(false)
		return nil
	}
	if t := p.cur(); t.Kind == IDENT && isRetiredKeyword(t.Text) {
		p.errorHere(at, retiredDesc(t.Text), "got "+t.Text, retiredFix(t.Text))
		p.next()
		p.synchronize(false)
		return nil
	}
	p.errorHere(at, "a non-declaration statement at the top level", "got "+describe(p.cur()),
		"the top level holds import / const / func / class / interface / enum (optionally with export, extern, or annotations)")
	p.synchronize(false)
	return nil
}

// parseAnnotationUse reads `@name` or `@name(args)` (e.g. `@derive(ToString)`).
// The raw spelling including arguments is kept verbatim so later stages
// (expand / @derive) can inspect the arguments.
func (p *Parser) parseAnnotationUse() string {
	name := p.next().Text
	if !p.at(PUNCT, "(") {
		return name
	}
	var b strings.Builder
	b.WriteString(name)
	depth := 0
	for {
		t := p.cur()
		if t.Kind == EOF {
			p.errorCur("annotation argument list is not closed", "got "+describe(t), "add the closing `)`")
			break
		}
		if t.Kind == PUNCT && t.Text == "(" {
			depth++
		}
		if t.Kind == PUNCT && t.Text == ")" {
			depth--
		}
		b.WriteString(t.Text)
		p.next()
		if depth == 0 {
			break
		}
	}
	return b.String()
}

func (p *Parser) parseImport() Decl {
	at := p.pos()
	p.next() // import
	if !p.atKind(IDENT) {
		p.errorHere(at, "import is missing the package name", "got "+describe(p.cur()),
			"write `import name` (a bare identifier, no quotes; a directory is a package)")
		return nil
	}
	name := p.next().Text
	return &ImportDecl{Name: name, Pos: at}
}

func (p *Parser) parseConst(annotations []string) Decl {
	at := p.pos()
	p.next() // const
	if !p.atKind(IDENT) {
		p.errorHere(at, "const is missing the constant name", "got "+describe(p.cur()),
			"write `const name = expression`; a constant name starts with an ASCII letter or `_` (core design §1)")
		return nil
	}
	name := p.next().Text
	d := &ConstDecl{Name: name, Pos: at}
	if p.atKind(IDENT) && !p.at(PUNCT, "=") {
		d.Type = p.parseType()
	}
	if p.at(PUNCT, "=") {
		p.next()
		d.Init = p.parseExpr()
		if hasCheckExpr(d.Init) {
			p.errorHere(at, "check cannot initialise a const (core design §6)", "const "+name+" = ... check ...",
				"bind it inside a function first: var x = check f(), then define the constant from a pure expression")
		}
	} else {
		p.errorHere(at, "a constant must be explicitly initialised", "got "+describe(p.cur()),
			"write `const name = expression`; compile-time evaluation requires an initialiser")
		return nil
	}
	if len(annotations) > 0 {
		p.errorHere(at, "annotations are not allowed on const", "const "+name+" carries "+strings.Join(annotations, " "),
			"annotations belong to classes/interfaces/enums/methods/local variables; drop it from const")
	}
	return d
}

// parseVar handles local declarations and class fields (isDecl 只控制错误
// 恢复的同步范围)。顶层 var 由 parseDecl 拒绝，不经过这里。
// 目标形：`name` / `@live name` / `_`（弃位）。
func (p *Parser) parseVar(isDecl bool) *VarDecl {
	at := p.pos()
	p.next() // var
	d := &VarDecl{Pos: at}
	for {
		tgt, ok := p.parseVarTarget()
		if !ok {
			p.synchronize(!isDecl)
			return nil
		}
		d.Targets = append(d.Targets, tgt)
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
		break
	}
	// Optional explicit type: `var a i32` / `var xs i32[]`
	if !p.at(PUNCT, "=") && !p.atAnyPUNCT(";") && p.startsType() {
		d.Type = p.parseType()
	}
	if p.at(PUNCT, "=") {
		p.next()
		d.Init = p.parseExpr()
		if d.Init != nil {
			p.validateCheckPosition(d.Init, at)
			p.validateBlankValues(d.Init, false)
		}
	} else if !isDecl && d.Type == nil {
		// A local declaration without type or initializer is meaningless.
		p.errorHere(at, "a local var has neither a type nor an initialiser", "var "+targetNames(d.Targets),
			"add a type (var a i32) or an initialiser (var a = 0)")
	}
	p.validateVarTargets(d, at)
	return d
}

// validateVarTargets enforces the parse-level binding rule: a single `_`
// target is meaningless — 单返回丢弃 = 直接语句调用 (核心设计 §六).
func (p *Parser) validateVarTargets(d *VarDecl, at Pos) {
	if len(d.Targets) == 1 && d.Targets[0].Blank {
		p.errorHere(at, "`var _ = ...` is meaningless", "a single discard _ was declared here",
			"to drop a single result, call it as a statement: f(); for multi-returns write var a, _ = f()")
	}
}

func targetNames(ts []VarTarget) string {
	var parts []string
	for _, t := range ts {
		switch {
		case t.Blank:
			parts = append(parts, "_")
		case t.Live:
			parts = append(parts, "@live "+t.Name)
		default:
			parts = append(parts, t.Name)
		}
	}
	return strings.Join(parts, ", ")
}

// parseVarTarget reads `name` / `@live name` / `_`.
func (p *Parser) parseVarTarget() (VarTarget, bool) {
	at := p.pos()
	tgt := VarTarget{Pos: at}
	if p.at(ANNOT, "@live") {
		p.next()
		tgt.Live = true
	}
	if p.at(PUNCT, "_") {
		p.next()
		tgt.Name = "_"
		tgt.Blank = true
		return tgt, true
	}
	if p.cur().Kind == ILL {
		// 词法器已报错（报错不重复计因）：吞掉 ILL 交给上层恢复。
		p.next()
		return tgt, false
	}
	if !p.atKind(IDENT) {
		p.errorHere(at, "variable name is missing or invalid", "got "+describe(p.cur()),
			"a name starts with an ASCII letter or `_`; `__` is reserved for mangling, and a top-level A-Z name is visible across packages (core design §1/§4)")
		return tgt, false
	}
	tgt.Name = p.next().Text
	// `var mut x` — the v1 mutability annotation arrived as a target name.
	if tgt.Name == "mut" && p.atKind(IDENT) {
		p.errorHere(tgt.Pos, "`mut` is retired: assignment shares a reference, there is no mutability annotation", "var mut "+p.peekText(),
			"drop the mut keyword: var "+p.peekText()+" = ...; variables are reassignable by default")
	}
	return tgt, true
}

// startsType distinguishes `var a T` from `var a = expr` / `var a, b = f()`.
// Types begin with a name (basic types, classes, set[T], map[K]V), `(` (函数
// 类型) or `[` (定长数组 [T;N] / 列表 T[] 追加在名字后).
func (p *Parser) startsType() bool {
	t := p.cur()
	if t.Kind == IDENT {
		return true
	}
	if t.Kind == PUNCT {
		return t.Text == "(" || t.Text == "["
	}
	return false
}
