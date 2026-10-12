package emit

import (
	"strings"
)

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
