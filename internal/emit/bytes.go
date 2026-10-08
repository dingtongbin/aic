package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// N7 `bytes` 的发射（核心设计 §16 N7）。
//
// 表示：与 `u8[]` 同形 —— **复用 aic_list_u8 实例**（禁第二份缓冲实现）；
// bytes 只是它的独立类型身份 + 独立方法集。运行时薄包装见 runtime/aic_l1_list.h。
//
//	bytes.new()        → aic_bytes_new(line)
//	bytes.withCap(n)   → aic_bytes_with_cap(n, line)
//	bytes.fromStr(s)   → aic_bytes_from_str(s, line)
//	b.len()/cap()      → aic_list_len_u8 / aic_bytes_cap
//	b.get(i)/readAt    → aic_list_get_u8      （越界读 = trap）
//	b.set(i,v)/writeAt → aic_list_set_u8      （越界写 = 增长补零）
//	b.append(v)        → aic_list_append_u8
//	b.appendBytes(x)   → aic_bytes_append_bytes
//	b.slice(a,b)       → aic_bytes_slice
//	b.toStr()          → aic_bytes_to_str
//	b.clear()          → aic_list_clear_u8
// ---------------------------------------------------------------------------

// comptimeExpr 发射一次编译期内省的结果（N4）：
//   typeName    → str 字面量（编译期已知，运行期零开销）
//   isValueType → true/false
//   sizeOf      → 常量（标量/指针/句柄）或 `((aic_i32)sizeof(T))`（@packed / [T;N]）
//                 —— **绝不猜 padding**，真尺寸只由 C 的 sizeof 给出。
func (c *Ctx) comptimeExpr(cv types.ComptimeVal, line int) (string, error) {
	switch cv.Kind {
	case "typeName":
		return c.strLit(cv.Const.Str), nil
	case "isValueType":
		if cv.Const.Bool {
			return "true", nil
		}
		return "false", nil
	case "sizeOf":
		if cv.Const.Kind == types.ConstInt {
			return fmt.Sprintf("%d", cv.Const.Int), nil
		}
		return fmt.Sprintf("((aic_i32)sizeof(%s))", c.cTypeName(cv.Ty)), nil
	}
	return "", fmt.Errorf("emit: unknown comptime intrinsic %s (line %d)", cv.Kind, line)
}

// emitBytesCtor 发射 bytes 的类型级构造。
func (c *Ctx) emitBytesCtor(v *parse.Call, name string) (string, bool, error) {
	ln := uint32(v.Pos.Line)
	c.need("l1")
	switch name {
	case "new":
		return fmt.Sprintf("aic_bytes_new(%du)", ln), true, nil
	case "withCap":
		n, err := c.arg(v, 0, types.TUsize)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("aic_bytes_with_cap(%s, %du)", n, ln), true, nil
	case "fromStr":
		s, err := c.arg(v, 0, types.TStr)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("aic_bytes_from_str(%s, %du)", s, ln), true, nil
	}
	return "", true, fmt.Errorf("emit: no such bytes constructor %s (line %d)", name, v.Pos.Line)
}

// emitBytesMethod 发射 bytes 的实例方法。
func (c *Ctx) emitBytesMethod(v *parse.Call, field *parse.Field, rt types.Type) (string, bool, error) {
	recv, err := c.expr(field.X, rt)
	if err != nil {
		return "", true, err
	}
	ln := uint32(v.Pos.Line)
	file := cstr(c.Path)
	c.need("l1")
	switch field.Name {
	case "len":
		return fmt.Sprintf("aic_list_len_u8(%s)", recv), true, nil
	case "cap":
		return fmt.Sprintf("aic_bytes_cap(%s)", recv), true, nil
	case "get", "readAt":
		i, err := c.arg(v, 0, types.TUsize)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("aic_list_get_u8(%s, %s, %s, %d)", recv, i, file, ln), true, nil
	case "set", "writeAt":
		i, err := c.arg(v, 0, types.TUsize)
		if err != nil {
			return "", true, err
		}
		x, err := c.arg(v, 1, types.TU8)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("aic_list_set_u8(%s, %s, %s, %s, %d)", recv, i, x, file, ln), true, nil
	case "append":
		x, err := c.arg(v, 0, types.TU8)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("aic_list_append_u8(%s, %s, %s, %d)", recv, x, file, ln), true, nil
	case "appendBytes":
		b, err := c.arg(v, 0, types.TBytes)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("aic_bytes_append_bytes(%s, %s, %s, %d)", recv, b, file, ln), true, nil
	case "slice":
		a, err := c.arg(v, 0, types.TUsize)
		if err != nil {
			return "", true, err
		}
		b, err := c.arg(v, 1, types.TUsize)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("aic_bytes_slice(%s, %s, %s, %s, %d)", recv, a, b, file, ln), true, nil
	case "toStr":
		return fmt.Sprintf("aic_bytes_to_str(%s, %du)", recv, ln), true, nil
	case "clear":
		return fmt.Sprintf("aic_list_clear_u8(%s, %s, %d)", recv, file, ln), true, nil
	}
	return "", true, fmt.Errorf("emit: bytes has no method %s (line %d)", field.Name, v.Pos.Line)
}
