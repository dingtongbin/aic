package emit

import (
	"fmt"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// @packed 值的比较发射（核心设计 §三 比较语义）。
//
//	`a == b` / `a != b`            →  逐字段比较（§三：「@packed == 字段逐项」）
//	`a < b` 等（需 @derive(Compare)）→  compare(a, b) <op> 0
//
// 为什么不能直接发 C 的 `==`：@packed 在 C 里是 struct，`a == b` 是非法 C
// （gcc：invalid operands to binary ==）。此前检查器放行、发射侧照发 ⇒ 产物编译失败。
//
// 落点约定与 spawn thunk 同形：**比较助手每类一份**（原型在原型段，体在 TU 末尾），
// 站点只发一次调用 —— 这样发射体时不必回头补定义。
// ---------------------------------------------------------------------------

// packedCmpSym 登记一个 @packed 类的比较助手并返回符号名（按类去重）。
func (c *Ctx) packedCmpSym(cl *types.Class) string {
	if c.packedCmp == nil {
		c.packedCmp = map[string]bool{}
	}
	if c.packedCmpUse == nil {
		c.packedCmpUse = map[string]string{}
	}
	key := cl.Pkg + "." + cl.Name
	sym := c.cMethodName(cl, "compare")
	if cl.Derived["Compare"] {
		// 已有 @derive(Compare) 生成的 compare 方法：直接用它（不重复生成）。
		c.packedCmpUse[key] = sym
		return sym
	}
	if !c.packedCmp[key] {
		c.packedCmp[key] = true
		c.packedCmpOrder = append(c.packedCmpOrder, cl)
	}
	c.packedCmpUse[key] = sym
	return sym
}

// emitPackedCmpProtos 发射尚未生成的比较助手原型（原型段之后、函数体之前）。
func (c *Ctx) emitPackedCmpProtos() {
	for _, cl := range c.packedCmpOrder {
		c.line("static aic_i32 %s(%s, %s);", c.cMethodName(cl, "compare"),
			c.cTypeDecl(cl, "this__"), c.cTypeDecl(cl, "other"))
	}
	if len(c.packedCmpOrder) > 0 {
		c.line("")
	}
}

// emitPackedCmpBodies 发射比较助手体（TU 末尾，与 spawn thunk 同段）。
func (c *Ctx) emitPackedCmpBodies() error {
	for _, cl := range c.packedCmpOrder {
		if err := c.emitCompareBody(cl); err != nil {
			return err
		}
	}
	return nil
}

// packedCompareCall 发射两个 @packed 值的比较调用（返回 <0 / 0 / >0 的 i32）。
func (c *Ctx) packedCompareCall(cl *types.Class, l, r string) string {
	return fmt.Sprintf("%s(%s, %s)", c.packedCmpSym(cl), l, r)
}
