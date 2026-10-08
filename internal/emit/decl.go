package emit

import (
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 文件骨架 + 具名类型声明 (核心设计 §四 布局确定性: class 字段按声明序自然
// 对齐, 禁重排; @packed = 严格 C 布局)。
// ---------------------------------------------------------------------------

const headerGuard = "AIC_EMIT_TU_H"

// prologue 发射文件头与 include。
func (c *Ctx) prologue() error {
	c.line("/* 由 aic emit 生成 (核心设计 §十: AST → C 文本, 纯函数)。请勿手改。 */")
	c.line("/* 源: %s  包: %s */", c.Path, c.pkg())
	c.line("#include \"aic_l0.h\"")
	c.line("#include \"aic_l1.h\"")
	c.line("#include \"aic_std.h\"")
	// L2（调度层）按需 include：不用 spawn/scope 的程序根本不拉它（红线 16）。
	if c.needed["l2"] {
		c.line("#include \"aic_l2.h\"")
	}
	c.line("#include <stdlib.h>")
	c.line("")
	return nil
}

// epilogue 收尾 (当前无额外内容; 保留以便后续按需链接清单输出)。
func (c *Ctx) epilogue() {
	c.line("/* end of translation unit */")
}

// pkg 是本次发射的包名 (目录即包; 单文件编译 = 声明该文件的目录名)。
func (c *Ctx) pkg() string {
	if c.Info != nil && c.Info.Pkg != "" {
		return c.Info.Pkg
	}
	return "main"
}

// forwardTypes 发射本文件具名类型的前向 typedef (class 可自引用, §二.6)。
// 整程序发射时全部包的前向声明都排在完整定义之前。
// 泛型类不在这里发（按实例发，见 emitInstances）。
func (c *Ctx) forwardTypes() {
	for _, d := range c.File.Decls {
		switch v := d.(type) {
		case *parse.ClassDecl:
			if len(v.TypeParams) > 0 {
				continue
			}
			c.line("typedef struct %s %s;", c.typedefName(c.pkg(), v.Name), c.typedefName(c.pkg(), v.Name))
		case *parse.EnumDecl:
			if hasDataVariants(v) {
				c.line("typedef enum %s_tag %s_tag;", c.typedefName(c.pkg(), v.Name), c.typedefName(c.pkg(), v.Name))
				c.line("typedef struct %s %s;", c.typedefName(c.pkg(), v.Name), c.typedefName(c.pkg(), v.Name))
			} else {
				c.line("typedef enum %s %s;", c.typedefName(c.pkg(), v.Name), c.typedefName(c.pkg(), v.Name))
			}
		}
	}
}

// emitTypeDefs 发射本文件具名类型的完整定义。
//
// **顺序 = 依赖序（拓扑）**：值类型可以按值嵌入别的值类型（enum 负载是 @packed 类、
// @packed 字段是另一个 @packed、`[T;N]` 的元素是值类型）。C 里按值成员要求被嵌类型
// **已完整定义**，按源码序发会在"枚举在前、其负载类在后"时得到
// `field 'Rect' has incomplete type`。引用类型只出现指针，不构成依赖。
func (c *Ctx) emitTypeDefs() error {
	type valDef struct {
		name string
		emit func()
		deps []string
	}
	var defs []valDef
	seen := map[string]bool{}
	for _, d := range c.File.Decls {
		switch v := d.(type) {
		case *parse.ClassDecl:
			if len(v.TypeParams) > 0 {
				continue // 泛型类按实例发（emitInstances）
			}
			name := c.typedefName(c.pkg(), v.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
			cl := v
			deps := []string{}
			if cl.Packed {
				for i := range cl.Fields {
					deps = append(deps, c.valueTypeDeps(cl.Fields[i].Type)...)
				}
			}
			defs = append(defs, valDef{name: name, emit: func() {
				c.srcLine(cl.Pos)
				c.emitClass(cl)
			}, deps: deps})
		case *parse.EnumDecl:
			name := c.typedefName(c.pkg(), v.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
			en := v
			var deps []string
			for _, va := range en.Variants {
				deps = append(deps, c.valueTypeDeps(va.Payload)...)
			}
			defs = append(defs, valDef{name: name, emit: func() {
				c.srcLine(en.Pos)
				c.emitEnum(en)
			}, deps: deps})
		}
	}
	// 稳定拓扑：反复取"依赖已发"的定义（依赖环是不可能的 —— 值类型递归被语言拒绝）。
	emitted := map[string]bool{}
	for len(defs) > 0 {
		progress := false
		rest := defs[:0:0]
		for _, df := range defs {
			ready := true
			for _, dep := range df.deps {
				if dep != "" && dep != df.name && !emitted[dep] {
					if _, known := seen[dep]; known {
						ready = false
						break
					}
				}
			}
			if !ready {
				rest = append(rest, df)
				continue
			}
			df.emit()
			emitted[df.name] = true
			progress = true
		}
		if !progress {
			// 环（不该出现）：按剩余顺序发，保证确定性与可见的 C 报错。
			for _, df := range rest {
				df.emit()
				emitted[df.name] = true
			}
			break
		}
		defs = rest
	}
	c.line("")
	return nil
}

// valueTypeDeps 报告一个类型作为**按值成员**时的依赖（C 类型名）：
// @packed 类、enum、`[T;N]`（元素递归）。引用类型/标量 = 无依赖。
func (c *Ctx) valueTypeDeps(te parse.TypeExpr) []string {
	if te == nil {
		return nil
	}
	t := c.typeOfTypeExpr(te)
	if t == nil {
		return nil
	}
	switch v := t.(type) {
	case *types.Class:
		if v.Packed {
			return []string{c.namedTypeName(v.Pkg, v.Name)}
		}
	case *types.Enum:
		return []string{c.namedTypeName(types.EnumPkg(v), v.Name)}
	case *types.ArrayT:
		return c.valueTypeDepsType(v.Elem)
	}
	return nil
}

// valueTypeDepsType 同 valueTypeDeps，但输入是已解析类型（数组元素递归用）。
func (c *Ctx) valueTypeDepsType(t types.Type) []string {
	switch v := t.(type) {
	case *types.Class:
		if v.Packed {
			return []string{c.namedTypeName(v.Pkg, v.Name)}
		}
	case *types.Enum:
		return []string{c.namedTypeName(types.EnumPkg(v), v.Name)}
	case *types.ArrayT:
		return c.valueTypeDepsType(v.Elem)
	}
	return nil
}

// emitClass 发射 class 的 C struct。布局 = 对象头 + 字段按声明序 (§四)。
// @packed 无对象头 (值类型, 无区域托管, §五)。
func (c *Ctx) emitClass(v *parse.ClassDecl) {
	name := c.typedefName(c.pkg(), v.Name)
	c.line("struct %s {", name)
	if !v.Packed {
		c.line("    aic_hdr hdr;")
	}
	for _, f := range v.Fields {
		ft := c.fieldType(f)
		for _, tgt := range f.Targets {
			c.line("    %s %s;", ft, tgt.Name)
		}
	}
	if len(v.Fields) == 0 {
		c.line("    aic_u8 _empty;")
	}
	c.line("};")
	c.line("")
}

// fieldType 解析字段声明类型 (来自检查器已解析的类型表; 未解析时回退字面文本)。
func (c *Ctx) fieldType(f *parse.FieldDecl) string {
	if f.Type != nil {
		if t := c.typeOfTypeExpr(f.Type); t != nil {
			return c.cTypeName(t)
		}
	}
	c.warn("unresolved field type (%s): emitting int", f.Pos.String())
	return "int"
}

// typeOfTypeExpr 把 parse 类型表达式映射回语义类型。
// 检查器已把字段类型存进 Class.Fields, 这里优先查它 (单一事实来源);
// 查不到时用声明文本兜底 (有限的基本类型名)。
func (c *Ctx) typeOfTypeExpr(te parse.TypeExpr) types.Type {
	if te == nil {
		return nil
	}
	if bt, ok := te.(*parse.BasicType); ok {
		if t, found := basicByName(bt.Name); found {
			return t
		}
		return nil
	}
	if nt, ok := te.(*parse.NamedType); ok {
		var base types.Type
		switch {
		case nt.Pkg == "":
			if t, found := basicByName(nt.Name); found {
				base = t
			} else if c.Info != nil {
				if cl, ok := c.Info.Classes[nt.Name]; ok {
					base = cl
				} else if en, ok := c.Info.Enums[nt.Name]; ok {
					base = en
				} else if ifc, ok := c.Info.Interfaces[nt.Name]; ok {
					base = ifc
				}
			}
		case nt.Pkg == "sync" && nt.Name == "Mutex":
			// std 具名类型：sync.Mutex（值类型，零值 = 未加锁，§七）
			base = types.TMutex
		case nt.Pkg == "option" && nt.Name == "Option":
			// std 泛型枚举：option.Option / option.Option[i32]
			base = types.OptionBaseEnum()
		default:
			// 跨包具名类型 util.Reader / util.Box[i32]（Pkg 非空且是用户包）
			base = c.depNamedType(nt.Pkg, nt.Name)
		}
		if base == nil {
			return nil
		}
		if len(nt.Args) == 0 {
			return base
		}
		inst := &types.Instance{Base: base}
		for _, a := range nt.Args {
			at := c.typeOfTypeExpr(a)
			if at == nil {
				return nil
			}
			inst.Args = append(inst.Args, at)
		}
		return inst
	}
	if ft, ok := te.(*parse.FuncType); ok {
		// 函数类型 (P1, P2) -> R：lambda 变量、函数型形参都要用
		out := &types.FuncT{}
		for _, p := range ft.Params {
			pt := c.typeOfTypeExpr(p)
			if pt == nil {
				return nil
			}
			out.Params = append(out.Params, pt)
		}
		if ft.Result != nil {
			out.Result = c.typeOfTypeExpr(ft.Result)
		}
		return out
	}
	if st, ok := te.(*parse.SliceType); ok {
		if e := c.typeOfTypeExpr(st.Elem); e != nil {
			return &types.Slice{Elem: e}
		}
		return nil
	}
	if at, ok := te.(*parse.ArrayType); ok {
		if e := c.typeOfTypeExpr(at.Elem); e != nil {
			return &types.ArrayT{Elem: e, N: c.constN(at.Size)}
		}
		return nil
	}
	if mt, ok := te.(*parse.MapType); ok {
		k, v := c.typeOfTypeExpr(mt.Key), c.typeOfTypeExpr(mt.Value)
		if k != nil && v != nil {
			return &types.MapT{Key: k, Value: v}
		}
		return nil
	}
	if st, ok := te.(*parse.SetType); ok {
		if e := c.typeOfTypeExpr(st.Elem); e != nil {
			return &types.SetT{Elem: e}
		}
		return nil
	}
	return nil
}

// constN 取 [T;N] 的 N (整数字面量或 const 名)。
func (c *Ctx) constN(e parse.Expr) int64 {
	// 首选检查器折出来的长度（唯一事实来源）：字面量 / const 名 / sizeOf 等内省原语
	// 都在那里折过一遍。emit 自己再折一遍只认字面量与 const 名 ⇒ `[i32; sizeOf(i32)]`
	// 会发成 `[0]`（C 层报 excess elements）。
	if c.Info != nil && e != nil {
		if n, ok := c.Info.ArrayLens[e]; ok {
			return n
		}
	}
	switch v := e.(type) {
	case *parse.IntLit:
		var n int64
		for i := 0; i < len(v.Text); i++ {
			ch := v.Text[i]
			if ch < '0' || ch > '9' {
				return n
			}
			n = n*10 + int64(ch-'0')
		}
		return n
	case *parse.Ident:
		if c.Info != nil {
			if sym, ok := c.Info.Consts[v.Name]; ok {
				if sym.Const.Kind == types.ConstInt {
					return sym.Const.Int
				}
				if sym.Const.Kind == types.ConstUint {
					return int64(sym.Const.Uint)
				}
			}
		}
	}
	return 0
}

// basicByName 与 types 的基本类型表同名同源 (emit 侧只需要名字→语义类型的查表)。
func basicByName(name string) (types.Type, bool) {
	switch name {
	case "i8":
		return types.TI8, true
	case "i16":
		return types.TI16, true
	case "i32":
		return types.TI32, true
	case "i64":
		return types.TI64, true
	case "u8":
		return types.TU8, true
	case "u16":
		return types.TU16, true
	case "u32":
		return types.TU32, true
	case "u64":
		return types.TU64, true
	case "usize":
		return types.TUsize, true
	case "f32":
		return types.TF32, true
	case "f64":
		return types.TF64, true
	case "bool":
		return types.TBool, true
	case "str":
		return types.TStr, true
	case "Err":
		return types.TErr, true
	}
	return nil, false
}

// emitEnum 发射 enum: 无数据 = C enum (§四); 带数据 = {tag, union}。
func (c *Ctx) emitEnum(v *parse.EnumDecl) {
	name := c.typedefName(c.pkg(), v.Name)
	if !hasDataVariants(v) {
		c.line("enum %s {", name)
		for i, va := range v.Variants {
			sep := ","
			if i == len(v.Variants)-1 {
				sep = ""
			}
			c.line("    %s_%s = %d%s", name, va.Name, i, sep)
		}
		c.line("};")
		c.line("")
		return
	}
	c.line("enum %s_tag {", name)
	for i, va := range v.Variants {
		sep := ","
		if i == len(v.Variants)-1 {
			sep = ""
		}
		c.line("    %s_%s = %d%s", name, va.Name, i, sep)
	}
	c.line("};")
	c.line("struct %s {", name)
	c.line("    %s_tag tag;", name)
	c.line("    union {")
	for _, va := range v.Variants {
		if va.Payload == nil {
			continue
		}
		pt := c.typeOfTypeExpr(va.Payload)
		if pt == nil {
			c.warn("the payload type of variant %s of enum %s is unresolved", v.Name, va.Name)
			continue
		}
		c.line("        %s %s;", c.cTypeName(pt), va.Name)
	}
	c.line("    } u;")
	c.line("};")
	c.line("")
}

// hasDataVariants 报告 enum 是否有带数据变体 (§四: 带数据 = {tag, union} 布局)。
func hasDataVariants(v *parse.EnumDecl) bool {
	for _, va := range v.Variants {
		if va.Payload != nil {
			return true
		}
	}
	return false
}
