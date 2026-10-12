package emit

import (
	"aic/internal/parse"
	"aic/internal/types"
	"strings"
)

// ---------------------------------------------------------------------------
// print.println 的类型化分派 (任意单值; 容器嵌套深度上限 4,
// 超出打 …, 环安全且永不打印地址 = H3 确定性的前提)。
//
// 为什么在 emit 侧分派: 运行时只有 L0/L1 的定型原语 (aic_println_i32/str/…),
// 多态由编译器在定型点静态选择 (CPU 条款: 动态派发仅显式 interface)。
// ---------------------------------------------------------------------------

// literalType 给未定型字面量定宽 (最窄可容纳)。
// 一元负号与字面量作为整体参与判定: -7 与 7 同为 i32。
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

// ---------------------------------------------------------------------------
// 字符串字面量（原 expr.go：AST 路径已删）。
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 表达式发射。
//
// want = 上下文期望类型 (字面量定宽、nil 零值形态、容器字面量元素类型都由它定)。
// 每个表达式返回一段 C 表达式文本; 需要时用 C 语句表达式承载 check 的传播。
// ---------------------------------------------------------------------------

// ti 取表达式节点的检查类型 (缺失时返回 nil, 由调用方按上下文兜底)。
// 实现见 names.go (含 pass 1 名字表兜底)。

// unquoteAIC 去掉字符串字面量的引号并还原转义 (仅 \n \t \r \" \\)。
func unquoteAIC(raw string) string {
	if len(raw) < 2 || raw[0] != '"' {
		return raw
	}
	body := raw[1 : len(raw)-1]
	var b strings.Builder
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' || i+1 >= len(body) {
			b.WriteByte(body[i])
			continue
		}
		i++
		switch body[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte(body[i])
		}
	}
	return b.String()
}
