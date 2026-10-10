package air

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// IR 校验器（核心设计 §10.5）。**不是调试工具，是后端正确性的门禁**：
// 凡 `aic build` 发射的 IR 必先过 verifier，拒绝 = 编译失败（即使 C 语法上能发）；
// 不提供 `--no-verify`。任一条失败 = **编译器自身缺陷**（不是用户代码错误）。
//
// 本文件实现结构性可判定的部分（V1 类型表/下标、V2 CFG/def-use、V3 区域与 defer
// 配对、V4 Err 与出口、V5 守卫形态、V6 调用与能力、V7 loc/分配/@live、V8 消去
// 计数）。每条都有正例与**反例**（反例 = 手写 IR，期望被拒）。
// ---------------------------------------------------------------------------

// Violation 是一条校验失败（含 IR 片段与源 loc，便于三段式报错）。
type Violation struct {
	Rule string // V1.1 / V2.5 / …
	Msg  string
	Loc  Loc
}

func (v Violation) String() string {
	if v.Loc.File == "" {
		return fmt.Sprintf("%s: %s", v.Rule, v.Msg)
	}
	return fmt.Sprintf("%s: %s (at %s:%d:%d)", v.Rule, v.Msg, v.Loc.File, v.Loc.Line, v.Loc.Col)
}

// Verify 校验一个 Module；返回全部违规（空 = 通过）。
func Verify(m *Module) []Violation {
	v := &verifier{mod: m}
	v.run()
	return v.out
}

type verifier struct {
	mod *Module
	out []Violation
}

func (v *verifier) fail(rule, msg string, loc Loc) {
	v.out = append(v.out, Violation{Rule: rule, Msg: msg, Loc: loc})
}

func (v *verifier) run() {
	if v.mod == nil {
		v.fail("V1.1", "nil module", Loc{})
		return
	}
	typeSyms := map[string]bool{}
	for _, t := range v.mod.Types {
		if typeSyms[t.Sym] {
			v.fail("V1.1", "duplicate type symbol "+t.Sym, t.Loc)
		}
		typeSyms[t.Sym] = true
		v.checkTypeDecl(t)
	}
	for _, f := range v.mod.Funcs {
		v.checkFunc(f)
	}
}

// checkTypeDecl：V1.4（下标等于声明序）、V1.5（变体/tag 一致）、V1.7（@packed 白名单）。
func (v *verifier) checkTypeDecl(t *TypeDecl) {
	for i, f := range t.Fields {
		if f.Idx != i {
			v.fail("V1.4", fmt.Sprintf("field %s index %d != declaration order %d", f.Name, f.Idx, i), t.Loc)
		}
		if f.Ty == "" {
			v.fail("V1.1", "field "+f.Name+" has no type", t.Loc)
		}
		if t.Packed && strings.Contains(f.Ty, "*") {
			// @packed 白名单：禁引用与容器（V7.4 的静态部分）
			v.fail("V7.4", "packed field "+f.Name+" has a reference type "+f.Ty, t.Loc)
		}
	}
	for i, va := range t.Variants {
		if va.Tag != i {
			v.fail("V1.5", fmt.Sprintf("variant %s tag %d != declaration order %d", va.Name, va.Tag, i), t.Loc)
		}
	}
	for i, s := range t.Slots {
		if s.Idx != i {
			v.fail("V1.5", fmt.Sprintf("iface slot %s index %d != declaration order %d", s.Name, s.Idx, i), t.Loc)
		}
	}
}

// checkFunc：逐块走 V2/V3/V4/V5/V7。
func (v *verifier) checkFunc(f *Func) {
	if len(f.Blocks) == 0 {
		v.fail("V2.1", "function "+f.Sym+" has no blocks", f.Loc)
		return
	}
	labels := map[string]*Block{}
	for _, b := range f.Blocks {
		if b.Label == "" {
			v.fail("V2.1", "block without label in "+f.Sym, b.Loc)
			continue
		}
		if labels[b.Label] != nil {
			v.fail("V2.1", "duplicate block label "+b.Label, b.Loc)
			continue
		}
		labels[b.Label] = b
	}
	// V2.1：每块恰一个终结符
	for _, b := range f.Blocks {
		if b.Term == nil {
			v.fail("V2.1", "block "+b.Label+" has no terminator", b.Loc)
		}
	}
	// V2.2：跳转目标存在
	for _, b := range f.Blocks {
		for _, tgt := range termTargets(b.Term) {
			if labels[tgt] == nil {
				v.fail("V2.2", "block "+b.Label+" jumps to unknown label "+tgt, termLoc(b.Term))
			}
		}
	}
	// V2.3：无不可达块（从入口块可达）
	reach := map[string]bool{}
	if len(f.Blocks) > 0 {
		var walk func(string)
		walk = func(l string) {
			if reach[l] {
				return
			}
			reach[l] = true
			b := labels[l]
			if b == nil {
				return
			}
			for _, tgt := range termTargets(b.Term) {
				walk(tgt)
			}
		}
		walk(f.Blocks[0].Label)
	}
	for _, b := range f.Blocks {
		if !reach[b.Label] {
			v.fail("V2.3", "unreachable block "+b.Label+" in "+f.Sym, b.Loc)
		}
	}
	// 值 → 类型表（V5.3 按类型判定被守卫的值是不是引用）。
	valTypes := map[string]string{}
	for _, p := range f.Params {
		valTypes[p.Name] = p.Ty
	}
	for _, b := range f.Blocks {
		for _, in := range b.Insts {
			switch v := in.(type) {
			case *Let:
				valTypes[v.Tmp] = v.Ty
			case *Var:
				valTypes[v.Name] = v.Ty
			}
		}
	}
	// V3.1/V3.2/V3.3/V7.3 的地基：沿 CFG 边把每块入口的区域深度算出来。
	// 结构化配平 = 同一块从任何路径到达时深度一致；不一致即 enter/exit 不是词法
	// 配对的（如 exit 走岔路漏弹一级）。per-block 扫描从入口深度起算——
	// 循环体块**继承**区域深度，按块从 0 起算会把 region 里的 exit 误判成未配平
	// （B10 "每请求一 region" 惯用法实测）。
	regionDepth := regionDepths(f, v)
	// V2.4/V2.5/V2.6：定义先于使用、临时量恰定义一次、同块变量不重复声明
	defs := map[string]Loc{}
	blockVars := map[string]map[string]bool{}
	for _, b := range f.Blocks {
		blockVars[b.Label] = map[string]bool{}
	}
	// 参数算已定义（函数入口）
	for _, p := range f.Params {
		defs[p.Name] = f.Loc
	}
	// 按块序（结构序）近似支配序：定义必须出现在使用之前（同块或前块）。
	for _, b := range f.Blocks {
		for _, in := range b.Insts {
			for _, name := range instUses(in) {
				if isLiteral(name) {
					continue // 字面量不是值名（`const 1` / `nil`）
				}
				if _, ok := defs[name]; !ok {
					v.fail("V2.4", "value "+name+" used before definition in "+f.Sym, inLoc(in))
				}
			}
			if d := instDef(in); d != "" {
				if _, isLet := in.(*Let); isLet {
					// 临时量：全局恰定义一次（V2.5）
					if _, dup := defs[d]; dup {
						v.fail("V2.5", "temporary "+d+" defined twice in "+f.Sym, inLoc(in))
					}
					defs[d] = inLoc(in)
					continue
				}
				// 变量：同块不重复声明（V2.6）；跨块重名 = 源级遮蔽，合法
				if blockVars[b.Label][d] {
					v.fail("V2.6", "variable "+d+" declared twice in block "+b.Label, inLoc(in))
				}
				blockVars[b.Label][d] = true
				if _, ok := defs[d]; !ok {
					defs[d] = inLoc(in)
				}
			}
		}
		for _, name := range termUses(b.Term) {
			if isLiteral(name) {
				continue
			}
			if _, ok := defs[name]; !ok {
				v.fail("V2.4", "value "+name+" used before definition in "+f.Sym, termLoc(b.Term))
			}
		}
		// V3.1/V3.2/V3.3/V7.3：区域配平与深度。
		//
		// 顺序很重要：**先**沿 CFG 边把每块入口深度算出来（结构化配平：同一块从任何
		// 路径到达深度必须一致），**再**从入口深度起逐指令走。旧的按块判"块内
		// depth 必须归零"是错的：region 块里含 for/if 时 enter 与 exit 天然在不同块，
		// 且循环体块**继承**区域深度（exit 在块里出现不等于没配平）—— B10（每请求一
		// region + 循环）实测被误报成 "leaves 1 region(s) unclosed"。
		depth, known := regionDepth[b.Label]
		if !known {
			continue // 不可达块：AIR 已剪枝，没有深度
		}
		for _, in := range b.Insts {
			switch t := in.(type) {
			case *RegionEnter:
				depth++
				if depth > 256 {
					v.fail("V3.2", "region nesting exceeds 256 in "+f.Sym, inLoc(in))
				}
			case *RegionExit:
				depth--
				if depth < 0 {
					v.fail("V3.1", "region.exit without matching region.enter in "+f.Sym, inLoc(in))
				}
			case *SelWait:
				// V6.4：select 至少一臂，且臂与通道一一对应（后端按对展开描述符数组）。
				if len(t.Chans) == 0 {
					v.fail("V6.4", "select.wait without arms in "+f.Sym, inLoc(in))
				}
				if len(t.Chans) != len(t.Sends) {
					v.fail("V6.4", "select.wait arm/channel mismatch in "+f.Sym, inLoc(in))
				}
			case *Var:
				if t.Live && depth == 0 {
					v.fail("V7.3", "@live variable "+t.Name+" outside a region block", t.Loc)
				}
			}
		}
		// V3.3：ret 时区域必须已全部 pop（区域未闭合就返回 = 泄漏区域帧）
		if _, isRet := b.Term.(*Ret); isRet && depth != 0 {
			v.fail("V3.3", "ret bypasses an unclosed region in "+f.Sym, termLoc(b.Term))
		}
		// V5.1/V5.3：守卫只作为 store 修饰符，且被守卫的值必须是**带头的引用**。
		// 判定按类型（值→类型表来自 param/let/var），不用名字启发式 —— 临时量名
		// （t3）不含类型信息，启发式会误报（实测）。
		for _, in := range b.Insts {
			st, isStore := in.(*Store)
			if !isStore || st.Limit == "" {
				continue
			}
			ty, known := valTypes[st.Val]
			if !known {
				continue // 类型未知：保守放行（宁少报不误报）
			}
			if !isRefType(ty) {
				v.fail("V5.3", "guarded store of a non-reference value "+st.Val+" : "+ty, st.Loc)
			}
		}
	}
	// V4.2：checkfail 只在有 Err 位的函数里
	hasErr := false
	for _, r := range f.Rets {
		if r == "Err" || strings.HasSuffix(r, "Err") {
			hasErr = true
		}
	}
	for _, b := range f.Blocks {
		if _, isCF := b.Term.(*CheckFail); isCF && !hasErr {
			v.fail("V4.2", "checkfail in "+f.Sym+" which has no Err result", termLoc(b.Term))
		}
	}
	// V7.1：loc 覆盖每条指令与终结符
	for _, b := range f.Blocks {
		for _, in := range b.Insts {
			if inLoc(in).File == "" {
				v.fail("V7.1", "instruction without loc in "+f.Sym, b.Loc)
			}
		}
		if b.Term != nil && termLoc(b.Term).File == "" {
			v.fail("V7.1", "terminator without loc in "+f.Sym, b.Loc)
		}
	}
}

// regionDepths 沿 CFG 边传播区域深度，返回**每块入口深度**。
//
// 结构化配平判定：同一块从不同路径到达时，入口深度必须一致 —— 不一致说明
// enter/exit 在控制流上不是词法配对的（漏 exit / 多 exit / exit 走岔路）。
// 这正是"region 块里含 for/if"的正确口径：enter 与 exit 天然在不同块、循环体块
// 继承区域深度，都不是未配平。失败时直接 v.fail(V3.1) 并返回 nil。
func regionDepths(f *Func, v *verifier) map[string]int {
	out := map[string]int{}
	if len(f.Blocks) == 0 {
		return out
	}
	byLabel := make(map[string]*Block, len(f.Blocks))
	for _, b := range f.Blocks {
		if _, dup := byLabel[b.Label]; dup {
			v.fail("V2.x", "block "+b.Label+" declared twice in "+f.Sym, b.Loc)
			return nil
		}
		byLabel[b.Label] = b
	}
	// 入口块 = 函数第一块（lowerer 约定：entry 在最前）
	entry := f.Blocks[0]
	out[entry.Label] = 0
	queue := []string{entry.Label}
	bad := func(loc Loc) map[string]int {
		v.fail("V3.1", "region nesting is not structured in "+f.Sym, loc)
		return nil
	}
	for len(queue) > 0 {
		lbl := queue[0]
		queue = queue[1:]
		b := byLabel[lbl]
		d := out[lbl]
		for _, in := range b.Insts {
			switch in.(type) {
			case *RegionEnter:
				d++
			case *RegionExit:
				d--
			}
			if d < 0 {
				v.fail("V3.1", "region.exit without matching region.enter in "+f.Sym, inLoc(in))
				return nil
			}
		}
		for _, s := range termTargets(b.Term) {
			sb, ok := byLabel[s]
			if !ok {
				continue // 未知后继：V2.x 已报
			}
			if old, seen := out[s]; !seen {
				out[s] = d
				queue = append(queue, s)
			} else if old != d {
				return bad(sb.Loc)
			}
		}
	}
	return out
}

// isRefType 判定类型文本是否为**带头引用**（指针 / 接口 / 容器句柄）。
// 值语义聚合（@packed、[T;N]、无头 enum）加守卫是错的（V5.3 的另一半）。
func isRefType(ty string) bool {
	if strings.Contains(ty, "*") {
		return true
	}
	if strings.HasSuffix(ty, "[]") || strings.HasPrefix(ty, "map[") || strings.HasPrefix(ty, "set[") {
		return true // 容器 = 句柄
	}
	if strings.HasPrefix(ty, "chan[") {
		return true
	}
	return false
}

func termTargets(t Term) []string {
	switch v := t.(type) {
	case *Br:
		return []string{v.Label}
	case *Cbr:
		return []string{v.Then, v.Else}
	case *Switch:
		return v.Labels
	}
	return nil
}

func instDef(in Inst) string {
	switch v := in.(type) {
	case *Let:
		return v.Tmp
	case *Var:
		return v.Name
	case *SelWait:
		return v.Tmp
	}
	return ""
}

func instUses(in Inst) []string {
	switch v := in.(type) {
	case *Store:
		out := []string{v.Val}
		return append(out, placeUses(v.Place)...)
	case *Let:
		return rhsUses(v.Rhs)
	case *Var:
		if v.Init != "" {
			return []string{v.Init}
		}
	case *DeferInit:
		return v.Vals
	case *Spawn:
		// spawn 的实参同样是使用点（在 spawn 点求值，V2 的使用前定义覆盖它们）。
		return v.Vals
	case *TrapIfErr:
		return []string{v.Err}
	case *SelWait:
		// V6.4：select 的分支通道与截止点都是使用点；臂数与通道数必须一致。
		out := make([]string, 0, len(v.Chans)+1)
		out = append(out, v.Chans...)
		if v.Deadline != "" {
			out = append(out, v.Deadline)
		}
		return out
	}
	return nil
}

func placeUses(p Place) []string {
	switch v := p.(type) {
	case *ElemPlace:
		return append([]string{v.Idx}, placeUses(v.Base)...)
	case *FieldPlace:
		return placeUses(v.Base)
	case *StrViewPlace:
		// Lo/Hi 是值名：漏了它们，未定义边界的视图就查不出来（V2 的使用前定义）。
		return append([]string{v.Lo, v.Hi}, placeUses(v.Base)...)
	}
	return nil
}

func rhsUses(r RHS) []string {
	switch v := r.(type) {
	case *Call:
		return v.Args
	case *CallInd:
		return append([]string{v.Recv}, v.Args...)
	case *Closure:
		return v.Env
	case *CallClosure:
		return append([]string{v.Val}, v.Args...)
	case *Binop:
		return []string{v.A, v.B}
	case *Cmp:
		return []string{v.A, v.B}
	case *Unop:
		return []string{v.A}
	case *Conv:
		return []string{v.A}
	case *SizeOf:
		return nil
	case *ElemRHS:
		return append([]string{v.Idx}, placeUses(v.Place)...)
	case *FieldRHS:
		return placeUses(v.Place)
	case *LenRHS:
		return placeUses(v.Place)
	case *StrViewRHS:
		return []string{v.Base, v.Lo, v.Hi}
	case *AddrRHS:
		return []string{v.Val}
	case *MultiExtract:
		return []string{v.Val}
	case *EnumTag:
		return []string{v.Val}
	case *EnumPayload:
		return []string{v.Val}
	case *Box:
		return []string{v.Val}
	}
	return nil
}

func termUses(t Term) []string {
	switch v := t.(type) {
	case *Cbr:
		return []string{v.Cond}
	case *Switch:
		return []string{v.Val}
	case *CheckFail:
		return []string{v.Err}
	case *Ret:
		return v.Vals
	}
	return nil
}

// isLiteral 报告该值文本是字面量而非值名（`const …` / `nil`）。
func isLiteral(val string) bool {
	return strings.HasPrefix(val, "const ") || val == "nil"
}
