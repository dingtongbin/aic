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

// parseDecl reads one top-level declaration. The export / extern modifiers may
// precede it. v7 起无注解（《AIC 语言规范》§1.3）。
// 顶层只许 const 与类型/函数声明：顶层 `var` = 编译错（红线 21）。
func (p *Parser) parseDecl() Decl {
	export, extern := false, false
	for {
		switch {
		case p.at(KW, "export"):
			export = true
			p.next()
			continue
		case p.at(KW, "extern"):
			// `extern from "h"` 是**整条声明**（§11.2），不是 extern 修饰符。
			// 这里若先吃掉 extern，下面行 108 的 `p.at(KW,"extern")` 就再也
			// 看不到它，于是报"顶层非声明语句 got IDENT"（037 实测）。
			if p.peekIs(IDENT, "from") {
				break
			}
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
		d = p.parseConst()
	case p.at(KW, "enum"):
		if export {
			p.errorHere(p.pos(), "export is only for exporting functions to C (core design §8)", "export enum",
				"capitalise the first letter to make the type visible across packages (core design §4); a `_` prefix makes it package-private, so drop export")
			export = false
		}
		d = p.parseEnum()
	case p.at(KW, "var"):
		at := p.pos()
		p.errorHere(at, "a top-level var declaration is not allowed (red line 21)", "a top-level var",
			"mutable global state means: create objects inside main and pass them as arguments; for constants write const")
		p.synchronize(false)
		return nil
	case p.at(KW, "func"):
		d = p.parseFunc("", false, export, extern, false)
	case p.at(KW, "struct"):
		d = p.parseStruct(export)
	case p.at(KW, "class"):
		d = p.parseClass(export)
	case p.at(KW, "interface"):
		d = p.parseInterface(export)
	default:
		handled = false
	}
	if handled {
		return d
	}
	// 《AIC 语言规范》§6.4：`comptime { … }` 是硬关键字开头的编译期块。
	if p.at(KW, "comptime") {
		if export || extern {
			p.errorHere(p.pos(), "comptime cannot be exported or extern", "comptime { … }",
				"a comptime block is compile-time only: it emits no code, so there is nothing to export")
		}
		return p.parseComptimeBlock()
	}
	// 《AIC 语言规范》§11.2：`extern from "header.h"` 直接包装 C 头文件。
	if p.at(KW, "extern") && p.peekIs(IDENT, "from") {
		return p.parseExternFrom(export)
	}

	at := p.pos()
	if p.cur().Kind == ILL {
		// 词法器已报错：不再叠加「顶层出现非声明语句」。
		p.next()
		p.synchronize(false)
		return nil
	}
	p.errorHere(at, "a non-declaration statement at the top level", "got "+describe(p.cur()),
		"the top level holds import / const / func / class / struct / interface / enum / comptime / extern (optionally with export or extern)")
	p.synchronize(false)
	return nil
}

// 注解（`@name`）已随《AIC 语言规范》§1.3 整体删除：`@` 在词法层就是非法字符，
// 这里连同 parseAnnotationUse 一起清掉 —— 留着一个永远调不到的解析函数，会让
// 下一个读代码的人以为注解还在（阶段 1 退出条件：注解机制从代码中消失）。

func (p *Parser) parseImport() Decl {
	at := p.pos()
	p.next() // import
	if !p.atKind(IDENT) {
		p.errorHere(at, "import is missing the package path", "got "+describe(p.cur()),
			"write `import app/ui` (a path of ASCII identifiers separated by `/`; a directory is a package)")
		return nil
	}
	path := p.next().Text
	// 路径导入：`import a/b/c` → 限定名 = 末段 `c`（《AIC 语言规范》§10.1）。
	// 单段名（`import fmt`）同时覆盖标准包与本地顶层包，解析规则见 §10.2。
	for p.at(PUNCT, "/") {
		if p.peekText() == "" || p.peekText() == "/" {
			p.errorHere(p.pos(), "import path has a trailing `/`", path+"/",
				"write the full path: `import app/ui`")
			break
		}
		p.next() // /
		if !p.atKind(IDENT) {
			p.errorHere(p.pos(), "import path segment is missing after `/`", "got "+describe(p.cur()),
				"each segment is an ASCII identifier: `import app/ui`")
			break
		}
		path = path + "/" + p.next().Text
	}
	return &ImportDecl{Name: path, Pos: at}
}

// parseExternFrom 读 `extern from "header.h"`（《AIC 语言规范》§11.2）。
// 它把 C 头文件的声明映射为本包的 extern 声明；头文件搜索路径与宏定义
// 写在 aic.mod 的 cflag 行。解析实现在 internal/cinterp（阶段 6），
// 这里只固定语法面。
func (p *Parser) parseExternFrom(export bool) Decl {
	at := p.pos()
	p.next() // extern
	p.next() // from
	if !p.atKind(STR) {
		p.errorHere(at, "extern from needs a header path in quotes", "got "+describe(p.cur()),
			"write extern from \"sqlite3.h\"; add search paths with cflag -I... in aic.mod")
		p.synchronize(false)
		return nil
	}
	if export {
		p.errorHere(at, "export cannot be combined with extern from", "export extern from",
			"extern from brings C declarations in; there is no AIC declaration to export")
	}
	header := p.next().Text
	// 可选的 only 过滤清单：extern from "h" only a, b, c
	var only []string
	if p.atKind(IDENT) && p.cur().Text == "only" {
		p.next()
		for {
			if !p.atKind(IDENT) {
				p.errorHere(p.pos(), "only needs a name list", "got "+describe(p.cur()),
					"write `extern from \"h\" only a, b, c`")
				break
			}
			only = append(only, p.next().Text)
			if p.at(PUNCT, ",") {
				p.next()
				continue
			}
			break
		}
	}
	return &ExternFromDecl{Header: header, Only: only, Pos: at}
}

func (p *Parser) parseConst() Decl {
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
	return d
}

// parseVar handles local declarations and class fields (isDecl 只控制错误
// 恢复的同步范围)。顶层 var 由 parseDecl 拒绝，不经过这里。
// 目标形：`name` / `live name` / `_`（弃位）。R20：`live` 是关键字（不再是
// `@live` 注解）—— 与 var/const 同属声明层修饰语。
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
			parts = append(parts, "live "+t.Name)
		default:
			parts = append(parts, t.Name)
		}
	}
	return strings.Join(parts, ", ")
}

// parseVarTarget reads `name` / `live name` / `_`.
func (p *Parser) parseVarTarget() (VarTarget, bool) {
	at := p.pos()
	tgt := VarTarget{Pos: at}
	if p.at(KW, "live") {
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
