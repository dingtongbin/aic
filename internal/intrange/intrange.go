// Package intrange 是 AIR 上的整数区间证明（核心设计 §九「区间分析」；§10.4 第 12 条②
// 「证明安全且要向量化收益 → 发射 unchecked 访问器」的前提）。
//
// 只做**可解释、可复核**的保守证明：证明不出来 = unknown（宁保守插检查，禁错误消除）。
// 事实来源全部是 IR 里明写的东西：
//   - `const N` → [N, N]
//   - `binop add/sub/mul` → 区间算术（溢出即 unknown）
//   - `cbr (cmp lt a, b)` 的两条出边 → then: a ≤ b−1；else: a ≥ b
//   - 循环头块（`forhead`）的条件事实在体内每轮成立（回边重新建立）
//   - `[T;N]` 定长数组的长度是字面量 N（V1 已保证下标等于声明序）
//
// 本包**不改 IR**：它只产事实（`Facts`）供调用方（election/emit）消费，并可由 verifier
// 复核 —— 「先证明后使用」与 escape 的 prove/elim 分离同构。
package intrange

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"aic/internal/air"
)

// Iv 是一个整数区间；!Known = 未知（保守）。
type Iv struct {
	Lo, Hi int64
	Known  bool
}

func unknown() Iv        { return Iv{} }
func point(v int64) Iv   { return Iv{Lo: v, Hi: v, Known: true} }
func (a Iv) add(b Iv) Iv { return bin(a, b, '+') }
func (a Iv) sub(b Iv) Iv { return bin(a, b, '-') }
func (a Iv) mul(b Iv) Iv { return bin(a, b, '*') }

func bin(a, b Iv, op byte) Iv {
	if !a.Known || !b.Known {
		return unknown()
	}
	switch op {
	case '+':
		lo, ok1 := addOk(a.Lo, b.Lo)
		hi, ok2 := addOk(a.Hi, b.Hi)
		if !ok1 || !ok2 {
			return unknown()
		}
		return Iv{Lo: lo, Hi: hi, Known: true}
	case '-':
		lo, ok1 := subOk(a.Lo, b.Hi)
		hi, ok2 := subOk(a.Hi, b.Lo)
		if !ok1 || !ok2 {
			return unknown()
		}
		return Iv{Lo: lo, Hi: hi, Known: true}
	case '*':
		lo, ok1 := mulOk(a.Lo, b.Lo)
		hi, ok2 := mulOk(a.Hi, b.Hi)
		if !ok1 || !ok2 {
			return unknown()
		}
		return Iv{Lo: lo, Hi: hi, Known: true}
	}
	return unknown()
}

func addOk(a, b int64) (int64, bool) {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s > 0) {
		return 0, false
	}
	return s, true
}
func subOk(a, b int64) (int64, bool) { return addOk(a, -b) }
func mulOk(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	return p, true
}

// Access 是一条可判定的访问事实（elem 读/写）。
type Access struct {
	Fn      string
	Block   string
	Place   string // 容器 place 的文本
	Index   string
	IdxIv   Iv
	LenIv   Iv
	InBound bool // 证明成立：0 ≤ lo 且 hi < len
	Reason  string
}

// Facts 是一次证明的产出。
type Facts struct {
	Accesses []Access
	Proved   int
	Unknown  int
}

// Prove 在模块上跑区间证明（不改 IR）。
func Prove(mod *air.Module) *Facts {
	f := &Facts{}
	for _, fn := range mod.Funcs {
		proveFunc(fn, f)
	}
	sort.Slice(f.Accesses, func(i, j int) bool {
		if f.Accesses[i].Fn != f.Accesses[j].Fn {
			return f.Accesses[i].Fn < f.Accesses[j].Fn
		}
		return f.Accesses[i].Block < f.Accesses[j].Block
	})
	return f
}

// proveFunc：识别归纳变量 → 把循环头条件事实与初值合成体内区间 → 记录访问事实。
//
// 健全性：只对**已识别的归纳变量**应用循环条件事实（初值常量 + 回边 `i = i + c`，
// c > 0）。其他变量一律 unknown —— 宁保守插检查，禁错误消除（§10.3 E3 的教条）。
func proveFunc(fn *air.Func, out *Facts) {
	induct := inductionVars(fn)
	entry := entryFacts(fn)

	iv := map[string]Iv{}
	for _, p := range fn.Params {
		iv[p.Name] = unknown()
	}
	for _, b := range fn.Blocks {
		local := map[string]Iv{}
		for k, v := range iv {
			local[k] = v
		}
		// 循环条件事实：只对归纳变量生效（上界 K−1 + 初值下界）
		factApplied := map[string]bool{}
		for _, ft := range entry[b.Label] {
			ind, isInd := induct[ft.name]
			if !isInd || !ft.hasHi || !ind.initKnown {
				continue
			}
			if ind.initLo > ft.hi {
				continue // 空区间（不可能路径）
			}
			local[ft.name] = Iv{Lo: ind.initLo, Hi: ft.hi, Known: true}
			factApplied[ft.name] = true
		}
		for _, in := range b.Insts {
			switch v := in.(type) {
			case *air.Var:
				iv[v.Name] = valIv(v.Init, local, fn)
				local[v.Name] = iv[v.Name]
			case *air.Let:
				iv[v.Tmp] = rhsIv(v.Rhs, local, fn)
				local[v.Tmp] = iv[v.Tmp]
			case *air.Store:
				recordAccess(fn, b, v.Place, v.Val, local, factApplied, out)
				if vp, isVar := v.Place.(*air.VarPlace); isVar {
					iv[vp.Name] = valIv(v.Val, local, fn)
					local[vp.Name] = iv[vp.Name]
				}
			}
		}
	}
}

// induction 是一个已识别的归纳变量。
type induction struct {
	step      int64
	initLo    int64
	initKnown bool
}

// inductionVars：识别「初值已知常量 + 回边 i = i + c（c > 0）」的变量。
func inductionVars(fn *air.Func) map[string]induction {
	out := map[string]induction{}
	for _, b := range fn.Blocks {
		for i, in := range b.Insts {
			st, ok := in.(*air.Store)
			if !ok {
				continue
			}
			vp, isVar := st.Place.(*air.VarPlace)
			if !isVar {
				continue
			}
			step, ok := addStep(b, i, vp.Name)
			if !ok || step <= 0 {
				continue
			}
			initLo, known := initOf(fn, vp.Name)
			out[vp.Name] = induction{step: step, initLo: initLo, initKnown: known}
		}
	}
	return out
}

// addStep：该 store 的值是否为 `x + c`（c 为正常量）——是则返回 c。
func addStep(b *air.Block, at int, name string) (int64, bool) {
	st := b.Insts[at].(*air.Store)
	// 值可能直接是 binop 形态的临时量：找它的定义
	for _, in := range b.Insts[:at] {
		let, ok := in.(*air.Let)
		if !ok || let.Tmp != st.Val {
			continue
		}
		bin, isBin := let.Rhs.(*air.Binop)
		if !isBin || bin.Op != "+" || bin.A != name {
			return 0, false
		}
		c := constOf(bin.B)
		if c <= 0 || c >= 1<<40 {
			return 0, false
		}
		return c, true
	}
	return 0, false
}

// initOf：归纳变量的初值（函数里第一处定义，通常是 preheader 的 `var i = lo`）。
func initOf(fn *air.Func, name string) (int64, bool) {
	for _, b := range fn.Blocks {
		for _, in := range b.Insts {
			switch v := in.(type) {
			case *air.Var:
				if v.Name != name {
					continue
				}
				if strings.HasPrefix(v.Init, "const ") {
					return constOf(v.Init), true
				}
				return 0, false
			case *air.Store:
				if vp, ok := v.Place.(*air.VarPlace); ok && vp.Name == name {
					if strings.HasPrefix(v.Val, "const ") {
						return constOf(v.Val), true
					}
					return 0, false
				}
			}
		}
	}
	return 0, false
}

// entryFacts：前驱终结符是 cbr(cmp …) 时，两条出边各带一条边界事实。
func entryFacts(fn *air.Func) map[string][]fact {
	entry := map[string][]fact{}
	for _, b := range fn.Blocks {
		cb, ok := b.Term.(*air.Cbr)
		if !ok {
			continue
		}
		cmp := cmpOf(b, cb.Cond)
		if cmp == nil {
			continue
		}
		k := constOf(cmp.B)
		switch cmp.Op {
		case "lt":
			entry[cb.Then] = append(entry[cb.Then], fact{name: cmp.A, hi: k - 1, hasHi: true})
			entry[cb.Else] = append(entry[cb.Else], fact{name: cmp.A, lo: k, hasLo: true})
		case "le":
			entry[cb.Then] = append(entry[cb.Then], fact{name: cmp.A, hi: k, hasHi: true})
			entry[cb.Else] = append(entry[cb.Else], fact{name: cmp.A, lo: k + 1, hasLo: true})
		case "gt":
			entry[cb.Then] = append(entry[cb.Then], fact{name: cmp.A, lo: k + 1, hasLo: true})
			entry[cb.Else] = append(entry[cb.Else], fact{name: cmp.A, hi: k, hasHi: true})
		case "ge":
			entry[cb.Then] = append(entry[cb.Then], fact{name: cmp.A, lo: k, hasLo: true})
			entry[cb.Else] = append(entry[cb.Else], fact{name: cmp.A, hi: k - 1, hasHi: true})
		}
	}
	return entry
}

// recordAccess：判定一次 elem 访问是否可证在界内。
func recordAccess(fn *air.Func, b *air.Block, p air.Place, val string, iv map[string]Iv,
	factApplied map[string]bool, out *Facts) {
	ep, ok := p.(*air.ElemPlace)
	if !ok {
		return
	}
	idxIv := valIv(ep.Idx, iv, fn)
	lenIv, place := lenIvOf(ep.Base, fn)
	a := Access{
		Fn: fn.Sym, Block: b.Label, Place: place, Index: ep.Idx,
		IdxIv: idxIv, LenIv: lenIv,
	}
	switch {
	case !factApplied[ep.Idx]:
		// 索引区间不是由**归纳事实**给出的 → 一律不证明（健全性闸门）
		a.Reason = "index bound not established by an induction fact"
	case !idxIv.Known:
		a.Reason = "index range unknown"
	case !lenIv.Known:
		a.Reason = "container length unknown"
	case idxIv.Lo < 0:
		a.Reason = "index may be negative"
	case idxIv.Hi < lenIv.Lo:
		a.InBound = true
		a.Reason = fmt.Sprintf("index in [%d,%d] < length %d", idxIv.Lo, idxIv.Hi, lenIv.Lo)
	default:
		a.Reason = fmt.Sprintf("index may reach %d >= length %d", idxIv.Hi, lenIv.Lo)
	}
	if a.InBound {
		out.Proved++
	} else {
		out.Unknown++
	}
	out.Accesses = append(out.Accesses, a)
}

// lenIvOf：容器长度区间。定长数组 `[T;N]` 的长度是字面量 N；其余未知（长度是运行期值）。
func lenIvOf(p air.Place, fn *air.Func) (Iv, string) {
	place := placeText(p)
	// 形参/局部变量的类型文本里带 `[T;N]` → 长度 = N
	ty := typeOfPlace(p, fn)
	if strings.HasPrefix(ty, "[") && strings.HasSuffix(ty, "]") {
		if i := strings.LastIndex(ty, ";"); i > 0 {
			n, err := strconv.ParseInt(strings.TrimSuffix(ty[i+1:], "]"), 10, 64)
			if err == nil {
				return point(n), place
			}
		}
	}
	return unknown(), place
}

// typeOfPlace：从函数体里找该 place 的根变量声明类型（place 文本 → 根名）。
func typeOfPlace(p air.Place, fn *air.Func) string {
	root := ""
	switch v := p.(type) {
	case *air.VarPlace:
		root = v.Name
	case *air.ElemPlace:
		if vp, ok := v.Base.(*air.VarPlace); ok {
			root = vp.Name
		}
	case *air.FieldPlace:
		if vp, ok := v.Base.(*air.VarPlace); ok {
			root = vp.Name
		}
	}
	if root == "" {
		return ""
	}
	for _, b := range fn.Blocks {
		for _, in := range b.Insts {
			switch v := in.(type) {
			case *air.Var:
				if v.Name == root {
					return v.Ty
				}
			case *air.Let:
				if v.Tmp == root {
					return v.Ty
				}
			}
		}
	}
	for _, prm := range fn.Params {
		if prm.Name == root {
			return prm.Ty
		}
	}
	return ""
}

func placeText(p air.Place) string {
	switch v := p.(type) {
	case *air.VarPlace:
		return v.Name
	case *air.ElemPlace:
		return placeText(v.Base) + "[" + v.Idx + "]"
	case *air.FieldPlace:
		return fmt.Sprintf("%s.%d", placeText(v.Base), v.Idx)
	case *air.StrViewPlace:
		return "strview " + placeText(v.Base)
	}
	return "?"
}

// valIv：值文本 → 区间（常量 / 变量 / 临时量）。
func valIv(val string, iv map[string]Iv, fn *air.Func) Iv {
	switch {
	case val == "" || val == "nil":
		return point(0)
	case strings.HasPrefix(val, "const "):
		lit := strings.TrimSpace(strings.TrimPrefix(val, "const "))
		if n, err := strconv.ParseInt(lit, 0, 64); err == nil {
			return point(n)
		}
		return unknown()
	case strings.HasPrefix(val, "tmp "):
		return iv[strings.TrimPrefix(val, "tmp ")]
	case strings.HasPrefix(val, "var "):
		return iv[strings.TrimPrefix(val, "var ")]
	}
	if v, ok := iv[val]; ok {
		return v
	}
	return unknown()
}

// rhsIv：右值 → 区间（区间算术 + 长度读取）。
func rhsIv(r air.RHS, iv map[string]Iv, fn *air.Func) Iv {
	switch v := r.(type) {
	case *air.Const:
		return valIv("const "+v.Lit, iv, fn)
	case *air.TmpRef:
		return valIv(v.Name, iv, fn)
	case *air.VarRef:
		return valIv(v.Name, iv, fn)
	case *air.Binop:
		a, b := valIv(v.A, iv, fn), valIv(v.B, iv, fn)
		switch v.Op {
		case "+":
			return a.add(b)
		case "-":
			return a.sub(b)
		case "*":
			return a.mul(b)
		}
		return unknown()
	case *air.LenRHS:
		l, _ := lenIvOf(v.Place, fn)
		return l
	case *air.ElemRHS:
		return unknown() // 元素值区间未知（保守）
	}
	return unknown()
}

// fact 是一条块入口不等式（可选下界 / 可选上界）。
type fact struct {
	name         string
	lo, hi       int64
	hasLo, hasHi bool
}

// cmpOf：在块内找 cond 临时量的定义（`let c = cmp op a, b`）。
func cmpOf(b *air.Block, cond string) *air.Cmp {
	for _, in := range b.Insts {
		let, ok := in.(*air.Let)
		if !ok || let.Tmp != cond {
			continue
		}
		if c, isCmp := let.Rhs.(*air.Cmp); isCmp {
			return c
		}
	}
	return nil
}

// 便捷：把 cmp 的右操作数当常量取界（左操作数是归纳变量）。
// 右操作数不是字面量时返回「未知上界」哨兵（保守：证明不会成立）。
func bLo(c *air.Cmp) int64 { return constOf(c.B) }
func bHi(c *air.Cmp) int64 { return constOf(c.B) }

func constOf(val string) int64 {
	if strings.HasPrefix(val, "const ") {
		if n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(val, "const ")), 0, 64); err == nil {
			return n
		}
	}
	return 1 << 40 // 非字面量界：用大数表示「未知上界」（保守：证明不会成立）
}

// Summary 是报告摘要。
func (f *Facts) Summary() string {
	return fmt.Sprintf("range: accesses=%d proved-in-bounds=%d unknown=%d",
		len(f.Accesses), f.Proved, f.Unknown)
}
