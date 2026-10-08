package parse

import "strings"

// ---------------------------------------------------------------------------
// Type syntax (核心设计 §二). Type checking proper lives at R2; the parser only
// enforces the shape and records position for diagnostics.
//
// v2：`(A, B)` 不再是元组类型——多返回值只存在于返回位（parseFunc 展开为多个
// Result）；类型位的 `(A, B) -> R` 是函数类型（lambda 的类型，§三）。
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
	// FuncType is `(P1, P2) -> R` — the pure lambda's type (核心设计 §三).
	FuncType struct {
		Params []TypeExpr
		Result TypeExpr // nil for `-> ()`-free form: 参数表后无箭头时不存在
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
// appears in the last return position (红线 2); the caller reports the error.
func IsErrType(t TypeExpr) bool {
	switch v := t.(type) {
	case *BasicType:
		return v.Name == "Err"
	case *NamedType:
		return v.Name == "Err" && v.Pkg == ""
	}
	return false
}

// parseType reads a type expression starting at the current token.
func (p *Parser) parseType() TypeExpr {
	at := p.pos()

	// `(T)` grouping or the function type `(P1, P2) -> R`.
	if p.at(PUNCT, "(") {
		return p.parseParenType(at)
	}

	// set[T] and map[K]V are type constructors spelled as names, not keywords:
	// they are absent from the 23 frozen keywords (核心设计 §一), so they arrive
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

	// 包限定名 sync.Mutex（import 裸名；目录即包，核心设计 §四）。
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
		// 此前后缀循环排在实例化之前 ⇒ 泛型实例的列表类型根本写不出来
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
// (核心设计 §二.2), so it is rejected with the mechanical fix.
func (p *Parser) parseParenType(at Pos) TypeExpr {
	p.next() // (
	var elems []TypeExpr
	for {
		elems = append(elems, p.parseType())
		if p.at(PUNCT, ",") {
			p.next()
			continue
		}
		break
	}
	p.expect(PUNCT, ")", "parenthesised type is missing the closing `)`")
	if !p.at(PUNCT, "->") {
		if len(elems) == 1 {
			return elems[0]
		}
		p.errorHere(at, "there is no tuple type (a tuple is not a first-class type)", "got "+parenTypeText(elems),
			"multiple results appear only in the function result position: -> (A, B, Err); to store values use a class or a container")
		return elems[0]
	}
	p.next() // ->
	result := p.parseType()
	return &FuncType{Params: elems, Result: result, Pos: at}
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
