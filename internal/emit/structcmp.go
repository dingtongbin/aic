package emit

import (
	"fmt"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// struct 值的比较发射（比较语义）。
//
//	`a == b` / `a != b`            →  逐字段比较（「struct == 字段逐项」）
//	`a < b` 等（需 derive Compare）→  compare(a, b) <op> 0
//
// 为什么不能直接发 C 的 `==`：struct 在 C 里是 struct，`a == b` 是非法 C
// （gcc：invalid operands to binary ==）。此前检查器放行、发射侧照发 ⇒ 产物编译失败。
//
// 落点约定与 spawn thunk 同形：**比较助手每类一份**（原型在原型段，体在 TU 末尾），
// 站点只发一次调用 —— 这样发射体时不必回头补定义。
// ---------------------------------------------------------------------------

// structCmpSym 登记一个 struct 类的比较助手并返回符号名（按类去重）。
func (c *Ctx) structCmpSym(cl *types.Class) string {
	if c.structCmp == nil {
		c.structCmp = map[string]bool{}
	}
	if c.structCmpUse == nil {
		c.structCmpUse = map[string]string{}
	}
	key := cl.Pkg + "." + cl.Name
	sym := c.cMethodName(cl, "compare")
	if cl.Derived["Compare"] {
		// 已有 derive Compare 生成的 compare 方法：直接用它（不重复生成）。
		c.structCmpUse[key] = sym
		return sym
	}
	if !c.structCmp[key] {
		c.structCmp[key] = true
		c.structCmpOrder = append(c.structCmpOrder, cl)
	}
	c.structCmpUse[key] = sym
	return sym
}

// emitStructCmpProtos 发射尚未生成的比较助手原型（原型段之后、函数体之前）。
func (c *Ctx) emitStructCmpProtos() {
	for _, cl := range c.structCmpOrder {
		c.line("static aic_i32 %s(%s, %s);", c.cMethodName(cl, "compare"),
			c.cTypeDecl(cl, "this__"), c.cTypeDecl(cl, "other"))
	}
	if len(c.structCmpOrder) > 0 {
		c.line("")
	}
}

// emitStructCmpBodies 发射比较助手体（TU 末尾，与 spawn thunk 同段）。
func (c *Ctx) emitStructCmpBodies() error {
	for _, cl := range c.structCmpOrder {
		if err := c.emitCompareBody(cl); err != nil {
			return err
		}
	}
	return nil
}

// structCompareCall 发射两个 struct 值的比较调用（返回 <0 / 0 / >0 的 i32）。
func (c *Ctx) structCompareCall(cl *types.Class, l, r string) string {
	return fmt.Sprintf("%s(%s, %s)", c.structCmpSym(cl), l, r)
}
