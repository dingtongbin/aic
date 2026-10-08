package types

import (
	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// N4 编译期反射的检查侧（核心设计 §16 N4）：
//
//   - `comptime { const …  assert … }` —— 全部编译期求值，零运行期代码；
//   - 内省原语 `fieldsOf(T)` / `variantsOf(T)` **只在编译期可用**：它们是
//     "编译期列表"，没有运行期形态（运行期零元数据）。
//
// 为什么 comptime 常量进**包级**表：`comptime { const N = fieldsOf(P).len }`
// 之后的任何函数体都要能用 `N`；若把它做成本地作用域，块外的代码就看不到，
// 这个块也就只剩断言一个用途（那不该叫 comptime）。
// ---------------------------------------------------------------------------

// checkComptimeBlock 检查包级 `comptime { … }`（在类型收尾之后、函数体之前跑）。
func (c *Checker) checkComptimeBlock(v *parse.ComptimeBlock) {
	for _, d := range v.Body {
		switch item := d.(type) {
		case *parse.ConstDecl:
			// 与普通 const 完全同一条路径：同一张表、同样的折叠与重名检查。
			c.collectConst(item)
		case *parse.ComptimeAssert:
			c.checkComptimeAssert(item)
		}
	}
}

// checkComptimeAssert 检查 `assert <可折叠 bool>`：不可折叠 / 不是 bool / 为 false
// 都是编译错，位置精确到断言点（断言的价值全在于报错位置准）。
func (c *Checker) checkComptimeAssert(v *parse.ComptimeAssert) {
	if v.Cond == nil {
		return
	}
	val, err := foldConst(v.Cond, c)
	if err != "" {
		c.errorAt(v.Pos, "a comptime assert needs a foldable condition", "assert "+exprText(v.Cond),
			err+" (core design §16 N4)")
		return
	}
	if val.Kind != ConstBool {
		c.errorAt(v.Pos, "a comptime assert needs a bool condition", "assert "+exprText(v.Cond),
			"the condition folds to "+typeText(val.inferredType())+"; write a comparison such as sizeOf(T) == 16 (core design §16 N4)")
		return
	}
	if !val.Bool {
		c.errorAt(v.Pos, "compile-time assertion failed", "assert "+exprText(v.Cond),
			"the condition is false at compile time; fix the invariant or the code (core design §16 N4)")
	}
}

// comptimeListIntrinsic 报告调用是不是**返回编译期列表**的内省原语。
func comptimeListIntrinsic(v *parse.Call) string {
	id, ok := v.Fn.(*parse.Ident)
	if !ok {
		return ""
	}
	switch id.Name {
	case "fieldsOf", "variantsOf":
		return id.Name
	}
	return ""
}

// isComptimeOnlyName 报告标识符是不是"只在编译期可用"的内省原语
// （运行期用 = 定向报错，而不是"未声明的名字"那种误导性诊断）。
func isComptimeOnlyName(name string) bool {
	return name == "fieldsOf" || name == "variantsOf"
}

// introRow 是内省列表的一行（field 或 variant 的编译期投影）。
type introRow struct {
	Name       string
	Ty         string // fieldsOf：字段类型的规范拼写；variantsOf：空
	Index      int64
	HasPayload bool // variantsOf 专用
}

// introspectRows 按原语名把类型展开成编译期行表；不可展开返回修复提示。
func (c *Checker) introspectRows(intrinsic string, t Type) ([]introRow, string) {
	switch intrinsic {
	case "fieldsOf":
		if en, isEnum := t.(*Enum); isEnum {
			return nil, "fieldsOf(" + en.Name + ") needs a class: an enum has variants — write variantsOf(" + en.Name + ")"
		}
		if in, isIface := t.(*Interface); isIface {
			return nil, "fieldsOf(" + in.Name + ") needs a class: an interface has methods, not fields (core design §16 N4)"
		}
		cl, ok := t.(*Class)
		if !ok {
			return nil, "fieldsOf(T) needs a class type, got " + typeText(t) + " (core design §16 N4)"
		}
		rows := make([]introRow, 0, len(cl.Fields))
		for i, f := range cl.Fields {
			rows = append(rows, introRow{Name: f.Name, Ty: typeText(f.Type), Index: int64(i)})
		}
		return rows, ""
	case "variantsOf":
		if cl, isClass := t.(*Class); isClass {
			return nil, "variantsOf(" + cl.Name + ") needs an enum: a class has fields — write fieldsOf(" + cl.Name + ")"
		}
		en, ok := t.(*Enum)
		if !ok {
			return nil, "variantsOf(T) needs an enum type, got " + typeText(t) + " (core design §16 N4)"
		}
		rows := make([]introRow, 0, len(en.Variants))
		for i, v := range en.Variants {
			rows = append(rows, introRow{Name: v.Name, Index: int64(i), HasPayload: v.Payload != nil})
		}
		return rows, ""
	}
	return nil, "not a compile-time list intrinsic"
}

// resolveIntroTarget 取 `fieldsOf(T)` / `variantsOf(T)` 的类型实参。
func (c *Checker) resolveIntroTarget(call *parse.Call, name string) (Type, string) {
	if len(call.Args) != 1 {
		return nil, name + " takes exactly one type argument, e.g. " + name + "(Point) (core design §16 N4)"
	}
	t := c.resolveTypeArgName(call.Args[0])
	if t == nil {
		return nil, name + " needs a type name as its argument (e.g. " + name + "(Point) or " + name + "(i32))"
	}
	return t, ""
}

// foldIntrospect 折叠内省原语的**取值形态**（N4）：
//
//	fieldsOf(T).len                  -> i32
//	fieldsOf(T)[i].name / .ty / .index
//	variantsOf(T).len                -> i32
//	variantsOf(T)[i].name / .hasPayload / .index
//
// 列表本身**不是运行期值**（"运行期零元数据"承诺的一部分），所以只支持
// `.len` 与「常量下标 + 取一行的一个成员」这两种形态。返回 handled=false
// 表示这个表达式根本不是内省调用（调用方给通用报错）。
func foldIntrospect(e parse.Expr, c *Checker) (ConstVal, string, bool) {
	field, ok := e.(*parse.Field)
	if !ok {
		return ConstVal{}, "", false
	}
	// 形态一：`fieldsOf(T).len`
	if call, isCall := field.X.(*parse.Call); isCall {
		name := comptimeListIntrinsic(call)
		if name == "" {
			return ConstVal{}, "", false
		}
		if field.Name != "len" {
			return ConstVal{}, name + "(T) is a compile-time list with no runtime value: read .len or [i].<member> (core design §16 N4)", true
		}
		t, err := c.resolveIntroTarget(call, name)
		if err != "" {
			return ConstVal{}, err, true
		}
		rows, rerr := c.introspectRows(name, t)
		if rerr != "" {
			return ConstVal{}, rerr, true
		}
		return ConstVal{Kind: ConstInt, Int: int64(len(rows))}, "", true
	}
	// 形态二：`fieldsOf(T)[i].member`
	ix, isIx := field.X.(*parse.Index)
	if !isIx {
		return ConstVal{}, "", false
	}
	call, isCall := ix.X.(*parse.Call)
	if !isCall {
		return ConstVal{}, "", false
	}
	name := comptimeListIntrinsic(call)
	if name == "" {
		return ConstVal{}, "", false
	}
	if ix.End != nil {
		return ConstVal{}, name + "(T) does not support slicing: index one row with a constant subscript (core design §16 N4)", true
	}
	t, err := c.resolveIntroTarget(call, name)
	if err != "" {
		return ConstVal{}, err, true
	}
	rows, rerr := c.introspectRows(name, t)
	if rerr != "" {
		return ConstVal{}, rerr, true
	}
	idxVal, ierr := foldConst(ix.Index, c)
	if ierr != "" {
		return ConstVal{}, "the subscript of " + name + "(T) must be a constant: " + ierr + " (core design §16 N4)", true
	}
	if idxVal.Kind != ConstInt {
		return ConstVal{}, "the subscript of " + name + "(T) must be an integer constant (core design §16 N4)", true
	}
	if idxVal.Int < 0 || idxVal.Int >= int64(len(rows)) {
		return ConstVal{}, "the subscript is out of range: " + name + "(T) has " + itoa(len(rows)) + " rows (core design §16 N4)", true
	}
	row := rows[idxVal.Int]
	switch field.Name {
	case "name":
		return ConstVal{Kind: ConstStr, Str: row.Name}, "", true
	case "index":
		return ConstVal{Kind: ConstInt, Int: row.Index}, "", true
	case "ty":
		if name != "fieldsOf" {
			return ConstVal{}, "variantsOf(T) rows have no .ty: a variant carries a payload, not a type slot; read .hasPayload (core design §16 N4)", true
		}
		return ConstVal{Kind: ConstStr, Str: row.Ty}, "", true
	case "hasPayload":
		if name != "variantsOf" {
			return ConstVal{}, "fieldsOf(T) rows have no .hasPayload: write .ty to read the field type (core design §16 N4)", true
		}
		return ConstVal{Kind: ConstBool, Bool: row.HasPayload}, "", true
	}
	if name == "fieldsOf" {
		return ConstVal{}, "a fieldsOf(T) row has .name / .ty / .index; got ." + field.Name + " (core design §16 N4)", true
	}
	return ConstVal{}, "a variantsOf(T) row has .name / .hasPayload / .index; got ." + field.Name + " (core design §16 N4)", true
}
