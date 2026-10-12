package air

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// `.air` 文本投影（文法）。**编译器不解析它**——只有 verifier 的
// 反例语料以手写 `.air` 为输入；故打印机只需单向保真：
//   - 一行一条指令，行首可选 `loc <file>:<line>:<col>`（IR 载体）；
//   - 缩进仅助读，verifier 以显式 <label> 为准；
//   - **同一输入两次 dump 必须逐字节一致**（H8）⇒ 禁止 map 遍历序进文本。
// ---------------------------------------------------------------------------

// Print 把一个 Module 渲染成 `.air` 文本（确定性）。
func Print(m *Module) string {
	var b strings.Builder
	fmt.Fprintf(&b, "air v1 module %s\n", m.Pkg)
	for _, im := range m.Imports {
		fmt.Fprintf(&b, "import %s\n", im)
	}
	for _, t := range m.Types {
		printTypeDecl(&b, t)
	}
	for _, f := range m.Funcs {
		printFunc(&b, f)
	}
	return b.String()
}

// PrintAll 渲染整程序（多包，按给定顺序 = 拓扑序）。
func PrintAll(mods []*Module) string {
	var b strings.Builder
	for i, m := range mods {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(Print(m))
	}
	return b.String()
}

func printTypeDecl(b *strings.Builder, t *TypeDecl) {
	switch t.Kind {
	case "struct":
		// Kind 恒为 "struct"（类型声明的统称），值/引用由 Struct 二分：
		// 源码写 struct 的是值类型，写 class 的是引用类型。
		kind := "class"
		if t.Struct {
			kind = "struct"
		}
		fmt.Fprintf(b, "type %s = %s\n", t.Sym, kind)
		for _, f := range t.Fields {
			fmt.Fprintf(b, "    %d:%s:%s\n", f.Idx, f.Name, f.Ty)
		}
	case "enum":
		fmt.Fprintf(b, "type %s = enum\n", t.Sym)
		for _, v := range t.Variants {
			if v.Payload == "" {
				fmt.Fprintf(b, "    %s\n", v.Name)
				continue
			}
			fmt.Fprintf(b, "    %s(%s)\n", v.Name, v.Payload)
		}
	case "iface":
		fmt.Fprintf(b, "type %s = iface\n", t.Sym)
		for _, s := range t.Slots {
			fmt.Fprintf(b, "    slot %d:%s:%s\n", s.Idx, s.Name, s.Sig)
		}
	default:
		fmt.Fprintf(b, "type %s = %s\n", t.Sym, t.Kind)
	}
}

func printFunc(b *strings.Builder, f *Func) {
	params := make([]string, 0, len(f.Params))
	for _, p := range f.Params {
		ref := ""
		if p.Ref {
			ref = "ref " // 按引用形参：.air 文本里必须可辨（H8 快照钉住形态）
		}
		params = append(params, ref+p.Name+":"+p.Ty)
	}
	head := fmt.Sprintf("func %s(%s)", f.Sym, strings.Join(params, ", "))
	switch len(f.Rets) {
	case 0:
	case 1:
		head += " -> " + f.Rets[0]
	default:
		head += " -> (" + strings.Join(f.Rets, ", ") + ")"
	}
	for _, fl := range f.Flags {
		head += " " + fl
	}
	fmt.Fprintf(b, "%s {\n", head)
	for _, blk := range f.Blocks {
		printBlock(b, blk)
	}
	fmt.Fprintf(b, "}\n")
}

func printBlock(b *strings.Builder, blk *Block) {
	fmt.Fprintf(b, "  %s:\n", blk.Label)
	for _, in := range blk.Insts {
		fmt.Fprintf(b, "    %s%s\n", locPrefix(inLoc(in)), printInst(in))
	}
	if blk.Term != nil {
		fmt.Fprintf(b, "    %s%s\n", locPrefix(termLoc(blk.Term)), printTerm(blk.Term))
	}
}

func locPrefix(l Loc) string {
	if l.File == "" {
		return ""
	}
	return fmt.Sprintf("loc %s:%d:%d ", l.File, l.Line, l.Col)
}

// InstLoc 取一条指令的位置（无 Loc 字段的指令返回零值）。
func InstLoc(in Inst) Loc {
	switch v := in.(type) {
	case *Let:
		return v.Loc
	case *Var:
		return v.Loc
	case *Store:
		return v.Loc
	case *RegionEnter:
		return v.Loc
	case *RegionExit:
		return v.Loc
	case *DeferReg:
		return v.Loc
	case *DeferInit:
		return v.Loc
	case *DeferRun:
		return v.Loc
	case *TrapIfErr:
		return v.Loc
	case *Trap:
		return v.Loc
	case *SelWait:
		return v.Loc
	case *Spawn:
		return v.Loc
	case *SelectInst:
		return v.Loc
	case *ForInInst:
		return v.Loc
	case *PropagateInst:
		return v.Loc
	case *ScopeInst:
		return v.Loc
	}
	return Loc{}
}

// PrintInst 是单条指令的文本投影（cir 打印与调试共用；文法唯一）。
func PrintInst(in Inst) string { return printInst(in) }

// PrintTerm 是终结符的文本投影（cir 打印与调试共用）。
func PrintTerm(t Term) string { return printTerm(t) }

func printInst(in Inst) string {
	switch v := in.(type) {
	case *Let:
		return fmt.Sprintf("let %s : %s = %s", v.Tmp, v.Ty, printRHS(v.Rhs))
	case *Var:
		if v.Init == "" {
			return fmt.Sprintf("var %s : %s", v.Name, v.Ty)
		}
		return fmt.Sprintf("var %s : %s = %s", v.Name, v.Ty, v.Init)
	case *Store:
		g := ""
		if v.Limit != "" {
			g = ".guard(" + v.Limit + ")"
		}
		return fmt.Sprintf("store%s %s, %s", g, printPlace(v.Place), v.Val)
	case *RegionEnter:
		if v.Scope {
			return "scope.enter " + v.TypeID
		}
		return "region.enter " + v.TypeID
	case *RegionExit:
		if v.Scope {
			return "scope.exit"
		}
		return "region.exit"
	case *DeferReg:
		if v.OnErr {
			return "defer.reg.onerr"
		}
		return "defer.reg"
	case *DeferInit:
		callee := v.Callee
		if len(v.TypeArgs) > 0 {
			callee += "[" + strings.Join(v.TypeArgs, ", ") + "]"
		}
		vals := make([]string, 0, len(v.Vals))
		for i, val := range v.Vals {
			g := ""
			if i < len(v.Limits) && v.Limits[i] != "" {
				g = ".guard(" + v.Limits[i] + ")"
			}
			vals = append(vals, g+val)
		}
		return fmt.Sprintf("defer.init %d, %s(%s)", v.ID, callee, strings.Join(vals, ", "))
	case *DeferRun:
		if v.Err == "" {
			return "defer.run"
		}
		return "defer.run " + v.Err
	case *Spawn:
		callee := v.Callee
		if len(v.TypeArgs) > 0 {
			callee += "[" + strings.Join(v.TypeArgs, ", ") + "]"
		}
		return fmt.Sprintf("spawn %s(%s)", callee, strings.Join(v.Vals, ", "))
	case *TrapIfErr:
		return fmt.Sprintf("trap.if.err %s, line %d", v.Err, v.Line)
	case *Trap:
		return fmt.Sprintf("trap %s, line %d", v.Code, v.Line)
	case *SelWait:
		arms := make([]string, 0, len(v.Chans))
		for i, ch := range v.Chans {
			kind := "recv"
			if v.Sends[i] {
				kind = "send"
			}
			arms = append(arms, kind+" "+ch)
		}
		out := fmt.Sprintf("let %s : i32 = select.wait [%s]", v.Tmp, strings.Join(arms, ", "))
		if v.Deadline != "" {
			out += " deadline " + v.Deadline
		}
		return out
	case *ForInInst:
		names := make([]string, 0, len(v.Names))
		for i, n := range v.Names {
			ty := ""
			if i < len(v.NameTys) {
				ty = " : " + v.NameTys[i]
			}
			names = append(names, n+ty)
		}
		return fmt.Sprintf("forin %s in %s : %s -> %s",
			strings.Join(names, ", "), v.Subject, v.SubjTy, v.Body)
	case *SelectInst:
		return fmt.Sprintf("select arms=%d -> %s", len(v.Arms), v.After)
	case *PropagateInst:
		return fmt.Sprintf("propagate %s mode=%s -> %s (mt: %s)", v.Operand, v.Mode, v.After, strings.Join(v.Mt, ", "))
	case *ScopeInst:
		if v.IsTimeo {
			return fmt.Sprintf("scope.timeout %s -> %s -> %s", v.Timeout, v.Body, v.After)
		}
		return fmt.Sprintf("scope -> %s -> %s", v.Body, v.After)
	}
	return fmt.Sprintf("<unknown inst %T>", in)
}

func printRHS(r RHS) string {
	switch v := r.(type) {
	case *Call:
		ta := ""
		if len(v.TypeArgs) > 0 {
			ta = "[" + strings.Join(v.TypeArgs, ", ") + "]"
		}
		return fmt.Sprintf("call %s%s(%s)", v.Sym, ta, strings.Join(v.Args, ", "))
	case *CallInd:
		return fmt.Sprintf("call.ind %s.%d(%s)", v.Recv, v.Slot, strings.Join(v.Args, ", "))
	case *Closure:
		if len(v.Env) == 0 {
			return "closure " + v.Sym
		}
		return fmt.Sprintf("closure %s [%s]", v.Sym, strings.Join(v.Env, ", "))
	case *CallClosure:
		return fmt.Sprintf("call.closure %s(%s)", v.Val, strings.Join(v.Args, ", "))
	case *Alloc:
		return fmt.Sprintf("alloc %s(%s)", v.Kind, v.Ty)
	case *Binop:
		return fmt.Sprintf("binop %s %s, %s", v.Op, v.A, v.B)
	case *Cmp:
		return fmt.Sprintf("cmp %s %s, %s", v.Op, v.A, v.B)
	case *Unop:
		return fmt.Sprintf("unop %s %s", v.Op, v.A)
	case *Conv:
		return fmt.Sprintf("conv %s %s", v.Kind, v.A)
	case *SizeOf:
		return "sizeof " + v.Ty
	case *FieldRHS:
		return fmt.Sprintf("field %s, %d", printPlace(v.Place), v.Idx)
	case *ElemRHS:
		return fmt.Sprintf("elem %s, %s", printPlace(v.Place), v.Idx)
	case *LenRHS:
		return fmt.Sprintf("len %s", printPlace(v.Place))
	case *StrViewRHS:
		return fmt.Sprintf("str.view %s, %s, %s", v.Base, v.Lo, v.Hi)
	case *AddrRHS:
		return "addr " + v.Val
	case *MultiExtract:
		return fmt.Sprintf("multi.extract %s, %d", v.Val, v.Idx)
	case *EnumTag:
		return fmt.Sprintf("enum.tag %s", v.Val)
	case *EnumPayload:
		return fmt.Sprintf("enum.payload %s, %d", v.Val, v.Idx)
	case *Box:
		return fmt.Sprintf("box %s, %s", v.Val, v.Iface)
	case *Witness:
		return fmt.Sprintf("witness %s, %s", v.Ty, v.Iface)
	case *Const:
		return "const " + v.Lit
	case *TmpRef:
		return "tmp " + v.Name
	case *VarRef:
		return "var " + v.Name
	case *Nil:
		return "nil : " + v.Ty
	case *MatchExprRHS:
		out := "match " + v.Subject + " : " + v.SubjTy + " slot " + v.Slot + " : " + v.SlotTy + " -> " + v.After + " { "
		for i := range v.Arms {
			if i > 0 {
				out += "; "
			}
			out += printMatchArm(v.Arms[i])
		}
		return out + " }"
	case *CheckRHS:
		return fmt.Sprintf("check %s -> %s -> %s (mt: %s)", v.Operand, v.Payload, v.After, strings.Join(v.Mt, ", "))
	case *CatchRHS:
		if v.Bind != "" {
			return fmt.Sprintf("catch %s as %s -> %s -> %s (mt: %s)", v.Operand, v.Bind, v.Body, v.After, strings.Join(v.Mt, ", "))
		}
		return fmt.Sprintf("catch %s -> %s -> %s (mt: %s)", v.Operand, v.Body, v.After, strings.Join(v.Mt, ", "))
	case *InterpRHS:
		return fmt.Sprintf("interp parts=%d vals=%d", len(v.Parts), len(v.Values))
	case *AssertRHS:
		if v.Payload != "" {
			return fmt.Sprintf("assert %s -> %s (mt: %s)", v.Operand, v.Payload, strings.Join(v.Mt, ", "))
		}
		return fmt.Sprintf("assert %s (mt: %s)", v.Operand, strings.Join(v.Mt, ", "))
	}
	return fmt.Sprintf("<unknown rhs %T>", r)
}

// printMatchArm 是 match 臂的文本投影（IR-1）：`Pat1 | Pat2 => body`。
func printMatchArm(a MatchArm) string {
	pats := make([]string, 0, len(a.Patterns))
	for _, p := range a.Patterns {
		s := p.Variant
		if p.Binding != "" && p.Binding != "_" {
			s += "(" + p.Binding
			if p.BindTy != "" {
				s += " : " + p.BindTy
			}
			s += ")"
		}
		pats = append(pats, s)
	}
	return strings.Join(pats, " | ") + " => " + a.Body
}

func printTerm(t Term) string {
	switch v := t.(type) {
	case *Br:
		return "br " + v.Label
	case *Cbr:
		return fmt.Sprintf("cbr %s, %s, %s", v.Cond, v.Then, v.Else)
	case *Switch:
		return fmt.Sprintf("switch %s (%s)", v.Val, strings.Join(v.Labels, " "))
	case *CheckFail:
		return fmt.Sprintf("checkfail %s, %s", v.Err, v.Tmp)
	case *Ret:
		return "ret " + strings.Join(v.Vals, ", ")
	case *MatchTerm:
		out := "match " + v.Subject + " : " + v.SubjTy + " { "
		for i := range v.Arms {
			a := v.Arms[i]
			out += printMatchArm(a)
			if i+1 < len(v.Arms) {
				out += "; "
			}
		}
		return out + " }"
	}
	return fmt.Sprintf("<unknown term %T>", t)
}

func printPlace(p Place) string {
	switch v := p.(type) {
	case *VarPlace:
		return v.Name
	case *FieldPlace:
		return fmt.Sprintf("field %s, %d", printPlace(v.Base), v.Idx)
	case *ElemPlace:
		return fmt.Sprintf("elem %s, %s", printPlace(v.Base), v.Idx)
	case *StrViewPlace:
		return fmt.Sprintf("strview %s, %s, %s", printPlace(v.Base), v.Lo, v.Hi)
	}
	return fmt.Sprintf("<unknown place %T>", p)
}

// --- 位置访问（verifier 的覆盖检查与 printer 共用） ---------------------

// inLoc 取一条指令的位置（verifier 的诊断锚点）。
//
// 这是 **InstLoc 的转发**，不是第二份 switch —— 各自维护两份时，加新节点
// 时改一份漏一份的表现是"新节点的指令报 without loc"（本次 IR-1 原语落地时
// 实测：verify 报，根因就是这里漏登记）。单一来源见 InstLoc。
func inLoc(in Inst) Loc { return InstLoc(in) }

func termLoc(t Term) Loc {
	switch v := t.(type) {
	case *Br:
		return v.Loc
	case *Cbr:
		return v.Loc
	case *Switch:
		return v.Loc
	case *CheckFail:
		return v.Loc
	case *Ret:
		return v.Loc
	case *MatchTerm:
		// IR-1 原语终结符（§13.1）：也带 Loc，否则 verify 的会误报
		// "terminator without loc" —— 实测正是这样。
		return v.Loc
	}
	return Loc{}
}
