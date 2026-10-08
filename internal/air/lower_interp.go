package air

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// N2 字符串插值的去糖（核心设计 §16）。
//
// `"a{x}b{y}"` ⇒ `str + fmt(x) + "b" + fmt(y)`（AIR 的 `+`，C 后端按操作数类型
// 落 aic_str_concat）。
//
// fmt 按类型选**唯一**一套格式化（可格式化性由 checker 判定，这里只查类型表）：
//   str                  → 恒等（不产生调用）
//   bool                 → str.fromBool(v)
//   i8/i16/i32           → str.fromI64(i64(v))      （sext）
//   i64                  → str.fromI64(v)
//   u8/u16/u32           → str.fromU64(u64(v))      （zext）
//   u64/usize            → str.fromU64(v)
//   f32                  → str.fromF64(f64(v))      （fpext）
//   f64                  → str.fromF64(v)
//
// 符号名与源码里手写 `str.fromI64(x)` 的 AIR 拼写**完全一致**（str_<名字>），
// 故 C 后端只需要一张 std 名表（禁第二套命名）。
// ---------------------------------------------------------------------------

// interpLit 把一个插值字面量降级成一串 str 拼接。
func (l *lowerer) interpLit(v *parse.InterpLit) (string, error) {
	if len(v.Texts) != len(v.Values)+1 {
		return "", fmt.Errorf("air: malformed interpolation (line %d)", v.Pos.Line)
	}
	acc := ""
	add := func(part string) {
		if acc == "" {
			acc = part
			return
		}
		acc = l.strConcat(acc, part, v.Pos)
	}
	if v.Texts[0] != "" {
		add(l.strConst(v.Texts[0], v.Pos))
	}
	for i, val := range v.Values {
		f, err := l.interpFormat(val, v.Pos)
		if err != nil {
			return "", err
		}
		add(f)
		if txt := v.Texts[i+1]; txt != "" {
			add(l.strConst(txt, v.Pos))
		}
	}
	if acc == "" { // 只有 `{{` / `}}` 转义的串：退化成空串常量
		acc = l.strConst("", v.Pos)
	}
	return acc, nil
}

// strConst 把一个文本段变成 AIR 常量值。文本段保留源码里的转义拼写（与 StrLit
// 同源：解码发生在 C 字符串字面量那一层，禁第二份解码器）。
func (l *lowerer) strConst(s string, at parse.Pos) string {
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: "str",
		Rhs: &Const{Lit: "\"" + s + "\""}, Loc: LocOf(at)})
	return t
}

// strConcat 发射一次 str 拼接（AIR 层用 `+`，由 C 后端按类型落 aic_str_concat）。
func (l *lowerer) strConcat(a, b string, at parse.Pos) string {
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: "str",
		Rhs: &Binop{Op: "+", A: a, B: b}, Loc: LocOf(at)})
	return t
}

// interpFormat 把一个占位符值降级成 str 值。
func (l *lowerer) interpFormat(e parse.Expr, at parse.Pos) (string, error) {
	val, err := l.value(e)
	if err != nil {
		return "", err
	}
	vt := l.info.LookupType(e)
	if vt == nil {
		return "", fmt.Errorf("air: interpolated value has no recorded type (line %d)", at.Line)
	}
	if !types.InterpFormattable(vt) {
		return "", fmt.Errorf("air: type %s cannot be interpolated (line %d)", vt.String(), at.Line)
	}
	b, isBasic := vt.(*types.Basic)
	if !isBasic {
		return "", fmt.Errorf("air: type %s cannot be interpolated (line %d)", vt.String(), at.Line)
	}
	switch b.Name {
	case "str":
		return val, nil
	case "bool":
		return l.fmtCall("str_fromBool", val, at), nil
	case "f64":
		return l.fmtCall("str_fromF64", val, at), nil
	case "f32":
		return l.fmtCall("str_fromF64", l.conv("fpext", "f64", val, at), at), nil
	case "i8", "i16", "i32":
		return l.fmtCall("str_fromI64", l.conv("sext", "i64", val, at), at), nil
	case "i64":
		return l.fmtCall("str_fromI64", val, at), nil
	case "u8", "u16", "u32":
		return l.fmtCall("str_fromU64", l.conv("zext", "u64", val, at), at), nil
	case "u64", "usize":
		return l.fmtCall("str_fromU64", val, at), nil
	}
	return "", fmt.Errorf("air: type %s cannot be interpolated (line %d)", vt.String(), at.Line)
}

// conv 发射一次显式转换（AIR 的 Conv RHS）。
func (l *lowerer) conv(kind, target, val string, at parse.Pos) string {
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: target,
		Rhs: &Conv{Kind: kind, A: val}, Loc: LocOf(at)})
	return t
}

// fmtCall 发一次格式化调用（符号名 = AIR 的 std 拼写 `str_<名字>`，由 C 后端映射
// 到 aic_std_str_<名字>；插值是语言级降级，不要求源码 import str）。
func (l *lowerer) fmtCall(sym, val string, at parse.Pos) string {
	t := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: "str",
		Rhs: &Call{Sym: sym, Args: []string{val}}, Loc: LocOf(at)})
	return t
}
