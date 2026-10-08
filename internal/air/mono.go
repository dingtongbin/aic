package air

import (
	"fmt"
	"sort"
	"strings"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// mair = **单态化后**的 AIR（核心设计 §十：tast → air → mair → eair → cir）。
//
// air 里的泛型体是**模板**（类型形参以 `T<名>` 形态出现在类型文本里，例如字段
// `0:v:TT`）；mair 里模板必须全部展开成具体实例，且**不留任何类型形参**
// （§10.1 V1.2：无 TypeParam、无未绑定实参）。
//
// 实例的**唯一来源**是检查器的事实（红线 11：实例键与符号名同源）：
//   - 调用点：`Call{Sym, TypeArgs}`（推断与显式两条路都由 CallTypeArgs 登记过）；
//   - 类型位：每条指令/签名里的类型文本 `Base[a, b]`（Alloc.Ty / Var.Ty / Let.Ty …）；
//   - `Info.FuncInsts`：检查器登记的实例表（跨包汇总后的权威清单）。
//
// 展开 = **类型文本替换**：air 的类型文本由 tyText 生成，类型形参渲染为 `T<名>`
// （前缀 T + 形参名），按标识符边界替换即可；这样不必解析 air 文本（打印机与解析器
// 无需双向一致，§十：文本 IR 是投影不是往返格式）。
// ---------------------------------------------------------------------------

// Monomorphize 展开模板，返回 mair。info 为 nil 时按"无泛型"处理（模板集合为空）。
func Monomorphize(m *Module, info *types.Info) (*Module, error) {
	out := &Module{Pkg: m.Pkg, Imports: append([]string{}, m.Imports...)}

	// ① 模板判定：名字 → 类型形参名表
	funcTemplates := map[string][]string{} // AIR 符号 → 形参名
	typeTemplates := map[string][]string{} // AIR 类型符号 → 形参名
	if info != nil {
		for name, sig := range info.Funcs {
			if len(sig.TypeParams) > 0 {
				funcTemplates[mangleFunc(m.Pkg, "", name)] = sig.TypeParams
			}
		}
		for cname, cl := range info.Classes {
			if len(cl.TypeParams) == 0 {
				continue
			}
			typeTemplates[mangleType(m.Pkg, cname)] = cl.TypeParams
			for mname := range cl.Methods {
				funcTemplates[mangleFunc(m.Pkg, cname, mname)] = cl.TypeParams
			}
		}
	}

	// ② 收集实例
	funcInsts := map[string]bool{} // "sym[args]" → 需要展开
	typeInsts := map[string]bool{} // "sym[args]"
	needFunc := func(sym string, args []string) {
		if len(args) == 0 {
			return
		}
		if _, isTemplate := funcTemplates[sym]; isTemplate {
			funcInsts[sym+"["+strings.Join(args, ", ")+"]"] = true
		}
	}
	var needTypeText func(text string)
	needTypeText = func(text string) {
		base, args, ok := splitTypeText(text)
		if !ok {
			return
		}
		if _, isTemplate := typeTemplates[base]; isTemplate && len(args) > 0 {
			typeInsts[base+"["+strings.Join(args, ", ")+"]"] = true
		}
		// 嵌套实参里也可能有实例（Box[Box[i32]]）
		for _, a := range args {
			needTypeText(a)
		}
	}
	for _, f := range m.Funcs {
		for _, r := range f.Rets {
			needTypeText(r)
		}
		for _, p := range f.Params {
			needTypeText(p.Ty)
		}
		for _, b := range f.Blocks {
			for _, in := range b.Insts {
				needFunc(inCallSym(in), inCallTypeArgs(in))
				for _, t := range instTypeTexts(in) {
					needTypeText(t)
				}
			}
		}
	}
	for _, td := range m.Types {
		for _, fld := range td.Fields {
			needTypeText(fld.Ty)
		}
		for _, va := range td.Variants {
			needTypeText(va.Payload)
		}
	}
	// 检查器登记的实例表（跨包汇总；方法实例也在其中）
	if info != nil {
		for _, fi := range info.FuncInsts {
			if fi.Fn == nil || len(fi.Args) == 0 {
				continue
			}
			args := make([]string, 0, len(fi.Args))
			for _, a := range fi.Args {
				args = append(args, tyText(a))
			}
			sym := mangleFunc(m.Pkg, fi.Fn.Recv, fi.Fn.Name)
			needFunc(sym, args)
		}
	}

	// ③ 非模板声明原样保留
	for _, td := range m.Types {
		if _, isTpl := typeTemplates[td.Sym]; isTpl {
			continue
		}
		out.Types = append(out.Types, td)
	}
	for _, f := range m.Funcs {
		if _, isTpl := funcTemplates[f.Sym]; isTpl {
			continue
		}
		out.Funcs = append(out.Funcs, f)
	}

	// ④ 展开类型实例（按符号排序：确定性）
	tkeys := monoKeys(typeInsts)
	for _, key := range tkeys {
		base, args := keyBaseArgs(key)
		tpl, ok := findTypeTemplate(m, base)
		if !ok {
			return nil, fmt.Errorf("air: type template %s not found for instance %s", base, key)
		}
		params := typeTemplates[base]
		if len(params) != len(args) {
			return nil, fmt.Errorf("air: type %s takes %d type arguments, got %d", base, len(params), len(args))
		}
		sub := map[string]string{}
		for i, p := range params {
			sub[p] = args[i]
		}
		clone := *tpl
		clone.Sym = key
		clone.Fields = make([]Field, 0, len(tpl.Fields))
		for _, fld := range tpl.Fields {
			fld.Ty = substTypeText(fld.Ty, sub)
			clone.Fields = append(clone.Fields, fld)
		}
		clone.Variants = make([]Variant, 0, len(tpl.Variants))
		for _, va := range tpl.Variants {
			va.Payload = substTypeText(va.Payload, sub)
			clone.Variants = append(clone.Variants, va)
		}
		clone.Slots = append([]Slot{}, tpl.Slots...)
		out.Types = append(out.Types, &clone)
	}

	// ⑤ 展开函数实例（模板体整体代入；实例体是独立副本 ⇒ 后续各层可自由改写）
	fkeys := monoKeys(funcInsts)
	for _, key := range fkeys {
		base, args := keyBaseArgs(key)
		tpl, ok := findFuncTemplate(m, base)
		if !ok {
			return nil, fmt.Errorf("air: function template %s not found for instance %s", base, key)
		}
		params := funcTemplates[base]
		if len(params) != len(args) {
			return nil, fmt.Errorf("air: %s takes %d type arguments, got %d", base, len(params), len(args))
		}
		sub := map[string]string{}
		for i, p := range params {
			sub[p] = args[i]
		}
		clone, err := substFunc(tpl, key, sub)
		if err != nil {
			return nil, err
		}
		out.Funcs = append(out.Funcs, clone)
	}
	return out, nil
}

// VerifyMono 是 mair 的 verifier：**不得残留类型形参**，且不得引用未展开的模板。
func VerifyMono(m *Module, info *types.Info) []string {
	var out []string
	templates := map[string]bool{}
	if info != nil {
		for name, sig := range info.Funcs {
			if len(sig.TypeParams) > 0 {
				templates[mangleFunc(m.Pkg, "", name)] = true
			}
		}
		for cname, cl := range info.Classes {
			if len(cl.TypeParams) == 0 {
				continue
			}
			for mname := range cl.Methods {
				templates[mangleFunc(m.Pkg, cname, mname)] = true
			}
		}
	}
	check := func(where, text string) {
		if hasTypeParamText(text, info, m.Pkg) {
			out = append(out, fmt.Sprintf("mair: %s still mentions a type parameter: %s", where, text))
		}
	}
	for _, f := range m.Funcs {
		if templates[f.Sym] {
			out = append(out, fmt.Sprintf("mair: generic template %s survived monomorphization", f.Sym))
		}
		for i, p := range f.Params {
			check(fmt.Sprintf("func %s param %d", f.Sym, i+1), p.Ty)
		}
		for i, r := range f.Rets {
			check(fmt.Sprintf("func %s result %d", f.Sym, i+1), r)
		}
		for _, b := range f.Blocks {
			for _, in := range b.Insts {
				// 调用实例时 Sym 存的是**基名**、TypeArgs 存实例实参（打印成 `base[args]`）
				// ⇒ 只有"没有 TypeArgs 却是模板名"才是没展开的引用。
				if sym := inCallSym(in); sym != "" && templates[sym] && len(inCallTypeArgs(in)) == 0 {
					out = append(out, fmt.Sprintf("mair: call to unexpanded template %s in %s", sym, f.Sym))
				}
				for _, t := range instTypeTexts(in) {
					check("inst in "+f.Sym, t)
				}
			}
		}
	}
	for _, td := range m.Types {
		for _, fld := range td.Fields {
			check("type "+td.Sym+" field "+fld.Name, fld.Ty)
		}
		for _, va := range td.Variants {
			check("type "+td.Sym+" variant "+va.Name, va.Payload)
		}
	}
	return out
}

// hasTypeParamText 报告一段类型文本里是否出现类型形参（`T<名>`，标识符边界成立）。
func hasTypeParamText(text string, info *types.Info, pkg string) bool {
	if text == "" || info == nil {
		return false
	}
	// 形参名表：本包函数/方法 + 泛型类的形参
	var params []string
	for _, sig := range info.Funcs {
		params = append(params, sig.TypeParams...)
	}
	for _, cl := range info.Classes {
		params = append(params, cl.TypeParams...)
	}
	for _, p := range params {
		pat := "T" + p
		idx := 0
		for {
			i := strings.Index(text[idx:], pat)
			if i < 0 {
				break
			}
			at := idx + i
			end := at + len(pat)
			if end >= len(text) || !isIdentChar(text[end]) {
				return true
			}
			idx = end
		}
	}
	return false
}

func isIdentChar(b byte) bool {
	return b == '_' || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// substTypeText 把类型文本里的 `T<形参名>` 换成实参文本（按标识符边界）。
func substTypeText(text string, sub map[string]string) string {
	if text == "" || len(sub) == 0 {
		return text
	}
	names := make([]string, 0, len(sub))
	for n := range sub {
		names = append(names, n)
	}
	sort.Strings(names) // 长的先换，避免前缀互相吃掉（形参名是单字母时可省，但排序更稳）
	sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
	for _, n := range names {
		pat := "T" + n
		var b strings.Builder
		i := 0
		for {
			k := strings.Index(text[i:], pat)
			if k < 0 {
				b.WriteString(text[i:])
				break
			}
			at := i + k
			end := at + len(pat)
			// 标识符边界：pat 之后不能还是标识符字符（否则 `TTime` 会被 `T`+`Time` 误配）
			b.WriteString(text[i:at])
			if end < len(text) && isIdentChar(text[end]) {
				b.WriteString(pat)
			} else {
				b.WriteString(sub[n])
			}
			i = end
		}
		text = b.String()
	}
	return text
}

// substFunc 复制一个函数模板并按 sub 替换全部类型文本。
func substFunc(tpl *Func, sym string, sub map[string]string) (*Func, error) {
	out := &Func{Sym: sym, Flags: append([]string{}, tpl.Flags...), Loc: tpl.Loc}
	for _, p := range tpl.Params {
		out.Params = append(out.Params, Param{Name: p.Name, Ty: substTypeText(p.Ty, sub)})
	}
	for _, r := range tpl.Rets {
		out.Rets = append(out.Rets, substTypeText(r, sub))
	}
	for _, b := range tpl.Blocks {
		nb := &Block{Label: b.Label, Term: substTerm(b.Term, sub), Loc: b.Loc}
		for _, in := range b.Insts {
			ni, err := substInst(in, sub)
			if err != nil {
				return nil, err
			}
			nb.Insts = append(nb.Insts, ni)
		}
		out.Blocks = append(out.Blocks, nb)
	}
	return out, nil
}

func substTerm(t Term, sub map[string]string) Term {
	switch v := t.(type) {
	case *Ret:
		vals := append([]string{}, v.Vals...)
		return &Ret{Vals: vals, Loc: v.Loc}
	case *Br:
		return &Br{Label: v.Label, Loc: v.Loc}
	case *Cbr:
		return &Cbr{Cond: v.Cond, Then: v.Then, Else: v.Else, Loc: v.Loc}
	case *Switch:
		return &Switch{Val: v.Val, Labels: append([]string{}, v.Labels...), Loc: v.Loc}
	case *CheckFail:
		return &CheckFail{Err: v.Err, Tmp: v.Tmp, Loc: v.Loc}
	}
	return t
}

// substInst 复制一条指令并按 sub 替换其中的类型文本。
func substInst(in Inst, sub map[string]string) (Inst, error) {
	switch v := in.(type) {
	case *Let:
		c := *v
		c.Ty = substTypeText(v.Ty, sub)
		c.Rhs = substRHS(v.Rhs, sub)
		return &c, nil
	case *Var:
		c := *v
		c.Ty = substTypeText(v.Ty, sub)
		return &c, nil
	case *RegionEnter:
		c := *v
		c.TypeID = substTypeText(v.TypeID, sub)
		return &c, nil
	case *DeferInit:
		c := *v
		c.Callee = substTypeText(v.Callee, sub)
		c.TypeArgs = substList(v.TypeArgs, sub)
		c.Vals = append([]string{}, v.Vals...)
		return &c, nil
	}
	return in, nil
}

// substRHS 替换值形 RHS 里的类型文本（Nil/SizeOf 带类型；其余只带值名）。
func substRHS(r RHS, sub map[string]string) RHS {
	switch v := r.(type) {
	case *Nil:
		return &Nil{Ty: substTypeText(v.Ty, sub)}
	case *SizeOf:
		return &SizeOf{Ty: substTypeText(v.Ty, sub)}
	case *Call:
		c := *v
		c.Sym = substTypeText(v.Sym, sub)
		c.TypeArgs = substList(v.TypeArgs, sub)
		c.Args = append([]string{}, v.Args...)
		return &c
	case *CallInd:
		c := *v
		c.Args = append([]string{}, v.Args...)
		return &c
	case *Box:
		c := *v
		c.Iface = substTypeText(v.Iface, sub)
		return &c
	case *Witness:
		c := *v
		c.Ty = substTypeText(v.Ty, sub)
		c.Iface = substTypeText(v.Iface, sub)
		return &c
	}
	return r
}

func substList(in []string, sub map[string]string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, substTypeText(s, sub))
	}
	return out
}

// inCallSym 报告一条指令是否是一次直接调用，返回其符号（否则空串）。
func inCallSym(in Inst) string {
	switch v := in.(type) {
	case *Let:
		if c, ok := v.Rhs.(*Call); ok {
			return c.Sym
		}
	}
	return ""
}

func inCallTypeArgs(in Inst) []string {
	switch v := in.(type) {
	case *Let:
		if c, ok := v.Rhs.(*Call); ok {
			return c.TypeArgs
		}
	}
	return nil
}

// instTypeTexts 取一条指令里出现的全部类型文本（收集实例与 verifier 共用）。
func instTypeTexts(in Inst) []string {
	var out []string
	switch v := in.(type) {
	case *Let:
		out = append(out, v.Ty)
		out = append(out, rhsTypeTexts(v.Rhs)...)
	case *Var:
		out = append(out, v.Ty)
	case *RegionEnter:
		out = append(out, v.TypeID)
	}
	return out
}

func rhsTypeTexts(r RHS) []string {
	switch v := r.(type) {
	case *Nil:
		return []string{v.Ty}
	case *SizeOf:
		return []string{v.Ty}
	case *Call:
		return v.TypeArgs
	case *CallInd:
		return nil
	case *Box:
		return []string{v.Iface}
	case *Witness:
		return []string{v.Ty, v.Iface}
	}
	return nil
}

// splitTypeText 拆 `base[arg, arg]`（只认**顶层**方括号；`ok_Box*[i32]` → base=ok_Box*，
// args=[i32]）。无方括号或括号不配对时 ok=false。
func splitTypeText(s string) (string, []string, bool) {
	open := strings.IndexByte(s, '[')
	if open < 0 || !strings.HasSuffix(s, "]") {
		return "", nil, false
	}
	base := s[:open]
	inner := s[open+1 : len(s)-1]
	var args []string
	depth := 0
	last := 0
	for i := 0; i < len(inner); i++ {
		switch inner[i] {
		case '[':
			depth++
		case ']':
			depth--
		case ',':
			if depth == 0 {
				args = append(args, strings.TrimSpace(inner[last:i]))
				last = i + 1
			}
		}
	}
	args = append(args, strings.TrimSpace(inner[last:]))
	return base, args, true
}

// keyBaseArgs 把 "sym[a, b]" 拆回 (sym, [a b])。
func keyBaseArgs(key string) (string, []string) {
	open := strings.IndexByte(key, '[')
	if open < 0 {
		return key, nil
	}
	base := key[:open]
	inner := strings.TrimSuffix(key[open+1:], "]")
	parts := strings.Split(inner, ", ")
	return base, parts
}

func monoKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func findTypeTemplate(m *Module, sym string) (*TypeDecl, bool) {
	for _, td := range m.Types {
		if td.Sym == sym {
			return td, true
		}
	}
	return nil, false
}

func findFuncTemplate(m *Module, sym string) (*Func, bool) {
	for _, f := range m.Funcs {
		if f.Sym == sym {
			return f, true
		}
	}
	return nil, false
}
