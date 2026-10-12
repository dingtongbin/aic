package emit

import (
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// print.println 的类型化分派 (核心设计 §十四: 任意单值; 容器嵌套深度上限 4,
// 超出打 …, 环安全且永不打印地址 = H3 确定性的前提)。
//
// 为什么在 emit 侧分派: 运行时只有 L0/L1 的定型原语 (aic_println_i32/str/…),
// 多态由编译器在定型点静态选择 (§〇 CPU 条款: 动态派发仅显式 interface)。
// ---------------------------------------------------------------------------

// literalType 给未定型字面量定宽 (§一 最窄可容纳)。
// 一元负号与字面量作为整体参与判定 (§一): -7 与 7 同为 i32。
func (c *Ctx) literalType(e parse.Expr) types.Type {
	switch v := e.(type) {
	case *parse.IntLit:
		var n int64
		text := v.Text
		for i := 0; i < len(text); i++ {
			ch := text[i]
			if ch < '0' || ch > '9' {
				return types.TI64
			}
			n = n*10 + int64(ch-'0')
		}
		if n <= 2147483647 {
			return types.TI32
		}
		return types.TI64
	case *parse.FloatLit:
		return types.TF64
	case *parse.StrLit:
		return types.TStr
	case *parse.BoolLit:
		return types.TBool
	case *parse.Unary:
		// 一元运算的结果类型 = 操作数的定型 (负号/取反/按位取反都不改类型)
		return c.literalType(v.X)
	}
	return nil
}
