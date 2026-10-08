package types

import (
	"fmt"
	"math"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// 常量折叠（核心设计 §三：const = 数值/bool/str 的折叠字面量算术，无函数调用）。
// 字面量推断（§一）：整数取最窄可容纳者 i32→i64→u64；浮点推断 f64、流向 f32
// 按可表示性判定。一元负号与字面量作为整体参与判定。
// ---------------------------------------------------------------------------

// foldConst 求值 const 初始化表达式；不可折叠返回修复提示。
func foldConst(e parse.Expr, c *Checker) (ConstVal, string) {
	switch v := e.(type) {
	case *parse.IntLit:
		return foldIntLit(v.Text)
	case *parse.FloatLit:
		f, err := parseFloat(v.Text)
		if err != nil {
			return ConstVal{}, "float literal cannot be parsed: " + v.Text
		}
		return ConstVal{Kind: ConstFloat, Float: f}, ""
	case *parse.StrLit:
		return ConstVal{Kind: ConstStr, Str: unquote(v.Text)}, ""
	case *parse.BoolLit:
		return ConstVal{Kind: ConstBool, Bool: v.Value}, ""
	case *parse.NilLit:
		return ConstVal{}, "a const cannot be initialised to nil (the zero value is meaningless)"
	case *parse.Unary:
		inner, err := foldConst(v.X, c)
		if err != "" {
			return ConstVal{}, err
		}
		switch v.Op {
		case "-":
			switch inner.Kind {
			case ConstInt:
				return ConstVal{Kind: ConstInt, Int: -inner.Int}, ""
			case ConstUint:
				if inner.Uint > 9223372036854775808 {
					return ConstVal{}, "negating an unsigned literal is out of range"
				}
				return ConstVal{Kind: ConstInt, Int: -int64(inner.Uint)}, ""
			case ConstFloat:
				return ConstVal{Kind: ConstFloat, Float: -inner.Float}, ""
			}
		case "!":
			if inner.Kind == ConstBool {
				return ConstVal{Kind: ConstBool, Bool: !inner.Bool}, ""
			}
		case "~":
			if inner.Kind == ConstInt {
				return ConstVal{Kind: ConstInt, Int: ^inner.Int}, ""
			}
			if inner.Kind == ConstUint {
				return ConstVal{Kind: ConstUint, Uint: ^inner.Uint}, ""
			}
		}
		return ConstVal{}, "this unary operation does not apply to the constant"
	case *parse.Binary:
		lhs, lerr := foldConst(v.Left, c)
		if lerr != "" {
			return ConstVal{}, lerr
		}
		rhs, rerr := foldConst(v.Right, c)
		if rerr != "" {
			return ConstVal{}, rerr
		}
		return foldBinary(v.Op, lhs, rhs)
	case *parse.Ident:
		if sym, ok := c.consts[v.Name]; ok && sym.Kind == SymConst {
			return sym.Const, ""
		}
		return ConstVal{}, "const allows literal arithmetic only, no name references (except existing consts)"
	case *parse.Field:
		// N4 内省取值形态：`fieldsOf(T).len` / `fieldsOf(T)[i].name`。
		if val, err, handled := foldIntrospect(v, c); handled {
			return val, err
		}
		return ConstVal{}, "const initialisation must be foldable literal arithmetic over numeric/bool/str"
	case *parse.Index:
		if val, err, handled := foldIntrospect(v, c); handled {
			return val, err
		}
		return ConstVal{}, "const initialisation must be foldable literal arithmetic over numeric/bool/str"
	case *parse.Call:
		// N4 编译期内省原语：在常量位可用（**运行期零元数据**：全部折成常量）。
		if v, err := foldComptime(v, c); err == "" {
			return v, ""
		} else if err != errNotComptime {
			return ConstVal{}, err
		}
		return ConstVal{}, "const initialisation allows no function calls; write literal arithmetic"
	}
	return ConstVal{}, "const initialisation must be foldable literal arithmetic over numeric/bool/str"
}

// errNotComptime 是"这不是编译期内省调用"的内部标记（调用方据此给出通用报错）。
var errNotComptime = "not a comptime intrinsic"

// foldComptime 折叠编译期内省原语（N4）：
//
//	typeName(T)    -> str     （类型的规范拼写）
//	sizeOf(T)      -> i32     （运行期字节数；@packed 与 [T;N] 走 C 的 sizeof，见 emit）
//	isValueType(T) -> bool    （是否有区域头）
//
// 实参必须是**类型名**（不是值）：i32 / 本包类型 / 跨包 pkg.Type。
func foldComptime(v *parse.Call, c *Checker) (ConstVal, string) {
	name := comptimeIntrinsic(v)
	if name == "" {
		return ConstVal{}, errNotComptime
	}
	if len(v.Args) != 1 {
		return ConstVal{}, name + " takes exactly one type argument"
	}
	t := c.resolveTypeArgName(v.Args[0])
	if t == nil {
		return ConstVal{}, name + " needs a type name as its argument (e.g. " + name + "(i32) or " + name + "(Point))"
	}
	switch name {
	case "typeName":
		return ConstVal{Kind: ConstStr, Str: typeDisplayName(c, t)}, ""
	case "isValueType":
		return ConstVal{Kind: ConstBool, Bool: isValueType(t)}, ""
	case "sizeOf":
		// 只有尺寸无歧义的类型能折成常量；@packed / [T;N] 的 C 布局由 emit 用
		// sizeof 现算（那才是真尺寸，绝不猜 padding）。
		if n, ok := staticSize(t); ok {
			return ConstVal{Kind: ConstInt, Int: n}, ""
		}
		return ConstVal{}, "sizeOf(" + typeText(t) + ") is only a compile-time constant for scalar and pointer types; " +
			"@packed / [T;N] sizes come from the C layout — use it in a runtime expression instead"
	}
	return ConstVal{}, errNotComptime
}

// typeDisplayName 是 `typeName(T)` 的结果：**非限定名**（就是类型标识符本身）。
//
// 为什么不带包前缀：typeName 的用途是"按名字分派/生成代码"（@derive 风格），
// 包前缀对同一份源码里的分派没有信息量，却会让结果随编译方式（单文件 / 整包）
// 变化 —— 那正好违反 H3「跨配置输出逐位一致」。跨包重名由用户自己限定。
func typeDisplayName(c *Checker, t Type) string {
	switch v := t.(type) {
	case *Class:
		return v.Name
	case *Enum:
		return v.Name
	case *Interface:
		return v.Name
	case *Instance:
		return typeDisplayName(c, v.Base)
	}
	return typeText(t)
}

// comptimeIntrinsic 报告调用是不是内省原语（返回原语名；不是 = 空串）。
func comptimeIntrinsic(v *parse.Call) string {
	id, ok := v.Fn.(*parse.Ident)
	if !ok {
		return ""
	}
	switch id.Name {
	case "typeName", "sizeOf", "isValueType":
		return id.Name
	}
	return ""
}

// staticSize 返回尺寸无歧义的类型的字节数（其余返回 false，由 emit 用 sizeof）。
func staticSize(t Type) (int64, bool) {
	switch v := t.(type) {
	case *Basic:
		switch v.Name {
		case "i8", "u8", "bool":
			return 1, true
		case "i16", "u16":
			return 2, true
		case "i32", "u32", "f32":
			return 4, true
		case "i64", "u64", "usize", "f64":
			return 8, true
		case "str":
			return 16, true // {ptr, len}
		case "Err":
			return 16, true // {code i32, msg str} → 对齐后 16
		}
	case *Class:
		if !v.Packed {
			return 8, true // 引用 = 指针
		}
	case *Interface:
		return 16, true // {data, vt}
	case *Slice, *MapT, *SetT, *BytesT, *ChanT:
		return 8, true // 容器/通道句柄 = 指针
	case *Enum:
		if !v.HasData {
			return 4, true
		}
	}
	return 0, false
}

// foldIntLit: 无后缀整数字面量 → 最窄可容纳（i32 → i64 → u64，§一）。
func foldIntLit(text string) (ConstVal, string) {
	n := normalizeUnderscores(text)
	neg := false
	if len(n) > 2 && (n[:2] == "0x" || n[:2] == "0b") {
		// 十六进制/二进制按位宽判定：默认走 u64 域再回签
		u, ok := parseUintRadix(n[2:], radixOf(n))
		if !ok {
			return ConstVal{}, "integer literal cannot be parsed: " + text
		}
		return classifyUint(u), ""
	}
	v, ok := parseIntRadix(stripSign(n))
	if !ok {
		return ConstVal{}, "integer literal cannot be parsed: " + text
	}
	_ = neg
	if v >= -2147483648 && v <= 2147483647 {
		return ConstVal{Kind: ConstInt, Int: v}, ""
	}
	return ConstVal{Kind: ConstInt, Int: v}, ""
}

// classifyUint: > i64 max 的无符号字面量归 ConstUint（u64 域）。
func classifyUint(u uint64) ConstVal {
	if u <= 9223372036854775807 {
		return ConstVal{Kind: ConstInt, Int: int64(u)}
	}
	return ConstVal{Kind: ConstUint, Uint: u}
}

func radixOf(n string) int {
	if n[:2] == "0x" {
		return 16
	}
	if n[:2] == "0b" {
		return 2
	}
	return 10
}

func normalizeUnderscores(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '_' {
			out = append(out, s[i])
		}
	}
	return string(out)
}

func stripSign(s string) string {
	if len(s) > 0 && s[0] == '-' {
		return s[1:]
	}
	return s
}

func parseUintRadix(s string, radix int) (uint64, bool) {
	if s == "" {
		return 0, false
	}
	var v uint64
	for i := 0; i < len(s); i++ {
		d := digitVal(s[i])
		if d < 0 || int(d) >= radix {
			return 0, false
		}
		nv := v*uint64(radix) + uint64(d)
		if nv < v {
			return 0, false // 溢出
		}
		v = nv
	}
	return v, true
}

func parseIntRadix(s string) (int64, bool) {
	u, ok := parseUintRadix(s, 10)
	if !ok {
		return 0, false
	}
	if u > 9223372036854775808 {
		return 0, false
	}
	return int64(u), true
}

func digitVal(b byte) int8 {
	switch {
	case b >= '0' && b <= '9':
		return int8(b - '0')
	case b >= 'a' && b <= 'f':
		return int8(b-'a') + 10
	case b >= 'A' && b <= 'F':
		return int8(b-'A') + 10
	}
	return -1
}

func parseFloat(s string) (float64, error) {
	var f float64
	_, err := fmt.Sscanf(normalizeUnderscores(s), "%g", &f)
	return f, err
}

// unquote 去掉字符串字面量的引号（转义由词法器校验，此处按原样截取）。
func unquote(raw string) string {
	if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
		body := raw[1 : len(raw)-1]
		return unescape(body)
	}
	return raw
}

func unescape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case 'n':
				out = append(out, '\n')
			case 't':
				out = append(out, '\t')
			case 'r':
				out = append(out, '\r')
			case '"':
				out = append(out, '"')
			case '\\':
				out = append(out, '\\')
			default:
				out = append(out, s[i+1])
			}
			i++
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}

func foldBinary(op string, l, r ConstVal) (ConstVal, string) {
	if l.Kind == ConstStr && r.Kind == ConstStr {
		switch op {
		case "+":
			return ConstVal{Kind: ConstStr, Str: l.Str + r.Str}, ""
		case "==":
			return ConstVal{Kind: ConstBool, Bool: l.Str == r.Str}, ""
		case "!=":
			return ConstVal{Kind: ConstBool, Bool: l.Str != r.Str}, ""
		}
		return ConstVal{}, "str supports + == != only"
	}
	if l.Kind == ConstBool && r.Kind == ConstBool {
		switch op {
		case "==":
			return ConstVal{Kind: ConstBool, Bool: l.Bool == r.Bool}, ""
		case "!=":
			return ConstVal{Kind: ConstBool, Bool: l.Bool != r.Bool}, ""
		case "&&":
			return ConstVal{Kind: ConstBool, Bool: l.Bool && r.Bool}, ""
		case "||":
			return ConstVal{Kind: ConstBool, Bool: l.Bool || r.Bool}, ""
		}
		return ConstVal{}, "bool supports == != && || only"
	}
	if isNumericConst(l) && isNumericConst(r) {
		if l.Kind == ConstFloat || r.Kind == ConstFloat {
			lf, lfOK := constAsFloat(l)
			rf, rfOK := constAsFloat(r)
			if !lfOK || !rfOK {
				return ConstVal{}, "float constant arithmetic failed"
			}
			return foldFloat(op, lf, rf)
		}
		if l.Kind == ConstUint || r.Kind == ConstUint {
			lu, lok := constAsUint(l)
			ru, rok := constAsUint(r)
			if !lok || !rok {
				return ConstVal{}, "unsigned constant arithmetic failed"
			}
			return foldUint(op, lu, ru)
		}
		return foldInt(op, l.Int, r.Int)
	}
	return ConstVal{}, "the two sides differ in type or are not foldable"
}

func isNumericConst(v ConstVal) bool {
	return v.Kind == ConstInt || v.Kind == ConstUint || v.Kind == ConstFloat
}

func constAsFloat(v ConstVal) (float64, bool) {
	switch v.Kind {
	case ConstFloat:
		return v.Float, true
	case ConstInt:
		return float64(v.Int), true
	case ConstUint:
		return float64(v.Uint), true
	}
	return 0, false
}

func constAsUint(v ConstVal) (uint64, bool) {
	switch v.Kind {
	case ConstUint:
		return v.Uint, true
	case ConstInt:
		if v.Int < 0 {
			return 0, false
		}
		return uint64(v.Int), true
	}
	return 0, false
}

func foldInt(op string, a, b int64) (ConstVal, string) {
	switch op {
	case "+":
		// 编译期算术**溢出 = 编译错**（§二.4/§九：可证必错 = 编译错；运行期才是 trap）。
		// 此前直接 `a + b` ⇒ Go 的 int64 回绕被静默接受
		// （`const Y = 9223372036854775807 + 1` 编译通过并打印负数）。
		if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
			return ConstVal{}, "constant addition overflows i64"
		}
		return ConstVal{Kind: ConstInt, Int: a + b}, ""
	case "-":
		if (b < 0 && a > math.MaxInt64+b) || (b > 0 && a < math.MinInt64+b) {
			return ConstVal{}, "constant subtraction overflows i64"
		}
		return ConstVal{Kind: ConstInt, Int: a - b}, ""
	case "*":
		if a != 0 && b != 0 {
			p := a * b
			if p/b != a || (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
				return ConstVal{}, "constant multiplication overflows i64"
			}
			return ConstVal{Kind: ConstInt, Int: p}, ""
		}
		return ConstVal{Kind: ConstInt, Int: 0}, ""
	case "/":
		if b == 0 {
			return ConstVal{}, "constant division by zero"
		}
		if a == math.MinInt64 && b == -1 {
			return ConstVal{}, "constant division overflows i64"
		}
		return ConstVal{Kind: ConstInt, Int: a / b}, ""
	case "%":
		if b == 0 {
			return ConstVal{}, "constant modulo by zero"
		}
		return ConstVal{Kind: ConstInt, Int: a % b}, ""
	case "==":
		return ConstVal{Kind: ConstBool, Bool: a == b}, ""
	case "!=":
		return ConstVal{Kind: ConstBool, Bool: a != b}, ""
	case "<":
		return ConstVal{Kind: ConstBool, Bool: a < b}, ""
	case "<=":
		return ConstVal{Kind: ConstBool, Bool: a <= b}, ""
	case ">":
		return ConstVal{Kind: ConstBool, Bool: a > b}, ""
	case ">=":
		return ConstVal{Kind: ConstBool, Bool: a >= b}, ""
	}
	return ConstVal{}, "this constant operation is not supported"
}

func foldUint(op string, a, b uint64) (ConstVal, string) {
	switch op {
	case "+":
		if a+b < a {
			return ConstVal{}, "constant addition overflows u64"
		}
		return classifyUint(a + b), ""
	case "-":
		if b > a {
			return ConstVal{}, "constant subtraction underflows u64"
		}
		return classifyUint(a - b), ""
	case "*":
		if a != 0 && (a*b)/a != b {
			return ConstVal{}, "constant multiplication overflows u64"
		}
		return classifyUint(a * b), ""
	case "/":
		if b == 0 {
			return ConstVal{}, "constant division by zero"
		}
		return classifyUint(a / b), ""
	case "%":
		if b == 0 {
			return ConstVal{}, "constant modulo by zero"
		}
		return classifyUint(a % b), ""
	}
	return ConstVal{}, "this constant operation is not supported"
}

func foldFloat(op string, a, b float64) (ConstVal, string) {
	switch op {
	case "+":
		return ConstVal{Kind: ConstFloat, Float: a + b}, ""
	case "-":
		return ConstVal{Kind: ConstFloat, Float: a - b}, ""
	case "*":
		return ConstVal{Kind: ConstFloat, Float: a * b}, ""
	case "/":
		return ConstVal{Kind: ConstFloat, Float: a / b}, ""
	}
	return ConstVal{}, "this constant operation is not supported"
}

// constFitsTy: 常量写入声明类型的范围证明（§二.4：编译期范围证明合法）。
func constFitsTy(v ConstVal, ty Type) bool {
	b, ok := ty.(*Basic)
	if !ok {
		return false
	}
	switch {
	case isInt(ty):
		switch v.Kind {
		case ConstInt:
			lo, hi, iok := intRange(ty)
			if !iok {
				return false
			}
			if v.Int >= lo && v.Int <= hi {
				return true
			}
			// u64 域边界：i64 无法表示 > i64max 的数，直接判位宽
			if v.Int < 0 {
				return false
			}
			um, uok := uintMax(ty)
			return uok && uint64(v.Int) <= um
		case ConstUint:
			um, uok := uintMax(ty)
			return uok && v.Uint <= um
		}
		return false
	case isFloat(ty):
		return v.Kind == ConstFloat || v.Kind == ConstInt || v.Kind == ConstUint
	case b.Name == "bool":
		return v.Kind == ConstBool
	case b.Name == "str":
		return v.Kind == ConstStr
	}
	return false
}

// inferredType: 无声明类型的 const 的推断类型（§一 字面量推断）。
func (v ConstVal) inferredType() Type {
	switch v.Kind {
	case ConstInt:
		if v.Int >= -2147483648 && v.Int <= 2147483647 {
			return TI32
		}
		return TI64
	case ConstUint:
		return TU64
	case ConstFloat:
		return TF64
	case ConstStr:
		return TStr
	case ConstBool:
		return TBool
	}
	return nil
}

// constArrayLen: [T;N] 的 N = **任何编译期可折叠的整数常量**（字面量 / const 名 /
// 编译期内省原语 —— `[i32; sizeOf(i32)]` 也必须能写，§16 N4 的 sizeOf 在标量上就是常量）。
func (c *Checker) constArrayLen(e parse.Expr) (int64, string) {
	switch v := e.(type) {
	case nil:
		return 0, "missing N"
	case *parse.Unary:
		if v.Op == "-" {
			return 0, "N must be non-negative"
		}
	}
	// 统一走常量折叠：字面量、const 名、内省原语（sizeOf/typeName 折叠）都在这里。
	cv, err := foldConst(e, c)
	if err != "" {
		return 0, "N must be a compile-time integer constant: " + err
	}
	switch cv.Kind {
	case ConstInt:
		if cv.Int < 0 || cv.Int > math.MaxInt32 {
			return 0, "N must be a non-negative integer that fits i32"
		}
		return cv.Int, ""
	case ConstUint:
		if cv.Uint > math.MaxInt32 {
			return 0, "N must be a non-negative integer that fits i32"
		}
		return int64(cv.Uint), ""
	}
	return 0, "N must be an integer constant (literal, const name, or a compile-time intrinsic)"
}

// typeText 渲染语义类型（诊断用）。
func typeText(t Type) string {
	if t == nil {
		return "?"
	}
	return t.String()
}
