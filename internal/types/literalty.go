package types

// ---------------------------------------------------------------------------
// 未定型字面量的定型（核心设计 §一「最窄可容纳」）。
//
// 这是**语言规则**，故只有这一处实现：AIR 降级（defer 上下文字段需要显式类型）
// 与 C 后端（打印原语按类型分派）都调它，各写一份迟早会分叉。
//
//   整数  按值域取最窄：i32 → i64（超出 i64 由检查器另行报错）
//   浮点  f64（`1.5f` 才是 f32，由字面量自带后缀决定，不走这里）
//   字符串 str   布尔 bool
// ---------------------------------------------------------------------------

// LiteralTy 给出一个字面量文本（AIR 的 `Const.Lit` 形态，可带前置 `-`）的类型名。
func LiteralTy(text string) string {
	if text == "" {
		return "i32"
	}
	switch {
	case text[0] == '"':
		return "str"
	case text == "true" || text == "false":
		return "bool"
	}
	neg := false
	s := text
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	if i := indexAnyByte(s, ".eE"); i >= 0 {
		return "f64" // 浮点字面量（f32 由 `f` 后缀在别处定，见 §一）
	}
	_ = neg
	v, ok := parseUintText(s)
	if !ok {
		return "i32"
	}
	if v <= 2147483647 {
		return "i32"
	}
	return "i64"
}

func indexAnyByte(s, set string) int {
	for i := 0; i < len(s); i++ {
		for j := 0; j < len(set); j++ {
			if s[i] == set[j] {
				return i
			}
		}
	}
	return -1
}

// parseUintText 解析无符号十进制文本（带 `_` 分隔符；不认识的形态返回 false）。
func parseUintText(s string) (uint64, bool) {
	var v uint64
	any := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' {
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
		any = true
		v = v*10 + uint64(c-'0')
	}
	return v, any
}
