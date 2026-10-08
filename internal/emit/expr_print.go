package emit

import (
	"fmt"

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

// isPrintCall 报告该调用是否为 print.println。
func (c *Ctx) isPrintCall(call *parse.Call) bool {
	field, ok := call.Fn.(*parse.Field)
	if !ok {
		return false
	}
	id, ok := field.X.(*parse.Ident)
	if !ok {
		return false
	}
	if field.Name != "println" || id.Name != "print" {
		return false
	}
	return c.Info != nil && c.Info.Imported["print"]
}

// emitPrint 把 print.println(x) 作为语句发射。
func (c *Ctx) emitPrint(call *parse.Call) error {
	e, err := c.printCall(call)
	if err != nil {
		return err
	}
	c.srcLine(call.Pos)
	c.line("%s;", e)
	return nil
}

// printCall 返回 print.println(x) 对应的 C 调用表达式。
//
// 分派 = 类型 → 打印函数名（printgen.go 的唯一实现）：标量与"container of scalar elements"走
// 运行时表 aic_l1_print.h；enum/class/嵌套容器由 emit 合成 static 函数。
// println 的换行由 aic_print_nl() 给出（打印函数本身不带换行，便于嵌套复用）。
func (c *Ctx) printCall(call *parse.Call) (string, error) {
	if len(call.Args) != 1 {
		return "", fmt.Errorf("emit: println takes exactly one value (line %d)", call.Pos.Line)
	}
	arg := call.Args[0]
	// nil 字面量 = 零值语义, 打印 nil (§十四)
	if isNilLiteral(arg) {
		return "(aic_pr_nil(), aic_print_nl())", nil
	}
	t := c.printExprOf(arg)
	if t == nil {
		return "", fmt.Errorf("emit: the argument type of println is unknown (line %d)", call.Pos.Line)
	}
	e, err := c.expr(arg, t)
	if err != nil {
		return "", err
	}
	pv, err := c.printValue(e, t)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("(%s, aic_print_nl())", pv), nil
}

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
