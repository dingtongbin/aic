package types

import (
	"sort"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// 函数体驱动：作用域装配（参数 = 函数体块既有名）、推断辅助、块作用域纪律。
// ---------------------------------------------------------------------------

// checkFuncBodies 第二阶段：逐函数检查体（多文件包按文件序，声明序不变）。
func (c *Checker) checkFuncBodies(files []*parse.File) {
	for _, f := range files {
		for _, d := range f.Decls {
			switch v := d.(type) {
			case *parse.FuncDecl:
				if v.Extern {
					continue // extern 绑定无函数体
				}
				c.checkOneFunc(v, c.funcs[v.Name])
			case *parse.ClassDecl:
				cl := c.classes[v.Name]
				for _, m := range v.Methods {
					c.checkOneFunc(m, cl.Methods[m.Name])
				}
			}
		}
	}
}

// checkOneFunc 检查单个函数/方法体。
func (c *Checker) checkOneFunc(v *parse.FuncDecl, sig *FuncSig) {
	if sig == nil || v.Body == nil {
		return
	}
	savedFn, savedThis, savedScope, savedNil, savedOff :=
		c.fn, c.thisClass, c.scope, c.nilState, c.regionOff
	savedKey := c.fnKey
	savedTP := c.typeParams
	c.fn = sig
	c.fnKey = FuncKey(sig.Recv, sig.Name)
	c.thisClass = nil
	if sig.Recv != "" {
		c.thisClass = c.classes[sig.Recv]
	}
	// **类型参数在函数/方法体内有效**（核心设计 §四 泛型）：`func dup[T](x T) -> T { var y T = x }`
	// 里的 T 必须解析成类型参数 —— 此前只在**签名**解析时安装（checker.go），
	// 于是泛型函数体里任何 `T` 都报"未知类型名"，泛型基本写不出可用代码。
	// 方法体继承接收类的类型参数（方法不得自带类型参数，§四）。
	c.typeParams = typeParamSet(v.TypeParams)
	if c.typeParams == nil {
		c.typeParams = map[string]bool{}
	}
	if sig.Recv != "" {
		if cl := c.classes[sig.Recv]; cl != nil && len(cl.TypeParams) > 0 {
			for _, tp := range cl.TypeParams {
				c.typeParams[tp] = true
			}
		}
	}
	defer func() { c.typeParams = savedTP }()
	c.scope = NewScope(nil, 0)
	c.nilState = map[string]*nilState{}
	c.regionOff = 0

	// 参数 = 函数体块的既有名（核心设计 §三）：声明进体块作用域。
	for i, p := range sig.Params {
		sym := &Symbol{Kind: SymVar, Name: p, Type: sig.ParamTypes[i], DeclOffset: 0, Param: true}
		if !c.scope.Declare(sym) {
			c.errorAt(v.Pos, "duplicate parameter name", p, "one name per parameter")
		}
		c.NoteLocal(c.fnKey, p)
		c.nilState[p] = &nilState{} // 参数 = 未证
	}
	// this 绑定
	if c.thisClass != nil && !sig.IsInit {
		c.scope.Declare(&Symbol{Kind: SymVar, Name: "this", Type: c.thisClass, DeclOffset: 0, Param: true})
		c.NoteLocal(c.fnKey, "this")
	}
	// 包级常量：函数体里按裸名可用（与参数同级的既有名）。**必须是这一层**：
	// 只在 const 折叠里认 c.consts 会让 `const X = 5` 在函数体里"未声明"
	// （emit 侧本来就按字面量发射常量，故这里只是让名字解析对得上）。
	for _, name := range sortedConstNames(c.consts) {
		c.scope.Declare(c.consts[name])
	}

	c.fnReturnCount = 0
	c.walkBlockStmts(v.Body)

	// 缺返回值：有返回位且体内零 return（全路径判定属圆 1 数据流）
	if len(sig.Results) > 0 && c.fnReturnCount == 0 {
		c.errorAt(v.Pos, "the function declares a result but has no return",
			sigName(sig)+" -> "+resultsText(sig.Results),
			"add a return (the Err slot takes nil); or drop the result type")
	}

	// main 特判：入口 func main() -> Err 或无返回（§四）
	if sig.Name == "main" && sig.Recv == "" && len(sig.Results) > 1 {
		c.errorAt(v.Pos, "main returns at most one Err", "func main() -> (…)",
			"write func main() -> Err or func main() (core design §4)")
	}

	c.fn, c.thisClass, c.scope, c.nilState, c.regionOff =
		savedFn, savedThis, savedScope, savedNil, savedOff
	c.fnKey = savedKey
}

// sortedConstNames 是包级常量名的确定序遍历（登记顺序不能依赖 map 序）。
func sortedConstNames(m map[string]*Symbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// walkBlockStmts 在当前作用域内走语句（不再建子作用域——块作用域由
// checkBlock 的调用方决定：函数体块=参数所在块）。
func (c *Checker) walkBlockStmts(b *parse.Block) {
	for _, s := range b.Stmts {
		c.checkStmt(s)
	}
}

// checkBlock 在新子作用域内检查一个块（内层块可遮蔽外层名）。
func (c *Checker) checkBlockScoped(b *parse.Block) {
	saved := c.scope
	c.scope = NewScope(saved, c.regionOff)
	c.walkBlockStmts(b)
	c.scope = saved
}

// literalDefault: 未类型化字面量在无目标类型时的默认定型（§一 最窄可容纳）。
func (c *Checker) literalDefault(info untyped, at parse.Pos) Type {
	switch info.kind {
	case unInt:
		if info.ival >= -2147483648 && info.ival <= 2147483647 {
			return TI32
		}
		return TI64
	case unUint:
		return TU64
	case unFloat:
		return TF64
	case unStr:
		return TStr
	case unBool:
		return TBool
	}
	return nil
}

// inferFromInit: var x = e 的推断（§一 字面量取最窄；无类型可推 = 报错）。
func (c *Checker) inferFromInit(vt Type, info untyped, at parse.Pos) Type {
	if info.kind != unNone && info.kind != unNil {
		switch info.kind {
		case unInt:
			if info.ival >= -2147483648 && info.ival <= 2147483647 {
				return TI32
			}
			return TI64
		case unUint:
			return TU64
		case unFloat:
			return TF64
		case unStr:
			return TStr
		case unBool:
			return TBool
		}
	}
	if vt != nil {
		return vt
	}
	c.errorAt(at, "the variable type cannot be inferred", "var x = ... (no type information on the right)",
		"write an explicit type: var x T = ...; a container literal needs a declared type (core design §2.2)")
	return nil
}
