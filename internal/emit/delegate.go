package emit

import (
	"fmt"
	"sort"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// N10 显式委托的发射（核心设计 §16 N10）。
//
// `use b B` 把 B 的方法提升到 A：A **没有**方法体，只有一层薄转发 ——
//
//	static R aic_pkg_A_m(A *this__ , P… p…) {
//	    return aic_pkg_B_m(this__->b, p…);
//	}
//
// 形态与 @derive 合成方法完全一致（都是"无源码声明的方法体由 emit 合成"），
// 故放在派生方法段里发射：证明「提升调用零额外开销」——就是一次普通调用。
// ---------------------------------------------------------------------------

// delegatedMethods 收集一个类里被委托的方法名（排序：登记序必须确定，H2）。
func (c *Ctx) delegatedMethods(cl *types.Class) []string {
	names := make([]string, 0, len(cl.Methods))
	for name := range cl.Methods {
		sig, ok := cl.Method(name)
		if ok && sig.DelegateField != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// emitDelegatedProtos 发射全部委托方法的原型。
// **必须在 emitProtos 之后**：委托体调用被委托类的方法，那些原型要先在册
// （C11 没有隐式函数声明，顺序错了就是编译错）。
func (c *Ctx) emitDelegatedProtos() error {
	if c.Info == nil {
		return nil
	}
	for _, name := range c.classNames() {
		cl := c.Info.Classes[name]
		for _, m := range c.delegatedMethods(cl) {
			sig, _ := cl.Method(m)
			if err := c.checkDelegateSig(sig, cl, m); err != nil {
				return err
			}
			c.line("static %s %s(%s);", c.retCT(sig), c.cMethodName(cl, m), c.paramList(sig))
		}
	}
	return nil
}

// emitDelegatedBodies 发射全部委托方法的转发体（在全部函数体之后）。
func (c *Ctx) emitDelegatedBodies() error {
	if c.Info == nil {
		return nil
	}
	emitted := false
	for _, name := range c.classNames() {
		cl := c.Info.Classes[name]
		for _, m := range c.delegatedMethods(cl) {
			sig, _ := cl.Method(m)
			if err := c.emitDelegateBody(cl, m, sig); err != nil {
				return err
			}
			emitted = true
		}
	}
	if emitted {
		c.line("")
	}
	return nil
}

// classNames 是类名的确定序（map 遍历序会破坏 H2）。
func (c *Ctx) classNames() []string {
	names := make([]string, 0, len(c.Info.Classes))
	for name := range c.Info.Classes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// checkDelegateSig 挡住尚未支持的形状（多返回）：宁可编译期报错，不发半截代码。
func (c *Ctx) checkDelegateSig(sig *types.FuncSig, cl *types.Class, name string) error {
	if len(sig.Results) > 1 {
		return fmt.Errorf("emit: delegated method %s.%s has multiple results, which is not supported yet (line %d)",
			cl.Name, name, sigLine(sig))
	}
	return nil
}

func sigLine(sig *types.FuncSig) int { return 0 }

// emitDelegateBody 发射一层转发体。
func (c *Ctx) emitDelegateBody(cl *types.Class, name string, sig *types.FuncSig) error {
	target, ok := c.Info.Classes[sig.DelegateClass]
	if !ok {
		return fmt.Errorf("emit: delegated method %s.%s targets unknown class %s", cl.Name, name, sig.DelegateClass)
	}
	idx, ok := cl.FieldIndex(sig.DelegateField)
	if !ok {
		return fmt.Errorf("emit: delegated method %s.%s names a missing field %s", cl.Name, name, sig.DelegateField)
	}
	_ = idx
	base := c.fieldAccess(cl, "this__", sig.DelegateField)
	args := make([]string, 0, len(sig.Params)+1)
	args = append(args, base)
	args = append(args, sig.Params...)
	call := fmt.Sprintf("%s(%s)", c.cMethodName(target, name), joinComma(args))
	c.line("static %s %s(%s) {", c.retCT(sig), c.cMethodName(cl, name), c.paramList(sig))
	if len(sig.Results) == 0 {
		c.line("    %s;", call)
	} else {
		c.line("    return %s;", call)
	}
	c.line("}")
	return nil
}
