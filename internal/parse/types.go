package parse

import "strings"

// ---------------------------------------------------------------------------
// Type syntax . Type checking proper lives at; the parser only
// enforces the shape and records position for diagnostics.
//
// `(A, B)` 不是元组类型——多返回值只存在于返回位（parseFunc 展开为多个
// Result）；类型位的 `(A, B) -> R` 是函数类型（lambda 的类型）。
// ---------------------------------------------------------------------------

type TypeExpr interface {
	Node
	typeNode()
}

type (
	// BasicType is one of the frozen primitive names (i8..usize, f32, f64,
	// bool, str) or the builtin Err.
	BasicType struct {
		Name string
		Pos
	}
	// SliceType is `T[]`.
	SliceType struct {
		Elem TypeExpr
		Pos
	}
	// SetType is `set[T]`.
	SetType struct {
		Elem TypeExpr
		Pos
	}
	// MapType is `map[K]V`.
	MapType struct {
		Key, Value TypeExpr
		Pos
	}
	// ArrayType is `[T;N]` — 内联值，N = 整数字面量或 const 名.
	ArrayType struct {
		Elem TypeExpr
		Size Expr
		Pos
	}
	// NamedType is a user type; Args carries generic instantiation Box[T].
	// Pkg carries the package prefix of a qualified name (sync.Mutex).
	NamedType struct {
		Name string
		Pkg  string
		Args []TypeExpr
		Pos
	}
	// FuncType is `(P1, P2) -> R` — the pure lambda's type .
	FuncType struct {
		Params []TypeExpr
		Result TypeExpr // nil for `-> ()`-free form: 参数表后无箭头时不存在
		// Results 非空 = 这是**结果位**的多返回值列表 `(A, B, Err)`。
		// 单一 Result 保留给普通函数类型，避免为同一语义引入两种 AST（P1）。
		Results []TypeExpr
		Pos
	}
)

func (*BasicType) typeNode() {}
func (*SliceType) typeNode() {}
func (*SetType) typeNode()   {}
func (*MapType) typeNode()   {}
func (*ArrayType) typeNode() {}
func (*NamedType) typeNode() {}
func (*FuncType) typeNode()  {}

// IsErrType reports whether a result slot is the frozen Err type. Err only
// appears in the last return position ; the caller reports the error.
func IsErrType(t TypeExpr) bool {
	switch v := t.(type) {
	case *BasicType:
		return v.Name == "Err"
	case *NamedType:
		return v.Name == "Err" && v.Pkg == ""
	}
	return false
}

// parseTypeNoDot 解析一个**不含包限定名**的类型（`i64` / `i32[]` / `Box[T]` 等）。
// 用途：表达式位 `map[K]V.new()` 的值类型 V —— `parseType` 会把 `.` 当作
// `pkg.Type` 的前缀从而吞掉 `.new`，这里明确不跨 `.`。
func (p *Parser) parseTypeNoDot() TypeExpr {
	at := p.pos()
	if !p.atKind(IDENT) {
		p.errorHere(at, "the value type of a map constructor is missing", "got "+describe(p.cur()),
			"write map[K]V.new() with a concrete value type V (AIC language spec §4)")
		return &BasicType{Name: "?", Pos: at}
	}
	name := p.next().Text
	nt := &NamedType{Name: name, Pos: at}
	// 泛型实参 Box[T]（可多层）
	for p.at(PUNCT, "[") {
		p.next()
		nt.Args = append(nt.Args, p.parseTypeNoDot())
		p.expect(PUNCT, "]", "type argument is missing the closing `]`")
	}
	// 切片后缀
	for p.at(PUNCT, "[") && p.peekIs(PUNCT, "]") {
		p.next()
		p.next()
		nt = &NamedType{Name: name, Pos: at}
		return &SliceType{Elem: nt, Pos: at}
	}
	return nt
}

// parseType reads a type expression starting at the current token.
// inResult = true 表示这是函数结果的**结果位**，此时 `(A, B, Err)` 是多结果
// 列表而非 tuple（《AIC 语言规范》S-1：Err 仅末位）。
func (p *Parser) parseType() TypeExpr {
	return p.parseTypeIn(false)
}

func (p *Parser) parseTypeIn(inResult bool) TypeExpr {
	at := p.pos()

	// `(T)` grouping or the function type `(P1, P2) -> R`.
	if p.at(PUNCT, "(") {
		grouped := p.parseParenTypeIn(at, inResult)
		// 分组的**后缀**必须接在这里：`(map[str]i64)[]` = map 的列表
		//（审计 ④：`map[str]i64[]` 的 `[]` 天然绑给 map 的值类型
		//⇒ 没有分组就写不出 ”map 的列表“，分组后缀必须在这里接）。直接 return
		//会让 `[]` 沦为表达式，报”赋值左侧必须是变量”。
		if _, isFunc := grouped.(*FuncType); !isFunc {
			for p.at(PUNCT, "[") && p.peekIs(PUNCT, "]") {
				p.next()
				p.next()
				grouped = &SliceType{Elem: grouped, Pos: at}
			}
		}
		return grouped
	}

	// set[T] and map[K]V are type constructors spelled as names, not keywords:
	// they are absent from the 23 frozen keywords , so they arrive
	// here as IDENT followed by `[`.
	if p.atKind(IDENT) && p.cur().Text == "set" && p.peekIs(PUNCT, "[") {
		p.next()
		p.expect(PUNCT, "[", "set type is missing `[`")
		elem := p.parseType()
		p.expect(PUNCT, "]", "set type is missing the closing `]`")
		return &SetType{Elem: elem, Pos: at}
	}
	if p.atKind(IDENT) && p.cur().Text == "map" && p.peekIs(PUNCT, "[") {
		p.next()
		p.expect(PUNCT, "[", "map type is missing `[`")
		key := p.parseType()
		p.expect(PUNCT, "]", "map key type is missing the closing `]`")
		val := p.parseType()
		return &MapType{Key: key, Value: val, Pos: at}
	}

	// [T;N]
	if p.at(PUNCT, "[") {
		p.next()
		elem := p.parseType()
		size := Expr(nil)
		if p.at(PUNCT, ";") {
			p.next()
			size = p.parseExpr()
		}
		p.expect(PUNCT, "]", "fixed-size array type is missing the closing `]`")
		return &ArrayType{Elem: elem, Size: size, Pos: at}
	}

	if !p.atKind(IDENT) {
		p.errorHere(at, "type name is missing", "a type name is required here",
			"add a type name such as i32, str, or a class name; a type is never optional")
		p.synchronize(false)
		return &BasicType{Name: "?", Pos: at}
	}

	name := p.next().Text
	t := TypeExpr(&NamedType{Name: name, Pos: at})

	// 包限定名 sync.Mutex（import 裸名；目录即包）。
	if p.at(PUNCT, ".") && p.peekKind() == IDENT {
		p.next()
		t = &NamedType{Name: p.next().Text, Pkg: name, Pos: at}
	}

	// T[] — repeated slice suffix
	for p.at(PUNCT, "[") && p.peekIs(PUNCT, "]") {
		p.next()
		p.next()
		t = &SliceType{Elem: t, Pos: at}
	}

	// Generic instantiation Box[T]
	if p.at(PUNCT, "[") {
		p.next()
		named, ok := t.(*NamedType)
		if !ok {
			p.errorHere(at, "only named types take type arguments", "got "+typeText(t),
				"write a named generic such as Box[T]; T[] is a list type and takes no arguments")
		}
		for {
			arg := p.parseType()
			if named != nil {
				named.Args = append(named.Args, arg)
			}
			if p.at(PUNCT, ",") {
				p.next()
				continue
			}
			break
		}
		p.expect(PUNCT, "]", "generic instantiation is missing the closing `]`")
		// **实例化之后的切片后缀**：`Box[i32][]` / `Box[Box[i32]][]`。
		// 后缀循环必须排在实例化之后 —— 若排在实例化之前 ⇒ 泛型实例的列表类型根本写不出来
		// （`var xs Box[i32][] = []` 报"赋值左侧必须是变量"）。
		for p.at(PUNCT, "[") && p.peekIs(PUNCT, "]") {
			p.next()
			p.next()
			t = &SliceType{Elem: t, Pos: at}
		}
	}
	return t
}

// parseParenType reads `(T)` (grouping) or `(P1, P2) -> R` (function type).
// A parenthesised type list without an arrow is a tuple type — 元组非一等类型
// , so it is rejected with the mechanical fix.
func (p *Parser) parseParenType(at Pos) TypeExpr {
	return p.parseParenTypeIn(at, false)
}

func (p *Parser) parseParenTypeIn(at Pos, inResult bool) TypeExpr {
	p.next() // (
	var elems []TypeExpr
	// 空形参表 `()` = 零参函数类型（《AIC 语言规范》§5.3：`func() { }` 与
	// `() -> R`）。无条件 parseType() 会把 `)` 当类型起点，报
	// "parenthesised type is missing the closing `)`"——必须先识别空形参表，
	// `g () -> (str, Err)` 才能作参数类型。
	if !p.at(PUNCT, ")") {
		for {
			elems = append(elems, p.parseType())
			if p.at(PUNCT, ",") {
				p.next()
				continue
			}
			break
		}
	}
	p.expect(PUNCT, ")", "parenthesised type is missing the closing `)`")
	if !p.at(PUNCT, "->") {
		if len(elems) == 1 {
			return elems[0]
		}
		if inResult {
			// 结果位的多返回值：由调用方（func 声明/字面量）按列表收集。
			return multiResultType(elems, at)
		}
		p.errorHere(at, "there is no tuple type (a tuple is not a first-class type)", "got "+parenTypeText(elems),
			"multiple results appear only in the function result position: -> (A, B, Err); to store values use a class or a container")
		return elems[0]
	}
	p.next() // ->
	// 结果位：`-> (A, B, Err)` 的括号是多结果列表，不是 tuple。
	result := p.parseTypeIn(true)
	return &FuncType{Params: elems, Result: result, Pos: at}
}

// multiResultType 把 `(A, B, Err)` 包成一个 FuncType 变体：Params 为空、
// Result 为首元素、Results 携带全表。这样结果位与普通类型位共用一套 AST，
// 不需要第二种类（P1：不为同一语义引入两种形态）。
func multiResultType(elems []TypeExpr, at Pos) TypeExpr {
	ft := &FuncType{Result: elems[0], Pos: at}
	ft.Results = elems
	return ft
}

func parenTypeText(elems []TypeExpr) string {
	var parts []string
	for _, e := range elems {
		parts = append(parts, typeText(e))
	}
	return "(" + strings.Join(parts, ", ") + ")"
}

// typeText renders a type back to a compact human string for diagnostics.
func typeText(t TypeExpr) string {
	switch v := t.(type) {
	case *BasicType:
		return v.Name
	case *NamedType:
		name := v.Name
		if v.Pkg != "" {
			name = v.Pkg + "." + v.Name
		}
		if len(v.Args) == 0 {
			return name
		}
		var args []string
		for _, a := range v.Args {
			args = append(args, typeText(a))
		}
		return name + "[" + strings.Join(args, ", ") + "]"
	case *SliceType:
		return typeText(v.Elem) + "[]"
	case *SetType:
		return "set[" + typeText(v.Elem) + "]"
	case *MapType:
		return "map[" + typeText(v.Key) + "]" + typeText(v.Value)
	case *ArrayType:
		return "[" + typeText(v.Elem) + ";N]"
	case *FuncType:
		var parts []string
		for _, e := range v.Params {
			parts = append(parts, typeText(e))
		}
		s := "(" + strings.Join(parts, ", ") + ")"
		if v.Result != nil {
			s += " -> " + typeText(v.Result)
		}
		return s
	}
	return "?"
}
