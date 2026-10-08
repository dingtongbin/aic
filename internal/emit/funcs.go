package emit

import (
	"fmt"
	"strings"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 函数/方法签名、返回结构、原型、入口包装 (核心设计 §四/§六/§八)。
//
// 返回形状: 0 个 = void; 1 个 = 该类型; ≥2 个 = 合成 struct aic_r_<形状>
// (元组非一等类型, 只是调用约定糖, §二.2; Err 只占末位, §六)。
// ---------------------------------------------------------------------------

// stdRetStructs 是 runtime/aic_std.h 已声明的合成返回结构（键 = 返回位的 mangling 段）。
// 这些形状必须**复用**运行时的声明：同一形状发两份 typedef 会撞名（687/718 的实际故障）。
var stdRetStructs = map[string]string{
	"i64_Err":   "aic_r_i64_err",
	"f64_Err":   "aic_r_f64_err",
	"str_Err":   "aic_r_str_err",
	"u32_usize": "aic_r_u32_usize",
}

// stdRetStruct 报告该返回形状是否由运行时声明（是则返回其 C 名）。
func stdRetStruct(elems []types.Type) (string, bool) {
	if len(elems) < 2 {
		return "", false
	}
	parts := make([]string, 0, len(elems))
	for _, r := range elems {
		parts = append(parts, typeSeg(r))
	}
	name, ok := stdRetStructs[strings.Join(parts, "_")]
	return name, ok
}

// retStructName 是合成返回结构的名字 (按元素的 C 名拼, 同一形状只发一次)。
func (c *Ctx) retStructName(sig *types.FuncSig) string {
	if n, ok := stdRetStruct(sig.Results); ok {
		return n
	}
	return c.retStructNameFor(sig.Results)
}

// retCT 返回函数在 C 侧的返回类型名。
func (c *Ctx) retCT(sig *types.FuncSig) string {
	switch len(sig.Results) {
	case 0:
		return "void"
	case 1:
		return c.cTypeName(sig.Results[0])
	default:
		return c.retStructName(sig)
	}
}

// cFuncName 返回函数/方法在 C 侧的符号名。
// 规则 (§八): extern/export = 原名直出; 其余 = aic_<pkg>_<name>, 方法加类型名。
func (c *Ctx) cFuncName(sig *types.FuncSig) string {
	if sig.Extern || sig.Export {
		return ExternName(sig.Name)
	}
	return MangleFunc(c.pkg(), sig.Recv, sig.Name)
}

// paramList 返回 C 形参表文本 (方法首位补 this, §八: this 为第一参数)。
func (c *Ctx) paramList(sig *types.FuncSig) string {
	parts := make([]string, 0, len(sig.Params)+1)
	if sig.Recv != "" {
		if cls := c.recvClass(sig.Recv); cls != nil {
			parts = append(parts, fmt.Sprintf("%s this__", c.cTypeName(cls)))
		}
	}
	for i, p := range sig.Params {
		pt := "int"
		if i < len(sig.ParamTypes) && sig.ParamTypes[i] != nil {
			// 用声明子形态：函数类型形参必须写 `R (*g)(P…)`，
			// `R (*)(P…) g` 不是合法的 C 形参声明。
			parts = append(parts, c.cTypeDecl(sig.ParamTypes[i], p))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %s", pt, p))
	}
	if len(parts) == 0 {
		return "void"
	}
	return strings.Join(parts, ", ")
}

// paramListNoThis 返回不含接收者的形参表（trampoline 用：首参是 aic_iface，
// 具体方法由 trampoline 内部提供 this，故形参表里不能再出现 this__）。
func (c *Ctx) paramListNoThis(sig *types.FuncSig) string {
	parts := make([]string, 0, len(sig.Params))
	for i, p := range sig.Params {
		if i < len(sig.ParamTypes) && sig.ParamTypes[i] != nil {
			parts = append(parts, c.cTypeDecl(sig.ParamTypes[i], p))
			continue
		}
		parts = append(parts, "int "+p)
	}
	if len(parts) == 0 {
		return "void"
	}
	return strings.Join(parts, ", ")
}

// recvClass 取接收者类 (方法发射需要 this 的类型)。
func (c *Ctx) recvClass(name string) *types.Class {
	if c.Info == nil {
		return nil
	}
	cl, _ := c.Info.Classes[name]
	return cl
}

// emitRetStructs 为所有 ≥2 返回的函数发射合成返回结构（跨文件/跨包全局去重：
// 同一形状只允许一份 typedef，否则 C 层直接撞名）。
func (c *Ctx) emitRetStructs(sigs []*types.FuncSig) {
	seen := c.retSeen
	emitted := false
	for _, sig := range sigs {
		if len(sig.Results) < 2 {
			continue
		}
		name := c.retStructName(sig)
		if seen[name] {
			continue
		}
		seen[name] = true
		if _, isStd := stdRetStruct(sig.Results); isStd {
			continue // 运行时已声明（aic_std.h），不重复 typedef
		}
		c.line("typedef struct %s {", name)
		for i, r := range sig.Results {
			c.line("    %s _%d;", c.cTypeName(r), i)
		}
		c.line("} %s;", name)
		emitted = true
	}
	if emitted {
		c.line("")
	}
}

// allSigs 按声明序收集本翻译单元的全部函数签名 (自由函数 + 方法 + 入口)。
func (c *Ctx) allSigs() []*types.FuncSig {
	out := make([]*types.FuncSig, 0, len(c.File.Decls))
	if c.Info == nil {
		return out
	}
	for _, d := range c.File.Decls {
		switch v := d.(type) {
		case *parse.FuncDecl:
			if sig, ok := c.Info.Funcs[v.Name]; ok {
				out = append(out, sig)
			}
		case *parse.ClassDecl:
			if len(v.TypeParams) > 0 {
				continue // 泛型类的方法按实例发射（emitInstMethodBodies）
			}
			cl := c.Info.Classes[v.Name]
			for _, m := range v.Methods {
				if cl == nil {
					continue
				}
				if sig, ok := cl.Method(m.Name); ok {
					out = append(out, sig)
				}
			}
		}
	}
	return out
}

// isMain 报告该签名是否为 AIC 入口 (发射为 static, 只被 C wrapper 调用)。
func (c *Ctx) isMain(sig *types.FuncSig) bool {
	return sig != nil && sig.Name == "main" && sig.Recv == "" && !sig.Extern
}

// emitProtos 发射合成返回结构与全部函数原型 (C 无前向声明也能编译, 但原型让
// 相互递归与声明序无关, 且是 extern 绑定的唯一落点)。
func (c *Ctx) emitProtos() error {
	sigs := c.allSigs()
	c.emitRetStructs(sigs)
	for _, sig := range sigs {
		if sig.IsInit {
			continue // init 是构造函数, 发射为返回本类指针的工厂 (见 emitFunc)
		}
		if len(sig.TypeParams) > 0 {
			continue // 泛型函数按实例发射（emitFuncInsts）；本体不直接发符号
		}
		if c.isMain(sig) {
			// 入口只被文件末尾的 wrapper 调用, 定义在 wrapper 之前:
			// 发原型会让 static 定义跟在非 static 声明之后 (C 不允许)。
			continue
		}
		c.line("%s%s %s(%s);", c.storageClass(sig), c.retCT(sig), c.cFuncName(sig), c.paramList(sig))
	}
	c.line("")
	return nil
}

// storageClass 返回函数定义的存储类（10.4 第 4 条：生成函数一律 static）。
// 例外：入口 main、export（零 mangle 外部链接）、extern 声明（无体，不在此路径）。
func (c *Ctx) storageClass(sig *types.FuncSig) string {
	if sig == nil {
		return ""
	}
	if sig.Export || sig.Extern || c.isMain(sig) {
		return ""
	}
	return "static "
}

// emitFuncs 发射本文件的全部函数体（入口 wrapper 由整程序发射在末尾统一发）。
func (c *Ctx) emitFuncs() error {
	for _, d := range c.File.Decls {
		switch v := d.(type) {
		case *parse.FuncDecl:
			if len(v.TypeParams) > 0 {
				continue // 泛型函数：按实例发射（emitFuncInstBodies）
			}
			if err := c.emitOneFunc(v, nil); err != nil {
				return err
			}
		case *parse.ClassDecl:
			if len(v.TypeParams) > 0 {
				continue // 泛型类的方法按实例发射（emitInstMethodBodies）
			}
			for _, m := range v.Methods {
				if err := c.emitOneFunc(m, v); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// emitOneFunc 发射一个函数/方法体。
func (c *Ctx) emitOneFunc(v *parse.FuncDecl, cls *parse.ClassDecl) error {
	if c.Info == nil {
		return fmt.Errorf("emit: missing the checker output")
	}
	var sig *types.FuncSig
	if cls != nil {
		cl := c.Info.Classes[cls.Name]
		if cl == nil {
			return fmt.Errorf("emit: unknown class %s", cls.Name)
		}
		sig, _ = cl.Method(v.Name)
	} else {
		sig, _ = c.Info.Funcs[v.Name]
	}
	if sig == nil {
		return fmt.Errorf("emit: unknown function %s", v.Name)
	}
	if v.Extern && v.Body == nil {
		return nil // extern 只有原型, 无函数体 (§八)
	}
	// 入口 main 由 wrapper 承载: 定义发射为 static (原型不发射, 见 emitProtos)。
	if c.isMain(sig) {
		return c.emitMainBody(v, sig)
	}
	// 事实按函数重置（健全性）：长度/守卫/归纳事实只在**本函数体内**有效，
	// 跨函数复用会把「A 里填满」错当成「B 里的容器也填满了」（红线 13）。
	c.lenSym, c.lenFacts, c.appendFacts, c.loopCondBound, c.loopLenBound = nil, nil, nil, nil, nil
	// F5：已声明变量集合按函数重置，并把形参视为已声明。
	c.declaredVars = map[string]bool{}
	for _, p := range sig.Params {
		c.declaredVars[p] = true
	}
	if sig.Recv != "" {
		c.declaredVars["this__"] = true
	}
	savedName, savedFn, savedRet, savedLoop := c.curFuncName, c.curFunc, c.retVar, c.loopDepth
	savedDefers, savedSubst := c.deferCur, c.subst
	c.curFuncName = types.FuncKey(sig.Recv, sig.Name)
	c.curFunc = sig
	c.retVar = ""
	c.loopDepth = 0
	c.subst = nil
	st, err := c.collectDeferSites(sig, v.Body)
	if err != nil {
		return err
	}
	c.deferCur = st
	c.collectLambdas(v.Body)
	if err := c.emitLambdaDefs(); err != nil {
		return err
	}
	if err := c.emitDeferTrampolines(st); err != nil {
		return err
	}
	c.srcLine(v.Pos)
	// 10.4 第 4 条：生成函数一律 static（除 main / export）；同一 TU 内联 + 未被引用
	// 的实例由 C 编译器整段丢弃（§10.2 膨胀控制第 ③ 道闸）。
	c.line("%s%s %s(%s) {", c.storageClass(sig), c.retCT(sig), c.cFuncName(sig), c.paramList(sig))
	if st.stack != "" {
		c.line("    aic_defer_stack %s = AIC_DEFER_STACK_EMPTY;", st.stack)
	}
	if len(sig.Results) > 1 {
		c.retVar = c.tmp("ret")
		c.line("    %s %s;", c.retStructName(sig), c.retVar)
		c.line("    memset(&%s, 0, sizeof(%s));", c.retVar, c.retVar)
	}
	if err := c.emitBody(v.Body, sig.HasErrResult()); err != nil {
		return err
	}
	c.emitDeferRun()
	c.line("}")
	c.line("")
	c.curFuncName, c.curFunc, c.retVar, c.loopDepth = savedName, savedFn, savedRet, savedLoop
	c.deferCur, c.subst = savedDefers, savedSubst
	return nil
}

// emitMainBody 发射 AIC 侧入口: 符号名走同一张 mangling 表 (aic_<pkg>_main),
// 由 wrapper 调用, 避免与 C 的 main 重名。
func (c *Ctx) emitMainBody(v *parse.FuncDecl, sig *types.FuncSig) error {
	savedName, savedFn, savedRet, savedLoop := c.curFuncName, c.curFunc, c.retVar, c.loopDepth
	savedDefers, savedSubst := c.deferCur, c.subst
	c.curFuncName, c.curFunc, c.retVar, c.loopDepth = "main", sig, "", 0
	c.subst = nil
	st, err := c.collectDeferSites(sig, v.Body)
	if err != nil {
		return err
	}
	c.deferCur = st
	c.collectLambdas(v.Body)
	if err := c.emitLambdaDefs(); err != nil {
		return err
	}
	if err := c.emitDeferTrampolines(st); err != nil {
		return err
	}
	c.srcLine(v.Pos)
	c.line("static %s %s(void) {", c.retCT(sig), c.cFuncName(sig))
	if st.stack != "" {
		c.line("    aic_defer_stack %s = AIC_DEFER_STACK_EMPTY;", st.stack)
	}
	if err := c.emitBody(v.Body, sig.HasErrResult()); err != nil {
		return err
	}
	c.emitDeferRun()
	c.line("}")
	c.line("")
	c.curFuncName, c.curFunc, c.retVar, c.loopDepth = savedName, savedFn, savedRet, savedLoop
	c.deferCur, c.subst = savedDefers, savedSubst
	return nil
}

// emitEntry 发射 C 入口: 建任务根区域 + os 参数 + Err 出口 (main 内逃逸 =
// 退出码 1 + stderr 打印 err.msg, §六)。
func (c *Ctx) emitEntry() {
	if c.Info == nil {
		return
	}
	sig, ok := c.Info.Funcs["main"]
	if !ok {
		c.warn("no func main: the artifact has no entry point (library-style translation unit)")
		return
	}
	line := 1
	for _, d := range c.File.Decls {
		if fd, isFn := d.(*parse.FuncDecl); isFn && fd.Name == "main" {
			line = fd.Pos.Line
			break
		}
	}
	mainSym := c.cFuncName(sig)
	c.srcLine(parse.Pos{File: c.Path, Line: line})
	c.line("int main(int argc, char **argv) {")
	c.line("    aic_task_init(%s, %d);", cstr(c.Path), line)
	if c.needed["l2"] {
		// 调度层起手初始化（存活任务计数 / 串行模式令牌）。只在用到 L2 时发（红线 16）。
		c.line("    aic_sched_init();")
	}
	c.line("    aic_os_init(argc, argv);")
	if sig.HasErrResult() {
		c.line("    aic_Err _err = %s();", mainSym)
		c.line("    if (_err.code != 0) {")
		// cause 链逐级打印（§16 N5 ④ 规则 5）；无链时与原来逐字节一致。
		c.line("        aic_err_report(_err); /* stderr：规范「退出码 1 + stderr」 */")
		c.line("        return 1;")
		c.line("    }")
		c.line("    return 0;")
	} else {
		c.line("    %s();", mainSym)
		c.line("    return 0;")
	}
	c.line("}")
	c.line("")
}

// sanitize 把 C 类型名压成可做标识符片段的形式 (指针/空格/方括号 → 下划线)。
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}
