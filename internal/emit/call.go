package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 调用发射 (核心设计 §二.2 容器方法 / §二.3 str / §四 构造 / §六 Err / §十四 std)。
//
// 约定: 一切调用都在编译期定型 (泛型单态化 + 静态派发, §〇 CPU 条款),
// 运行时函数名来自各自的单表 (容器后缀表 / std 名表 / mangling 表)。
// ---------------------------------------------------------------------------

// call 发射调用表达式。
func (c *Ctx) call(v *parse.Call, want types.Type) (string, error) {
	if e, handled, err := c.tryStdCall(v, want); handled {
		return e, err
	}
	if e, handled, err := c.tryMutexCall(v, want); handled {
		return e, err
	}
	if e, handled, err := c.tryChanCall(v, want); handled {
		return e, err
	}
	if e, handled, err := c.tryBuiltinCall(v, want); handled {
		return e, err
	}
	if e, handled, err := c.tryMethodCall(v, want); handled {
		return e, err
	}
	return c.tryFuncCall(v, want)
}

// tryBuiltinCall: 内建构造与转换 (Err/T(x)/C(args)/变体构造)。
// 返回 (表达式, 是否已处理, 错误)。
func (c *Ctx) tryBuiltinCall(v *parse.Call, want types.Type) (string, bool, error) {
	// Err.wrap(code, msg, cause)：内建错误链构造 —— **必须在方法分派之前**认出来
	// （Err 不是类，走方法路径会报"接收者类型未知"）。
	if f, isField := v.Fn.(*parse.Field); isField {
		if base, isID := f.X.(*parse.Ident); isID && base.Name == "Err" && f.Name == "wrap" {
			return c.errCtor(v)
		}
	}
	// 限定变体构造 E.Variant(负载) / pkg.E.Variant(负载)：先于一切字段求值
	// （枚举名不是值，按接收者求值会失败）。
	if field, ok := v.Fn.(*parse.Field); ok {
		if en, payload, found := c.qualifiedVariant(field); found {
			if payload == nil {
				return "", true, fmt.Errorf("emit: this variant carries no payload (line %d)", v.Pos.Line)
			}
			if len(v.Args) != 1 {
				return "", true, fmt.Errorf("emit: a variant payload is exactly one value (line %d)", v.Pos.Line)
			}
			arg, err := c.expr(v.Args[0], payload)
			if err != nil {
				return "", true, err
			}
			return c.variantValue(en, field.Name, arg), true, nil
		}
	}
	id, ok := v.Fn.(*parse.Ident)
	if !ok {
		return "", false, nil
	}
	// Err(code, msg) / Err.wrap(code, msg, cause) —— 内建结构 {code i32, msg str, cause}
	// 按值（§六 / §16 N5 ④）。
	if id.Name == "Err" {
		return c.errCtor(v)
	}
	// Err.wrap(...)：Fn 是 Field{Err, wrap}
	if f, isField := v.Fn.(*parse.Field); isField {
		if base, isID := f.X.(*parse.Ident); isID && base.Name == "Err" && f.Name == "wrap" {
			return c.errCtor(v)
		}
	}
	// 显式数值转换 T(x): 仅数值↔数值; 运行期窄化越界 = trap (§二.4)
	if b, isBasic := basicByName(id.Name); isBasic {
		if tb, isBasicTy := b.(*types.Basic); isBasicTy && types.IsNumeric(tb) {
			if len(v.Args) != 1 {
				return "", true, fmt.Errorf("emit: a conversion takes one argument (line %d)", v.Pos.Line)
			}
			srcT := c.ti(v.Args[0])
			arg, err := c.expr(v.Args[0], srcT)
			if err != nil {
				return "", true, err
			}
			return c.conversion(tb, srcT, arg, v.Pos.Line), true, nil
		}
	}
	// std option 的 Some(x)：目标类型 = 期望类型 / 结果类型 / 实参类型
	if c.Info != nil && c.Info.Imported["option"] && id.Name == "Some" {
		if _, isOpt := c.someInstance(v, want); isOpt {
			if len(v.Args) != 1 {
				return "", true, fmt.Errorf("emit: Some takes exactly one payload (line %d)", v.Pos.Line)
			}
			e, err := c.optionSomeCT(v, v.Args[0], want)
			return e, true, err
		}
	}
	// 变体构造 Some(x) / Name(payload) —— 带数据 enum 的 {tag, union}
	if en, payload, isVar := c.lookupVariant(id.Name); isVar {
		if payload == nil {
			return "", true, fmt.Errorf("emit: this variant carries no payload (%s, line %d)", id.Name, v.Pos.Line)
		}
		if len(v.Args) != 1 {
			return "", true, fmt.Errorf("emit: a variant payload is exactly one value (line %d)", v.Pos.Line)
		}
		arg, err := c.expr(v.Args[0], payload)
		if err != nil {
			return "", true, err
		}
		return c.variantValue(en, id.Name, arg), true, nil
	}
	// 构造 C(args): 有 init 的类 = 工厂调用 (§四)
	if cl := c.recvClass(id.Name); cl != nil {
		if init, has := c.initMethod(cl); has {
			args, err := c.argsFor(v, init)
			if err != nil {
				return "", true, err
			}
			name := c.cMethodName(cl, init.Name)
			if len(init.Results) > 1 {
				return fmt.Sprintf("%s(%s)", name, args), true, nil
			}
			return fmt.Sprintf("%s(%s)", name, args), true, nil
		}
		return "", true, fmt.Errorf("emit: class %s has no init, use the composite literal %s{...} (line %d)",
			cl.Name, cl.Name, v.Pos.Line)
	}
	return "", false, nil
}

// errCtor 发射 `Err(code, msg)`（无链）与 `Err.wrap(code, msg, cause)`（建链）。
// C15：初始化列表的元素求值顺序未规定 ⇒ 按源码序绑临时量。
func (c *Ctx) errCtor(v *parse.Call) (string, bool, error) {
	if len(v.Args) == 2 {
		parts, err := c.orderedList(v.Args, []types.Type{types.TI32, types.TStr})
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("((aic_Err){ %s, %s, NULL })", parts[0], parts[1]), true, nil
	}
	if len(v.Args) == 3 {
		parts, err := c.orderedList(v.Args, []types.Type{types.TI32, types.TStr, types.TErr})
		if err != nil {
			return "", true, err
		}
		// 运行时把 cause 拷贝进当前区域（栈上的临时 Err 也能安全上链）。
		c.need("aic_err_wrap")
		return fmt.Sprintf("aic_err_wrap(%s, %s, %s)", parts[0], parts[1], parts[2]), true, nil
	}
	return "", true, fmt.Errorf("emit: Err construction needs (code, msg) or Err.wrap(code, msg, cause) (line %d)", v.Pos.Line)
}

// tryFuncCall: 自由函数调用 / lambda 变量调用。
func (c *Ctx) tryFuncCall(v *parse.Call, want types.Type) (string, error) {
	// 显式泛型实参的调用 `id[i32](x)`：Fn 是 Index 形态 ⇒ 按**写的**类型实参
	// 直接取实例符号（与检查器的 checkGenericCallExplicit 同一套实例化）。
	if ix, isIx := v.Fn.(*parse.Index); isIx {
		if id, isID := ix.X.(*parse.Ident); isID {
			if c.Info != nil {
				if sig, has := c.Info.Funcs[id.Name]; has && len(sig.TypeParams) > 0 {
					argExprs := append([]parse.Expr{ix.Index}, ix.TypeArgs...)
					if len(argExprs) != len(sig.TypeParams) {
						return "", fmt.Errorf("emit: %s needs %d type arguments (line %d)",
							id.Name, len(sig.TypeParams), v.Pos.Line)
					}
					m := map[string]types.Type{}
					for i, te := range argExprs {
						at := c.typeOfTypeExpr(typeExprOfExpr(te))
						if at == nil {
							return "", fmt.Errorf("emit: cannot resolve the type argument of %s (line %d)", id.Name, v.Pos.Line)
						}
						m[sig.TypeParams[i]] = at
					}
					// 与检查器同源的实例命名（funcInstName 是唯一定义）。
					args := make([]types.Type, 0, len(sig.TypeParams))
					for _, p := range sig.TypeParams {
						args = append(args, m[p])
					}
					name := funcInstName(c.pkg(), sig, args)
					isig := types.SubstSig(sig, m)
					argStr, err := c.argsFor(v, isig)
					if err != nil {
						return "", err
					}
					return fmt.Sprintf("%s(%s)", name, argStr), nil
				}
			}
		}
		return "", fmt.Errorf("emit: this call form is not supported yet (explicit type arguments on a non-generic callee, line %d)", v.Pos.Line)
	}
	id, ok := v.Fn.(*parse.Ident)
	if !ok {
		return "", fmt.Errorf("emit: unsupported call form (line %d)", v.Pos.Line)
	}
	if c.Info != nil {
		if sig, has := c.Info.Funcs[id.Name]; has {
			// 泛型函数：调用点发**实例**符号名（与检查器同一套推断，禁第二份）。
			if len(sig.TypeParams) > 0 {
				argTypes := make([]types.Type, 0, len(v.Args))
				for _, a := range v.Args {
					argTypes = append(argTypes, c.ti(a))
				}
				name, inst, ok := c.genericCallName(c.pkg(), sig, argTypes)
				if !ok {
					return "", fmt.Errorf("emit: cannot infer the type arguments of %s (line %d)", id.Name, v.Pos.Line)
				}
				args, err := c.argsFor(v, inst)
				if err != nil {
					return "", err
				}
				return fmt.Sprintf("%s(%s)", name, args), nil
			}
			args, err := c.argsFor(v, sig)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("%s(%s)", c.cFuncName(sig), args), nil
		}
	}
	// lambda 类型变量的调用 = C 函数指针调用 (§三)
	if c.Info != nil && c.Info.IsLocal(c.curFuncName, id.Name) {
		args, err := c.rawArgs(v)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s(%s)", id.Name, args), nil
	}
	return "", fmt.Errorf("emit: undeclared function %s (line %d)", id.Name, v.Pos.Line)
}

// argsFor 发射实参表 (按签名形参定宽; 求值顺序 = 从左到右, §六)。
//
// C15：实参顺序在 C 里**未规定**，故带副作用的实参先按源码序绑临时量
// （见 order.go）；这里只负责给出形参类型。
func (c *Ctx) argsFor(v *parse.Call, sig *types.FuncSig) (string, error) {
	if c.probe {
		// 探测遍（F3 事实收集）：容器当实参传出 = 可能被别名修改。
		for _, a := range v.Args {
			c.markLenMutated(a)
		}
	}
	wants := make([]types.Type, 0, len(v.Args))
	for i := range v.Args {
		if i < len(sig.ParamTypes) {
			wants = append(wants, sig.ParamTypes[i])
		} else {
			wants = append(wants, nil)
		}
	}
	parts, err := c.orderedList(v.Args, wants)
	if err != nil {
		return "", err
	}
	return joinComma(parts), nil
}

// rawArgs 发射实参表 (无签名可用: lambda 变量调用)。
func (c *Ctx) rawArgs(v *parse.Call) (string, error) {
	wants := make([]types.Type, 0, len(v.Args))
	for _, a := range v.Args {
		wants = append(wants, c.ti(a))
	}
	parts, err := c.orderedList(v.Args, wants)
	if err != nil {
		return "", err
	}
	return joinComma(parts), nil
}

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

// variantValue 发射带数据变体的构造值 ({tag, union} 布局, §四)。
func (c *Ctx) variantValue(en *types.Enum, variant, arg string) string {
	ty := c.namedTypeName(types.EnumPkg(en), en.Name)
	return fmt.Sprintf("((%s){ .tag = %s_%s, .u.%s = %s })", ty, ty, variant, variant, arg)
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

// initMethod 取类的 init 构造函数 (§四: 至多一个、无重载、可 -> Err)。
func (c *Ctx) initMethod(cl *types.Class) (*types.FuncSig, bool) {
	for name := range cl.Methods {
		if sig, ok := cl.Method(name); ok && sig.IsInit {
			return sig, true
		}
	}
	return nil, false
}

// conversion 发射显式数值转换 (§二.4: 运行期窄化越界 = trap)。
// 运行时符号表 = aic_num_f2i_<tgt> / aic_num_narrow_<src>_to_<tgt>; 更宽的
// 源一律以 i64/u64 承载 (见 runtime/aic_std.h 的落点声明)。
func (c *Ctx) conversion(target *types.Basic, src types.Type, arg string, line int) string {
	if src == nil {
		return fmt.Sprintf("((%s)(%s))", BasicCName[target.Name], arg)
	}
	sb, isBasic := src.(*types.Basic)
	if !isBasic {
		return fmt.Sprintf("((%s)(%s))", BasicCName[target.Name], arg)
	}
	// 浮点 → 整数: 向零截断, 越界/NaN = trap
	if (sb.Name == "f32" || sb.Name == "f64") && isIntName(target.Name) {
		return fmt.Sprintf("aic_num_f2i_%s((aic_f64)(%s), %s, %d)",
			target.Name, arg, cstr(c.Path), line)
	}
	// 整数 → 整数 窄化: 越界 = trap
	if isIntName(sb.Name) && isIntName(target.Name) && narrower(target.Name, sb.Name) {
		wide := "i64"
		if isUnsignedName(sb.Name) && isUnsignedName(target.Name) {
			wide = "u64"
		} else if isUnsignedName(sb.Name) {
			wide = "u64"
		}
		return fmt.Sprintf("(%s)aic_num_narrow_%s_to_%s((%s)(%s), %s, %d)",
			BasicCName[target.Name], wide, target.Name, BasicCName[wide], arg, cstr(c.Path), line)
	}
	return fmt.Sprintf("((%s)(%s))", BasicCName[target.Name], arg)
}

// isUnsignedName 报告基本类型名是否无符号。
func isUnsignedName(name string) bool {
	switch name {
	case "u8", "u16", "u32", "u64", "usize":
		return true
	}
	return false
}

// isIntName 报告基本类型名是否整型。
func isIntName(name string) bool {
	switch name {
	case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize":
		return true
	}
	return false
}

// narrower 报告目标位宽是否小于源位宽 (窄化需检查)。
func narrower(target, src string) bool {
	width := map[string]int{
		"i8": 8, "u8": 8, "i16": 16, "u16": 16,
		"i32": 32, "u32": 32, "i64": 64, "u64": 64, "usize": 64,
	}
	tw, ok1 := width[target]
	sw, ok2 := width[src]
	if !ok1 || !ok2 {
		return false
	}
	return tw < sw
}
