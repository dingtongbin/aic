package air

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 去糖补齐（T2 第三批续）：break/continue、容器字面量、多目标赋值。
// ---------------------------------------------------------------------------

// loopCtx 是循环上下文（break → done，continue → post）。
type loopCtx struct {
	post string
	done string
}

// branchStmt：break / continue → br 到循环的出口 / 步进块（无循环标签，§三）。
func (l *lowerer) branchStmt(v *parse.BranchStmt) error {
	if len(l.loops) == 0 {
		return fmt.Errorf("air: %s outside a loop (line %d)", v.Keyword, v.Pos.Line)
	}
	top := l.loops[len(l.loops)-1]
	target := top.done
	if v.Keyword == "continue" {
		target = top.post
	}
	l.cur.Term = &Br{Label: target, Loc: LocOf(v.Pos)}
	return nil
}

// arrayLit：容器字面量 → 逻辑调用（list.new / list.append），C 侧再定后缀。
// `[]`（空）与 `[a, b]` 同形：new + 逐元素 append（插入序 = 源序）。
//
// **定长数组字面量例外**：`[T;N]` 是内联值（不是句柄），既没有 list 实例也不该
// 有"追加"这种动态操作 —— 它必须降级成"零值 + 逐下标存储"，否则后端只能靠
// 结果类型去猜，而 `list.append` 里根本没有下标（下标的唯一权威在 IR 里）。
func (l *lowerer) arrayLit(v *parse.ArrayLit) (string, error) {
	ty := l.typeOf(v)
	if ty == nil {
		return "", fmt.Errorf("air: container literal without a declared type (line %d)", v.Pos.Line)
	}
	if types.IsArray(ty) {
		elemTy, n, _ := types.ArrayElem(ty)
		elemText := l.ty(elemTy)
		obj := l.tmp("lit")
		l.cur.Insts = append(l.cur.Insts, &Var{Name: obj, Ty: l.ty(ty), Loc: LocOf(v.Pos)})
		if len(v.Elems) > int(n) {
			return "", fmt.Errorf("air: the array literal has %d elements but its type holds %d (line %d)",
				len(v.Elems), n, v.Pos.Line)
		}
		for i, e := range v.Elems {
			val, err := l.value(e)
			if err != nil {
				return "", err
			}
			// 元素槽是接口而元素值是具体类 ⇒ 显式装箱。
			val = l.coerce(val, e, elemText)
			l.cur.Insts = append(l.cur.Insts, &Store{
				Place: &ElemPlace{Base: &VarPlace{Name: obj}, Idx: fmt.Sprintf("const %d", i)},
				Val:   val, Loc: LocOf(v.Pos)})
		}
		_ = elemTy
		return obj, nil
	}
	kind := "list"
	elem := "i32"
	switch {
	case types.IsSlice(ty):
		elem = l.ty(types.SliceElem(ty))
	case types.IsSet(ty):
		kind = "set"
		elem = l.ty(types.SetElem(ty))
	}
	obj := l.tmp("lit")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: obj, Ty: l.ty(ty),
		Rhs: &Call{Sym: kind + ".new", TypeArgs: []string{elem}}, Loc: LocOf(v.Pos)})
	for _, e := range v.Elems {
		val, err := l.value(e)
		if err != nil {
			return "", err
		}
		// 元素槽是接口而元素值是具体类 ⇒ 显式装箱（`var xs Shape[] = [a, b, c]`）。
		val = l.coerce(val, e, elem)
		add := kind + ".append"
		if kind == "set" {
			add = "set.add"
		}
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: l.tmp("t"), Ty: "void",
			Rhs: &Call{Sym: add, TypeArgs: []string{elem}, Args: []string{obj, val}}, Loc: LocOf(v.Pos)})
	}
	return obj, nil
}

// multiAssign：`a, err = f()` → call + 逐位 multi.extract + store 到各自 place。
func (l *lowerer) multiAssign(v *parse.MultiAssign) error {
	res, err := l.value(v.Right)
	if err != nil {
		return err
	}
	mt := types.MultiElems(l.typeOf(v.Right))
	for i, tgt := range v.LHS {
		ty := "i32"
		if mt != nil && i < len(mt) {
			ty = l.ty(mt[i])
		}
		t := l.tmp("t")
		l.cur.Insts = append(l.cur.Insts, &Let{Tmp: t, Ty: ty,
			Rhs: &MultiExtract{Val: res, Idx: i}, Loc: LocOf(v.Pos)})
		if id, isID := tgt.(*parse.Ident); isID && id.Name == "_" {
			continue
		}
		place, err := l.place(tgt)
		if err != nil {
			return err
		}
		l.cur.Insts = append(l.cur.Insts, &Store{Place: place, Val: t, Loc: LocOf(v.Pos)})
	}
	return nil
}
