package emit

import (
	"fmt"
	"sort"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 泛型函数单态化（T1：基本形态 + 实例缓存，红线 11）。
//
// 每个实例 = 一个具体函数：符号名走 §八 的 aic_<pkg>_<T>__<实参>（MangleGeneric），
// 签名与体在**类型代换上下文**里发射（T → i32），故实例化的 C 里没有 T 残留。
//
// 实例集合来自检查器登记的 FuncInsts（同键只登记一次 = 实例缓存），跨包汇总后
// 按 (包, 名字, 实参) 排序发射 —— map 序随机会破坏 H2 的逐字节一致。
// ---------------------------------------------------------------------------

// funcInstEntry 是一个待发射的泛型函数实例。
type funcInstEntry struct {
	name string
	pkg  string
	fn   string
	sig  *types.FuncSig
	args []types.Type
	// params 是代换用的类型形参名表：泛型函数 = 自己的；**泛型类方法** = 所属类的
	// （方法不得自带类型形参，§四：继承类的）。
	params []string
}

// funcInstName 是实例的 C 符号名（键与名同源，§十 10.2）。
func funcInstName(pkg string, sig *types.FuncSig, args []types.Type) string {
	segs := make([]string, 0, len(args))
	for _, a := range args {
		segs = append(segs, typeSeg(a))
	}
	return MangleGeneric(pkg, sig.Name, segs)
}

// collectFuncInsts 汇总各包登记的泛型函数实例（pass 1）。
func (c *Ctx) collectFuncInsts(u *Unit) {
	if c.funcInstSeen == nil {
		c.funcInstSeen = map[string]bool{}
	}
	for _, fu := range u.Files {
		if fu.Info == nil {
			continue
		}
		for _, fi := range fu.Info.FuncInsts {
			if fi.Fn == nil {
				continue
			}
			// **泛型类方法的实例走它自己的发射路径**（genericmethod.go：原型与体都在那里
			// 按实例类型发）。FuncInsts 里出现方法实例是给 AIR/mair 用的（单态化要按实例
			// 各降一份体）—— 这里跳过，否则会按"自由函数"发一份没有接收者的原型。
			if fi.Fn.Recv != "" {
				continue
			}
			name := funcInstName(fu.Info.Pkg, fi.Fn, fi.Args)
			if c.funcInstSeen[name] {
				continue
			}
			c.funcInstSeen[name] = true
			params := fi.Fn.TypeParams
			if fi.Fn.Recv != "" {
				if cl, ok := fu.Info.Classes[fi.Fn.Recv]; ok && len(cl.TypeParams) > 0 {
					params = cl.TypeParams
				}
			}
			c.funcInsts = append(c.funcInsts, funcInstEntry{
				name: name, pkg: fu.Info.Pkg, fn: fi.Fn.Name, sig: fi.Fn, args: fi.Args, params: params,
			})
		}
	}
	sort.Slice(c.funcInsts, func(i, j int) bool { return c.funcInsts[i].name < c.funcInsts[j].name })
}

// emitFuncInsts 发射全部泛型函数实例（原型 + 定义），在原型段之前。
func (c *Ctx) emitFuncInsts(u *Unit) error {
	if len(c.funcInsts) == 0 {
		return nil
	}
	// 实例体要用到类型定义（含实例 typedef），故此处只发原型；体在主函数体之后发。
	for _, e := range c.funcInsts {
		inst := types.SubstSig(e.sig, paramMapOf(e.params, e.args))
		c.line("static %s %s(%s);", c.retCT(inst), e.name, c.paramList(inst))
	}
	c.line("")
	return nil
}

// emitFuncInstBodies 发射实例的函数体（在全部普通函数体之后、入口之前）。
func (c *Ctx) emitFuncInstBodies(u *Unit) error {
	for _, e := range c.funcInsts {
		if err := c.emitOneFuncInst(u, e); err != nil {
			return err
		}
	}
	return nil
}

// emitOneFuncInst 发射一个实例的函数体：签名代换 + 体在代换上下文里发射。
func (c *Ctx) emitOneFuncInst(u *Unit, e funcInstEntry) error {
	recv := ""
	if e.sig != nil {
		recv = e.sig.Recv
	}
	decl := findFuncDecl(u, e.pkg, e.fn, recv)
	if decl == nil || decl.Body == nil {
		return fmt.Errorf("emit: generic instance %s has no body to specialise", e.name)
	}
	inst := types.SubstSig(e.sig, paramMapOf(e.params, e.args))
	fu := fileUnitOf(u, e.pkg, e.fn, recv)
	if fu == nil {
		return fmt.Errorf("emit: generic instance %s: declaring file not found", e.name)
	}
	c.use(fu)
	return c.withSubst(e.params, e.args, func() error {
		savedName, savedFn, savedRet, savedLoop := c.curFuncName, c.curFunc, c.retVar, c.loopDepth
		savedDefers, savedSubst := c.deferCur, c.subst
		c.curFuncName, c.curFunc, c.retVar, c.loopDepth = types.FuncKey(inst.Recv, inst.Name), inst, "", 0
		c.subst = nil
		st, err := c.collectDeferSites(inst, decl.Body)
		if err != nil {
			return err
		}
		c.deferCur = st
		c.collectLambdas(decl.Body)
		if err := c.emitLambdaDefs(); err != nil {
			return err
		}
		if err := c.emitDeferTrampolines(st); err != nil {
			return err
		}
		c.srcLine(decl.Pos)
		c.line("static %s %s(%s) {", c.retCT(inst), e.name, c.paramList(inst))
		if st.stack != "" {
			c.line("    aic_defer_stack %s = AIC_DEFER_STACK_EMPTY;", st.stack)
		}
		if len(inst.Results) > 1 {
			c.retVar = c.tmp("ret")
			c.line("    %s %s;", c.retStructName(inst), c.retVar)
			c.line("    memset(&%s, 0, sizeof(%s));", c.retVar, c.retVar)
		}
		if err := c.emitBody(decl.Body, inst.HasErrResult()); err != nil {
			return err
		}
		c.emitDeferRun()
		c.line("}")
		c.line("")
		c.curFuncName, c.curFunc, c.retVar, c.loopDepth = savedName, savedFn, savedRet, savedLoop
		c.deferCur, c.subst = savedDefers, savedSubst
		return nil
	})
}

// paramMap 把实例实参表还原成类型形参映射（SubstSig 的输入形态）。
// paramMapOf 按给定的形参名表建代换映射（泛型类方法的实例用**类的**形参名表）。
func paramMapOf(params []string, args []types.Type) map[string]types.Type {
	m := make(map[string]types.Type, len(params))
	for i, p := range params {
		if i < len(args) {
			m[p] = args[i]
		}
	}
	return m
}

// findFuncDecl 在整程序里找某个包的自由函数声明（实例体发射的源码来源）。
func findFuncDecl(u *Unit, pkg, name, recv string) *parse.FuncDecl {
	for _, fu := range u.Files {
		if fu.Info == nil || fu.Info.Pkg != pkg {
			continue
		}
		for _, d := range fu.File.Decls {
			if recv == "" {
				if fd, ok := d.(*parse.FuncDecl); ok && fd.Name == name && fd.Recv == "" {
					return fd
				}
				continue
			}
			// 泛型**类方法**实例：体在 ClassDecl 里（方法不得自带类型形参，§四）。
			if cd, ok := d.(*parse.ClassDecl); ok && cd.Name == recv {
				for _, m := range cd.Methods {
					if m.Name == name {
						return m
					}
				}
			}
		}
	}
	return nil
}

// fileUnitOf 找某个包的文件单元（切换发射上下文用）。
func fileUnitOf(u *Unit, pkg, name, recv string) *FileUnit {
	for _, fu := range u.Files {
		if fu.Info == nil || fu.Info.Pkg != pkg {
			continue
		}
		for _, d := range fu.File.Decls {
			if recv == "" {
				if fd, ok := d.(*parse.FuncDecl); ok && fd.Name == name && fd.Recv == "" {
					return fu
				}
				continue
			}
			if cd, ok := d.(*parse.ClassDecl); ok && cd.Name == recv {
				for _, m := range cd.Methods {
					if m.Name == name {
						return fu
					}
				}
			}
		}
	}
	return nil
}

// genericHead 报告 `Name[...]` 是否是泛型实例化的头部（头部名字是带 [T] 的类）。
func (c *Ctx) genericHead(ix *parse.Index) (*types.Class, bool) {
	id, ok := ix.X.(*parse.Ident)
	if !ok || c.Info == nil {
		return nil, false
	}
	cl, isCl := c.Info.Classes[id.Name]
	if !isCl || len(cl.TypeParams) == 0 {
		return nil, false
	}
	return cl, true
}

// genericCallName 给泛型函数调用点算实例名（与检查器同一套推断，禁第二份实现）。
func (c *Ctx) genericCallName(pkg string, sig *types.FuncSig, argTypes []types.Type) (string, *types.FuncSig, bool) {
	m := types.InferArgs(sig, argTypes)
	if m == nil {
		return "", nil, false
	}
	args := make([]types.Type, 0, len(sig.TypeParams))
	for _, p := range sig.TypeParams {
		args = append(args, m[p])
	}
	return funcInstName(pkg, sig, args), types.SubstSig(sig, m), true
}
