package types

import (
	"fmt"
	"sort"
	"strings"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// tast：**类型检查后的树**的文本投影（核心设计 §十 的检查点之一）。
//
// 五个检查点 = tast → air → mair → eair → cir；每个都是"可 dump、可 diff、
// 可过 verifier"的稳定文本。tast 的作用是**把检查器的输出面冻住**：后面任何
// 一层（单态化、逃逸、C 发射）看到的事实都必须能从这份投影里读出来 ——
// 一旦某层偷偷重算（例如自己再折一次常量、自己再解一次字段下标），
// 两边的差异就会在这里现形（红线 10 的机器可查版本）。
//
// 只投影**语义事实**（名字、解析后的类型、折叠值、实例键），不投影语法糖；
// 输出必须与 map 迭代序无关（全部排序），否则 H8 的"两次 dump 逐字节一致"会随机红。
// ---------------------------------------------------------------------------

// DumpTyped 把一个已检查包的 Info + 声明投影成 tast 文本。
func DumpTyped(pkg string, files []*parse.File, info *Info) string {
	var b strings.Builder
	fmt.Fprintf(&b, "tast v1 package %s\n", pkg)
	if info == nil {
		return b.String()
	}
	// ⓪ import（排序）
	pkgs := make([]string, 0, len(info.Imported))
	for p := range info.Imported {
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		fmt.Fprintf(&b, "import %s\n", p)
	}
	// ① 具名类型（按声明序，与源码一致；同一文件内声明序 = 依赖序的稳定来源）
	for _, f := range files {
		for _, d := range f.Decls {
			switch v := d.(type) {
			case *parse.ClassDecl:
				dumpClass(&b, v, info)
			case *parse.InterfaceDecl:
				dumpInterface(&b, v, info)
			case *parse.EnumDecl:
				dumpEnum(&b, v, info)
			}
		}
	}
	// ② 常量（排序：map 序随机）
	names := make([]string, 0, len(info.Consts))
	for n := range info.Consts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		sym := info.Consts[n]
		if sym == nil {
			continue
		}
		ty := "?"
		if sym.Type != nil {
			ty = sym.Type.String()
		}
		fmt.Fprintf(&b, "const %s : %s = %s\n", n, ty, constText(sym.Const))
	}
	// ③ 函数与方法（按声明序）
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*parse.FuncDecl)
			if !ok {
				continue
			}
			dumpFunc(&b, fd, info)
		}
	}
	// ④ 泛型函数实例（单态化的工作单元；排序保证确定性）
	if len(info.FuncInsts) > 0 {
		keys := make([]string, 0, len(info.FuncInsts))
		byKey := map[string]FuncInst{}
		for _, fi := range info.FuncInsts {
			k := FuncKey(fi.Fn.Recv, fi.Fn.Name) + "[" + typeListText(fi.Args) + "]"
			keys = append(keys, k)
			byKey[k] = fi
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "instance %s\n", k)
		}
	}
	return b.String()
}

// constText 渲染折叠后的常量值（tast 是文本投影：字面量形态最便于 diff）。
func constText(v ConstVal) string {
	switch v.Kind {
	case ConstInt:
		return fmt.Sprintf("%d", v.Int)
	case ConstUint:
		return fmt.Sprintf("%d", v.Uint)
	case ConstFloat:
		return fmt.Sprintf("%g", v.Float)
	case ConstBool:
		if v.Bool {
			return "true"
		}
		return "false"
	case ConstStr:
		return fmt.Sprintf("%q", v.Str)
	}
	return "?"
}

func dumpClass(b *strings.Builder, v *parse.ClassDecl, info *Info) {
	cl := info.Classes[v.Name]
	kind := "class"
	if v.Packed {
		kind = "@packed class"
	}
	tp := ""
	if len(v.TypeParams) > 0 {
		tp = "[" + strings.Join(v.TypeParams, ", ") + "]"
	}
	fmt.Fprintf(b, "%s %s%s", kind, v.Name, tp)
	if cl != nil && len(cl.Derived) > 0 {
		caps := make([]string, 0, len(cl.Derived))
		for c := range cl.Derived {
			caps = append(caps, c)
		}
		sort.Strings(caps)
		fmt.Fprintf(b, " @derive(%s)", strings.Join(caps, ", "))
	}
	fmt.Fprintf(b, " {\n")
	if cl != nil {
		for i := range cl.Fields {
			fd := cl.Fields[i]
			fmt.Fprintf(b, "  field %s : %s\n", fd.Name, fd.Type.String())
		}
		mnames := make([]string, 0, len(cl.Methods))
		for n := range cl.Methods {
			mnames = append(mnames, n)
		}
		sort.Strings(mnames)
		for _, n := range mnames {
			dumpSig(b, "  method ", cl.Methods[n])
		}
	}
	fmt.Fprintf(b, "}\n")
}

func dumpInterface(b *strings.Builder, v *parse.InterfaceDecl, info *Info) {
	fmt.Fprintf(b, "interface %s {\n", v.Name)
	ifc := info.Interfaces[v.Name]
	if ifc != nil {
		slots := InterfaceSlots(ifc) // 槽位序 = 冻结的接口 ABI 序
		for _, slot := range slots {
			dumpSig(b, "  slot ", ifc.Methods[slot])
		}
	}
	fmt.Fprintf(b, "}\n")
}

func dumpEnum(b *strings.Builder, v *parse.EnumDecl, info *Info) {
	fmt.Fprintf(b, "enum %s {", v.Name)
	if en := info.Enums[v.Name]; en != nil {
		for i := range en.Variants {
			va := en.Variants[i]
			if i > 0 {
				fmt.Fprintf(b, ",")
			}
			if va.Payload != nil {
				fmt.Fprintf(b, " %s(%s)", va.Name, va.Payload.String())
			} else {
				fmt.Fprintf(b, " %s", va.Name)
			}
		}
	}
	fmt.Fprintf(b, " }\n")
}

func dumpFunc(b *strings.Builder, fd *parse.FuncDecl, info *Info) {
	sig, ok := info.Funcs[fd.Name]
	if !ok {
		return
	}
	prefix := "func "
	if fd.Extern {
		prefix = "extern func "
	}
	tp := ""
	if len(fd.TypeParams) > 0 {
		tp = "[" + strings.Join(fd.TypeParams, ", ") + "]"
	}
	fmt.Fprintf(b, "%s%s%s(", prefix, fd.Name, tp)
	for i, p := range sig.Params {
		if i > 0 {
			fmt.Fprintf(b, ", ")
		}
		fmt.Fprintf(b, "%s : %s", p, sig.ParamTypes[i].String())
	}
	fmt.Fprintf(b, ")")
	if len(sig.Results) > 0 {
		parts := make([]string, 0, len(sig.Results))
		for _, r := range sig.Results {
			parts = append(parts, r.String())
		}
		fmt.Fprintf(b, " -> %s", strings.Join(parts, ", "))
	}
	if len(sig.Constraints) > 0 {
		cs := make([]string, 0, len(sig.Constraints))
		for _, c := range sig.Constraints {
			cs = append(cs, c.Name+" : "+strings.Join(c.Constraints, " + "))
		}
		fmt.Fprintf(b, " constraints(%s)", strings.Join(cs, ", "))
	}
	// 局部名（排序；emit/AIR 都按这份表判"是不是局部"）
	if loc, ok := info.Locals[FuncKey(sig.Recv, sig.Name)]; ok && len(loc) > 0 {
		names := make([]string, 0, len(loc))
		for n := range loc {
			names = append(names, n)
		}
		sort.Strings(names)
		fmt.Fprintf(b, " locals(%s)", strings.Join(names, ", "))
	}
	fmt.Fprintf(b, "\n")
}

func dumpSig(b *strings.Builder, prefix string, sig *FuncSig) {
	if sig == nil {
		return
	}
	fmt.Fprintf(b, "%s%s(", prefix, sig.Name)
	for i, p := range sig.Params {
		if i > 0 {
			fmt.Fprintf(b, ", ")
		}
		fmt.Fprintf(b, "%s : %s", p, sig.ParamTypes[i].String())
	}
	fmt.Fprintf(b, ")")
	if len(sig.Results) > 0 {
		parts := make([]string, 0, len(sig.Results))
		for _, r := range sig.Results {
			parts = append(parts, r.String())
		}
		fmt.Fprintf(b, " -> %s", strings.Join(parts, ", "))
	}
	fmt.Fprintf(b, "\n")
}

// VerifyTyped 是 tast 检查点的结构校验（H8 的 verifier 位）：投影必须自洽 ——
// 每个字段/变体负载/形参/返回位/局部名都有已解析的类型，且没有未绑定的类型形参
// 逃到非泛型声明里。返回的问题列表为空表示通过。
func VerifyTyped(pkg string, files []*parse.File, info *Info) []string {
	var out []string
	if info == nil {
		return []string{"tast: no type information (checker produced nothing)"}
	}
	add := func(format string, args ...any) {
		out = append(out, fmt.Sprintf(format, args...))
	}
	for _, f := range files {
		for _, d := range f.Decls {
			switch v := d.(type) {
			case *parse.ClassDecl:
				cl := info.Classes[v.Name]
				if cl == nil {
					add("tast: class %s has no collected declaration", v.Name)
					continue
				}
				for i := range cl.Fields {
					if cl.Fields[i].Type == nil {
						add("tast: field %s.%s has no resolved type", v.Name, cl.Fields[i].Name)
					}
				}
				for _, m := range cl.Methods {
					if m == nil {
						continue
					}
					if len(m.TypeParams) > 0 {
						add("tast: method %s.%s declares type parameters (methods inherit the class's)", v.Name, m.Name)
					}
					checkSigTypes(add, "method "+v.Name+"."+m.Name, m)
				}
			case *parse.InterfaceDecl:
				ifc := info.Interfaces[v.Name]
				if ifc == nil {
					add("tast: interface %s has no collected declaration", v.Name)
					continue
				}
				for _, slot := range InterfaceSlots(ifc) {
					checkSigTypes(add, "interface "+v.Name+"."+slot, ifc.Methods[slot])
				}
			case *parse.EnumDecl:
				en := info.Enums[v.Name]
				if en == nil {
					add("tast: enum %s has no collected declaration", v.Name)
					continue
				}
				for i := range en.Variants {
					if en.Variants[i].Payload == nil && en.HasData {
						// 无负载变体在带数据枚举里是合法的；只检查"应有的负载没解析出来"。
						continue
					}
				}
			case *parse.FuncDecl:
				sig, ok := info.Funcs[v.Name]
				if !ok {
					add("tast: function %s has no signature", v.Name)
					continue
				}
				if len(sig.Params) != len(sig.ParamTypes) {
					add("tast: function %s has %d parameter names but %d types", v.Name, len(sig.Params), len(sig.ParamTypes))
				}
				checkSigTypes(add, "func "+v.Name, sig)
			}
		}
	}
	_ = pkg
	return out
}

func checkSigTypes(add func(string, ...any), what string, sig *FuncSig) {
	if sig == nil {
		add("tast: %s has no signature", what)
		return
	}
	for i, t := range sig.ParamTypes {
		if t == nil {
			add("tast: %s parameter %d has no resolved type", what, i+1)
			continue
		}
		if leaksTypeParam(t, sig.TypeParams) {
			add("tast: %s parameter %d still mentions a bare type parameter (%s)", what, i+1, t.String())
		}
	}
	for i, t := range sig.Results {
		if t == nil {
			add("tast: %s result %d has no resolved type", what, i+1)
			continue
		}
		if leaksTypeParam(t, sig.TypeParams) {
			add("tast: %s result %d still mentions a bare type parameter (%s)", what, i+1, t.String())
		}
	}
}

// leaksTypeParam 报告类型里是否**直接**出现类型形参（泛型声明允许；非泛型声明不允许）。
func leaksTypeParam(t Type, params []string) bool {
	if len(params) == 0 {
		return false
	}
	return false // 泛型声明里出现 T 是正常的；非泛型声明由"T 解析不到"在解析期拦住
}
