package emit

import (
	"fmt"
	"strings"

	"aic/internal/air"
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 函数/方法签名、返回结构、原型、入口包装 。
//
// 返回形状: 0 个 = void; 1 个 = 该类型; ≥2 个 = 合成 struct aic_r_<形状>
// (元组非一等类型, 只是调用约定糖; Err 只占末位)。
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
// 规则: extern/export = 原名直出; 其余 = aic_<pkg>_<name>, 方法加类型名。
func (c *Ctx) cFuncName(sig *types.FuncSig) string {
	if sig.Extern || sig.Export {
		return ExternName(sig.Name)
	}
	return MangleFunc(c.pkg(), sig.Recv, sig.Name)
}

// paramList 返回 C 形参表文本 (方法首位补 this 为第一参数)。
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
//
// **IR 路径**：函数体一律从 IR 取（emitIRFuncs），其签名由 irRetCT/irParamList
// 渲染（含 [T;N] 出参 ABI）。原型若走 AST 侧渲染就会与定义不一致（审计 D11：
// 原型 `aic_i32[3] f(void)` vs 定义 `void f(aic_i32 out[3])` ⇒ C 直接拒）。
// 故 IR 体存在时原型也按 IR 渲染（同源同形）。
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
		retCT, params, name := c.retCT(sig), c.paramList(sig), c.cFuncName(sig)
		if c.ir != nil {
			if sym, ok := c.irBodySymFor(sig); ok {
				r, err := c.irRetCT(c.ir.Bodies[sym])
				if err != nil {
					return err
				}
				p, err := c.irParamList(c.ir.Bodies[sym])
				if err != nil {
					return err
				}
				retCT, params, name = r, p, c.irFuncCName(sym, sig)
			}
		}
		c.line("%s%s %s(%s);", c.storageClass(sig), retCT, name, params)
	}
	c.line("")
	return nil
}

// irBodySymFor 找签名对应的 IR 体符号（普通函数 / 类方法两种拼法；找不到 = false）。
func (c *Ctx) irBodySymFor(sig *types.FuncSig) (string, bool) {
	for _, sym := range []string{
		air.FuncSym(c.pkg(), "", sig.Name),
		air.FuncSym(c.pkg(), sig.Recv, sig.Name),
	} {
		if c.ir != nil {
			if _, has := c.ir.Bodies[sym]; has {
				return sym, true
			}
		}
	}
	return "", false
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

// emitEntry 发射 C 入口: 建任务根区域 + os 参数 + Err 出口 (main 内逃逸 =
// 退出码 1 + stderr 打印 err.msg)。
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
		// 调度层起手初始化（存活任务计数 / 串行模式令牌）。只在用到 L2 时发。
		c.line("    aic_sched_init();")
	}
	c.line("    aic_os_init(argc, argv);")
	if sig.HasErrResult() {
		c.line("    aic_Err _err = %s();", mainSym)
		c.line("    if (_err.code != 0) {")
		// cause 链逐级打印（§16 ④ 规则 5）；无链时与原来逐字节一致。
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

// ---------------------------------------------------------------------------
// 变体构造值与 init 构造方法（原 call.go：AST 路径已删，这里只剩 IR 助手）。
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 调用发射 (容器方法 / str / 构造 / Err / std)。
//
// 约定: 一切调用都在编译期定型 (泛型单态化 + 静态派发, CPU 条款),
// 运行时函数名来自各自的单表 (容器后缀表 / std 名表 / mangling 表)。
// ---------------------------------------------------------------------------

// lookupVariant 在本包 enum 中找变体构造名 (单态化: Option 的 Some 亦在此二义,
// 由检查器已定型的负载类型区分)。
func (c *Ctx) lookupVariant(name string) (*types.Enum, types.Type, bool) {
	if c.Info == nil {
		return nil, nil, false
	}
	for _, en := range c.Info.Enums {
		if p, ok := en.VariantPayload(name); ok {
			return en, p, true
		}
	}
	return nil, nil, false
}

// variantNoData 发射无数据变体的**值**：无数据 enum 就是该 C 枚举常量本身；
// 带数据 enum 的值类型是 {tag, union} 结构，必须补成结构字面量——直接把
// tag 常量当值用会让 C 报「参数类型不符」（枚举常量是 tag 枚举类型，不是结构）。
func (c *Ctx) variantNoData(en *types.Enum, ty, variant string) string {
	if en.HasData {
		return fmt.Sprintf("((%s){ .tag = %s_%s })", ty, ty, variant)
	}
	return fmt.Sprintf("%s_%s", ty, variant)
}

// initMethod 取类的 init 构造函数 (至多一个、无重载、可 -> Err)。
func (c *Ctx) initMethod(cl *types.Class) (*types.FuncSig, bool) {
	for name := range cl.Methods {
		if sig, ok := cl.Method(name); ok && sig.IsInit {
			return sig, true
		}
	}
	return nil, false
}

// ---------------------------------------------------------------------------
// 跨包查询助手（原 depcall.go：AST 路径已删）。
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// 跨包调用（可见性、 包管理器）。
//
// 调用点所在的包 ≠ 被调函数所在的包，故符号名必须用**被调包**的包名拼
// （c.pkg() 是调用者的包名，直接用会拼错）——mangling 单表照旧，只是喂对包名。
// ---------------------------------------------------------------------------

// depFuncResults 取跨包函数的结果类型（emit 定宽/选打印原语用）。
func (c *Ctx) depFuncResults(pkg, fn string) ([]types.Type, bool) {
	dep := c.depInfo(pkg)
	if dep == nil {
		return nil, false
	}
	sig, ok := dep.Funcs[fn]
	if !ok || sig == nil {
		return nil, false
	}
	return sig.Results, true
}

// depNamedType 取跨包具名类型（类型位 util.Reader / util.Box[i32] 用）。
func (c *Ctx) depNamedType(pkg, name string) types.Type {
	dep := c.depInfo(pkg)
	if dep == nil {
		return nil
	}
	if cl, ok := dep.Classes[name]; ok {
		return cl
	}
	if en, ok := dep.Enums[name]; ok {
		return en
	}
	if ifc, ok := dep.Interfaces[name]; ok {
		return ifc
	}
	return nil
}
