package emit

import (
	"fmt"
	"sort"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 泛型类实例的方法（T1）：实例的每个方法都是具体函数
//   aic_<pkg>_<Cls>_<method>__<实参…>
// 签名与体在类型代换上下文里发射（this 的类型 = 实例 typedef）。
// ---------------------------------------------------------------------------

// instMethodEntry 是一个待发射的实例方法。
type instMethodEntry struct {
	name string
	cl   *types.Class
	inst *types.Instance
	decl *parse.FuncDecl
	fu   *FileUnit
	sig  *types.FuncSig
}

// collectInstMethods 汇总全部类实例的方法（按符号名排序，保 H2 确定性）。
func (c *Ctx) collectInstMethods(u *Unit) []instMethodEntry {
	var out []instMethodEntry
	for _, e := range c.insts {
		cl, isCl := e.inst.Base.(*types.Class)
		if !isCl {
			continue
		}
		segs := make([]string, 0, len(e.inst.Args))
		for _, a := range e.inst.Args {
			segs = append(segs, typeSeg(a))
		}
		for _, fu := range u.Files {
			if fu.Info == nil {
				continue
			}
			if bcl, ok := fu.Info.Classes[cl.Name]; !ok || bcl != cl {
				continue
			}
			for _, d := range fu.File.Decls {
				cd, isCD := d.(*parse.ClassDecl)
				if !isCD || cd.Name != cl.Name {
					continue
				}
				for _, m := range cd.Methods {
					if m.IsInit {
						continue // init 是构造函数，归构造路径
					}
					sig, has := cl.Method(m.Name)
					if !has || sig == nil {
						continue
					}
					out = append(out, instMethodEntry{
						name: MangleGeneric(c.ownerPkg(cl.Pkg), cl.Name+"_"+m.Name, segs),
						cl:   cl, inst: e.inst, decl: m, fu: fu,
						sig: types.SubstSig(sig, paramMapOf(cl.TypeParams, e.inst.Args)),
					})
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// instParamList 是实例方法的 C 形参表：this__ 用**实例**类型（不是泛型类），
// 其余形参按实例代换（T → i32）。
func (c *Ctx) instParamList(m instMethodEntry) string {
	parts := make([]string, 0, len(m.sig.Params)+1)
	parts = append(parts, fmt.Sprintf("%s this__", c.cTypeName(m.inst)))
	for i, p := range m.sig.Params {
		pt := "int"
		if i < len(m.sig.ParamTypes) && m.sig.ParamTypes[i] != nil {
			parts = append(parts, c.cTypeDecl(m.sig.ParamTypes[i], p))
			continue
		}
		parts = append(parts, pt+" "+p)
	}
	return joinComma(parts)
}

// emitInstMethodProtos 发射实例方法原型（在实例 typedef 之后、原型段之前）。
func (c *Ctx) emitInstMethodProtos(u *Unit) ([]instMethodEntry, error) {
	ms := c.collectInstMethods(u)
	for _, m := range ms {
		if err := c.withSubst(m.cl.TypeParams, m.inst.Args, func() error {
			c.use(m.fu)
			c.line("static %s %s(%s);", c.retCT(m.sig), m.name, c.instParamList(m))
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if len(ms) > 0 {
		c.line("")
	}
	return ms, nil
}

// emitInstMethodBodies 发射实例方法体（在普通函数体之后）。
func (c *Ctx) emitInstMethodBodies(ms []instMethodEntry) error {
	for _, m := range ms {
		if err := c.emitOneInstMethod(m); err != nil {
			return err
		}
	}
	return nil
}

// emitOneInstMethod 发射一个实例方法的体。
func (c *Ctx) emitOneInstMethod(m instMethodEntry) error {
	c.use(m.fu)
	return c.withSubst(m.cl.TypeParams, m.inst.Args, func() error {
		savedName, savedFn, savedRet, savedLoop := c.curFuncName, c.curFunc, c.retVar, c.loopDepth
		savedDefers, savedSubst := c.deferCur, c.subst
		c.curFuncName, c.curFunc, c.retVar, c.loopDepth = types.FuncKey(m.cl.Name, m.sig.Name), m.sig, "", 0
		c.subst = nil
		st, err := c.collectDeferSites(m.sig, m.decl.Body)
		if err != nil {
			return err
		}
		c.deferCur = st
		c.collectLambdas(m.decl.Body)
		if err := c.emitLambdaDefs(); err != nil {
			return err
		}
		if err := c.emitDeferTrampolines(st); err != nil {
			return err
		}
		c.srcLine(m.decl.Pos)
		c.line("static %s %s(%s) {", c.retCT(m.sig), m.name, c.instParamList(m))
		if st.stack != "" {
			c.line("    aic_defer_stack %s = AIC_DEFER_STACK_EMPTY;", st.stack)
		}
		if len(m.sig.Results) > 1 {
			c.retVar = c.tmp("ret")
			c.line("    %s %s;", c.retStructName(m.sig), c.retVar)
			c.line("    memset(&%s, 0, sizeof(%s));", c.retVar, c.retVar)
		}
		if err := c.emitBody(m.decl.Body, m.sig.HasErrResult()); err != nil {
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

// instMethodCallName 给实例方法调用点算符号名（与实例发射同一张表）。
func (c *Ctx) instMethodCallName(inst *types.Instance, cl *types.Class, method string) string {
	segs := make([]string, 0, len(inst.Args))
	for _, a := range inst.Args {
		segs = append(segs, typeSeg(a))
	}
	return MangleGeneric(c.ownerPkg(cl.Pkg), cl.Name+"_"+method, segs)
}

// instMethodSig 取实例方法的代换签名（调用点定宽用）。
func instMethodSig(cl *types.Class, inst *types.Instance, method string) (*types.FuncSig, bool) {
	sig, has := cl.Method(method)
	if !has || sig == nil {
		return nil, false
	}
	return types.SubstSig(sig, paramMapOf(cl.TypeParams, inst.Args)), true
}

var _ = fmt.Sprintf
