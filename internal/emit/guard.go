package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 存储点守卫发射 (核心设计 §五 R3)。
//
// 守卫 = 判空跳过 + 一次深度比较 (AIC_GUARD 宏); 违反 = trap, 消息含存储点
// 位置与创建行 + 「创建处加 live」修复。静态可证安全时检查器不标 GuardSpec,
// 故本文件只在被标处发射 —— 无 region 块的程序全部存储同深度, 守卫为零开销。
// ---------------------------------------------------------------------------

// afterStore 在存储点之后发射守卫 (若有)。
// 存储点全清单 ⓪–⑥ (§五 R3): 变量槽 / 字段 / 下标 / 容器方法 / 接口装箱 /
// 多返回接收 / 含引用复合值。name 是已发射的存储目标表达式 (便于诊断)。
// hostExpr 是**宿主对象**的 C 表达式 (字段/下标存用；形参宿主时守卫的 limit
// 必须在运行期读宿主对象所在区域的深度，见 GuardSpec.Dynamic)；无宿主传 ""。
func (c *Ctx) afterStore(value parse.Expr, target string, valueType types.Type, hostExpr string) error {
	g, ok := c.guardOf(value)
	if !ok {
		return nil
	}
	switch g.Kind {
	case types.GuardStore:
		return c.emitGuardStore(value, target, g.Delta, g.Dynamic, hostExpr)
	case types.GuardComposite:
		// 含引用复合值: 对内部每个引用逐个守卫 (§五 R3 ⑥)。
		// 目前发射形态 = 对整体做一次存储点守卫 (值类型无头, 内部引用逐字段发射
		// 属圆 1 的复合值展开; 此处先保证可证非法的情形已在检查期拒绝)。
		return c.emitGuardStore(value, target, g.Delta, g.Dynamic, hostExpr)
	case types.GuardReturn:
		return nil // 返回点守卫由 emitSingleReturn 处理
	case types.GuardDefer:
		return nil // defer 注册点守卫由 emitDefer 处理
	}
	return nil
}

// emitGuardStore 发射一次存储点守卫。
// limit = 目标存储的深度: 变量槽 = 当前深度 − Delta (R0c-② F1; Delta 由检查器
// 记下声明偏移与当前偏移之差), 字段/容器 = 当前深度 (宿主界, §五 R3)。
// dynamic=true（宿主是形参派生）时 limit 必须**运行期**读宿主对象所在区域的深度：
// 参数定理只给"形参绝对深度 ≤ 入口"，用 aic_depth 当 limit 是**放宽**检查
// （实测：`func sink(h Holder, t Buf) { h.b = t }` 会静默悬垂）。
// value 可等价于宿主节点: 容器方法的守卫标记打在实参上, 但语义属于该调用语句
// (see emitExprStmt 的存储点 ③)。
func (c *Ctx) emitGuardStore(value parse.Expr, target string, delta int, dynamic bool, hostExpr string) error {
	e, err := c.expr(value, nil)
	if err != nil {
		return err
	}
	limit := "aic_depth"
	if dynamic && hostExpr != "" {
		limit = fmt.Sprintf("aic_region_depth_of(%s->hdr.reg)", paren("", hostExpr))
	} else if delta > 0 {
		limit = fmt.Sprintf("(aic_depth - %du)", delta)
	}
	pos := parse.ExprPos(value)
	c.srcLine(pos)
	c.guards.Kept++
	c.guards.Store++
	c.line("AIC_GUARD(%s, %s, %s, %d); /* 存储点 %s */",
		paren("", e), limit, cstr(c.Path), pos.Line, target)
	return nil
}

// emitReturnGuard 发射返回点守卫 (非精确界 > 0: 运行时比较 ≤ 入口深度, §五 R3)。
func (c *Ctx) emitReturnGuard(e string) {
	c.guards.Kept++
	c.guards.Return++
	c.line("AIC_GUARD(%s, 0u, %s, __LINE__); /* 返回点: 界 ≤ 入口 */", e, cstr(c.Path))
}

// guardOf 取节点的守卫标记。
func (c *Ctx) guardOf(e parse.Expr) (types.GuardSpec, bool) {
	if c.Info == nil || e == nil {
		return types.GuardSpec{}, false
	}
	return c.Info.HasGuard(e)
}

// emitRegion 发射 region 块 (进入 push +1, 退出 pop; 弹出 = bump 指针复位, §五 R1)。
func (c *Ctx) emitRegion(v *parse.RegionStmt, fnHasErr bool) error {
	c.srcLine(v.Pos)
	c.line("aic_region_push(%s, %d);", cstr(c.Path), v.Pos.Line)
	c.line("{")
	if err := c.emitBody(v.Body, fnHasErr); err != nil {
		return err
	}
	c.line("}")
	c.line("aic_region_pop();")
	return nil
}

// emitDefer 已由 defer.go 的实现取代（trampoline + 运行时 defer 栈）。
// 注册点入口见 emitDeferReg，出口见 emitDeferRun。

// emitCheckPropagate 发射 check 的传播 (err 非 nil 时立即返回前置零值 + 该 err, §六)。
// fnHasErr 由检查器保证 (所在函数无 Err 位 = 编译错)。
func (c *Ctx) emitCheckPropagate(x *parse.CheckExpr) error {
	call, ok := x.X.(*parse.Call)
	if !ok {
		return fmt.Errorf("emit: the check operand must be a call (line %d)", x.Pos.Line)
	}
	mt := types.MultiElems(c.ti(call))
	if mt == nil {
		return fmt.Errorf("emit: the check operand does not return multiple values (line %d)", x.Pos.Line)
	}
	tmp := c.tmp("chk")
	c.line("%s %s = %s;", c.retStructNameFor(mt), tmp, mustExpr(c.expr(call, nil)))
	last := len(mt) - 1
	c.line("if (%s._%d.code != 0) {", tmp, last)
	c.emitPropagateReturn(tmp, last)
	c.line("}")
	return nil
}

// emitPropagateReturn 发射 check/? 失败分支的返回（§六 传播）：
//   - 函数结果位 ≥ 2：合成返回结构，末位置该 Err，其余前置位取零（memset）；
//   - 结果位**恰好 1 个**（就是 Err 本身，如文档化的入口 `func main() -> Err`）：
//     直接返回那个 Err —— 此前一律按合成结构发，于是产出 `unknown type name
//     'aic_r_aic_Err'` + `._0` 的非法 C（属"文档化的形态却发不出"级缺陷）。
func (c *Ctx) emitPropagateReturn(src string, srcIdx int) {
	sig := c.curFunc
	if sig == nil || len(sig.Results) == 0 {
		c.emitDeferRun()
		c.line("    return;")
		return
	}
	if len(sig.Results) == 1 {
		out := c.tmp("prop")
		c.line("    %s %s = %s._%d;", c.cTypeName(sig.Results[0]), out, src, srcIdx)
		c.emitDeferRun()
		c.line("    return %s;", out)
		return
	}
	out := c.tmp("prop")
	c.line("    %s %s;", c.retStructNameFor(sig.Results), out)
	c.line("    memset(&%s, 0, sizeof(%s));", out, out)
	c.line("    %s._%d = %s._%d;", out, len(sig.Results)-1, src, srcIdx)
	c.emitDeferRun()
	c.line("    return %s;", out)
}

// checkValueExpr 把一个 check 调用发射成"evaluate + propagate + fetch"的表达式。
//
// 形态: 单独一行 { 声明 tmp = 调用; if (err) return 前置零值+err; } 后跟取值表达式。
// 这样不依赖语句表达式扩展（GNU `({ … })` 在 tcc 上不保证），四配置通用。
func (c *Ctx) checkValueExpr(x *parse.CheckExpr, want types.Type) (string, error) {
	call, ok := x.X.(*parse.Call)
	if !ok {
		return "", fmt.Errorf("emit: the check operand must be a call (line %d)", x.Pos.Line)
	}
	mt := types.MultiElems(c.ti(call))
	if mt == nil {
		return "", fmt.Errorf("emit: the check operand does not return multiple values (line %d)", x.Pos.Line)
	}
	tmp := c.tmp("chk")
	last := len(mt) - 1
	c.line("%s %s = %s;", c.retStructNameFor(mt), tmp, mustExpr(c.expr(call, nil)))
	c.line("if (%s._%d.code != 0) {", tmp, last)
	c.emitPropagateReturn(tmp, last)
	c.line("}")
	if len(mt) == 1 {
		_ = want
		return "(void)0", nil
	}
	return fmt.Sprintf("%s._0", tmp), nil
}
