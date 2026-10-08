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

// collectInstMethods 汇总**已登记**的泛型类方法实例。
//
// 判据 = 检查器的 FuncInsts（真正被调用点引用过的实例，实例缓存与键的唯一来源），
// **不是**「类实例 × 全部方法」的笛卡尔积 —— 那个积里的多数实例 AIR 侧没有体
// （air 只降级登记过的实例），irOneFunc 会报 no IR body（741 实测）。
func (c *Ctx) collectInstMethods(u *Unit) []instMethodEntry {
	var out []instMethodEntry
	seen := map[string]bool{}
	for _, fu := range u.Files {
		if fu.Info == nil {
			continue
		}
		for _, fi := range fu.Info.FuncInsts {
			if fi.Fn == nil || fi.Fn.Recv == "" || len(fi.Args) == 0 {
				continue
			}
			cl, ok := fu.Info.Classes[fi.Fn.Recv]
			if !ok || len(cl.TypeParams) != len(fi.Args) {
				continue // 非泛型类的方法不走实例路径
			}
			decl := findMethodDecl(u, fu.Info.Pkg, fi.Fn.Recv, fi.Fn.Name)
			if decl == nil || decl.Body == nil {
				continue
			}
			inst := &types.Instance{Base: cl, Args: fi.Args}
			name := MangleGeneric(c.ownerPkg(cl.Pkg), cl.Name+"_"+decl.Name, instSegs(inst))
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, instMethodEntry{
				name: name,
				cl:   cl, inst: inst, decl: decl, fu: fu,
				sig: types.SubstSig(fi.Fn, paramMapOf(cl.TypeParams, fi.Args)),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// instSegs 是实例实参的短后缀表（键与名同源：符号名由实参拼出）。
func instSegs(inst *types.Instance) []string {
	segs := make([]string, 0, len(inst.Args))
	for _, a := range inst.Args {
		segs = append(segs, typeSeg(a))
	}
	return segs
}

// findMethodDecl 在整程序里找某个类的某个方法声明（实例体的源码来源）。
func findMethodDecl(u *Unit, pkg, cls, name string) *parse.FuncDecl {
	for _, fu := range u.Files {
		if fu.Info == nil || fu.Info.Pkg != pkg || fu.File == nil {
			continue
		}
		for _, d := range fu.File.Decls {
			if cd, ok := d.(*parse.ClassDecl); ok && cd.Name == cls {
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
