package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// N7 `bytes`（核心设计 §16）：区域托管、**可增长**的字节缓冲。
//
// 表示：与 `u8[]` 同形（复用运行时的 aic_list_u8 实例）—— `bytes` 是它的
// **独立类型身份 + 独立方法集**，不是第二个缓冲实现（禁第二份）。
//
// 语义（与容器一致的越界规则）：
//   - 越界**读** = trap；
//   - 越界**写** = 增长并补零（与 list.set 同源）；
//   - 与 `str` 的转换**必须显式**（toStr / fromStr，无隐式）。
// ---------------------------------------------------------------------------

// BytesT 是 `bytes` 的语义类型。
type BytesT struct{}

func (*BytesT) typeNode() {}

func (t *BytesT) String() string { return "bytes" }

// TBytes 是单例（与其它基本类型一样按值比较）。
var TBytes = &BytesT{}

// IsBytes 报告类型是否为 bytes。
func IsBytes(t Type) bool {
	_, ok := t.(*BytesT)
	return ok
}

// bytesMethod 返回 bytes 的实例方法签名（类型级构造 new/withCap/fromStr 在
// 调用点单独处理，与 chan[T].new 同款）。
func bytesMethod(name string) (*FuncSig, bool) {
	switch name {
	case "len", "cap":
		return &FuncSig{Name: name, Results: []Type{TUsize}}, true
	case "get", "readAt":
		return &FuncSig{Name: name, Params: []string{"i"}, ParamTypes: []Type{TUsize}, Results: []Type{TU8}}, true
	case "set", "writeAt":
		return &FuncSig{Name: name, Params: []string{"i", "v"}, ParamTypes: []Type{TUsize, TU8}}, true
	case "append":
		return &FuncSig{Name: name, Params: []string{"v"}, ParamTypes: []Type{TU8}}, true
	case "appendBytes":
		return &FuncSig{Name: name, Params: []string{"b"}, ParamTypes: []Type{TBytes}}, true
	case "slice":
		return &FuncSig{Name: name, Params: []string{"a", "b"}, ParamTypes: []Type{TUsize, TUsize}, Results: []Type{TBytes}}, true
	case "toStr":
		return &FuncSig{Name: name, Results: []Type{TStr}}, true
	case "clear":
		return &FuncSig{Name: name}, true
	}
	return nil, false
}

// checkBytesCtor 检查 `bytes.new()` / `bytes.withCap(n)` / `bytes.fromStr(s)`。
func (c *Checker) checkBytesCtor(v *parse.Call, name string) (Type, bound, untyped, bool) {
	switch name {
	case "new":
		if len(v.Args) != 0 {
			c.errorAt(v.Pos, "bytes.new() takes no arguments", "bytes.new(...)",
				"write bytes.new() for an empty buffer, or bytes.withCap(n) to reserve capacity (core design §16 N7)")
			return nil, nilBound, untyped{}, true
		}
	case "withCap":
		if len(v.Args) != 1 {
			c.errorAt(v.Pos, "bytes.withCap(n) takes exactly one argument", "bytes.withCap(...)",
				"write bytes.withCap(n) with n >= 0 (core design §16 N7)")
			return nil, nilBound, untyped{}, true
		}
		at, _, ainfo := c.checkExprFull(v.Args[0], TUsize)
		if at != nil {
			c.requireAssignableAt(v.Args[0], at, ainfo, TUsize, "bytes capacity")
		}
		if isNegativeLiteral(v.Args[0]) {
			c.errorAt(v.Pos, "a byte-buffer capacity cannot be negative", "bytes.withCap("+exprText(v.Args[0])+")",
				"a capacity is a count: write 0 for an empty buffer (core design §16 N7)")
			return nil, nilBound, untyped{}, true
		}
	case "fromStr":
		if len(v.Args) != 1 {
			c.errorAt(v.Pos, "bytes.fromStr(s) takes exactly one str", "bytes.fromStr(...)",
				"write bytes.fromStr(s): the bytes are a copy in the current region (core design §16 N7)")
			return nil, nilBound, untyped{}, true
		}
		st, _, sinfo := c.checkExprFull(v.Args[0], TStr)
		if st != nil {
			c.requireAssignableAt(v.Args[0], st, sinfo, TStr, "bytes.fromStr argument")
		}
	default:
		return nil, nilBound, untyped{}, false
	}
	return TBytes, bound{off: c.regionOff, exact: true}, untyped{}, true
}
