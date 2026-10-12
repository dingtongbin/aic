// 自动传播（§5.6）：fallible 调用在**语句位**与 **return 位实参**不需写
// check，语义即"失败即上抛"。
//
// 这是一等 IR-1 原语（PropagateInst）而不是 lowering 就地拆开：拆开就得在
// lowering 里重建 check 的分支 —— 那是把"哪些位置自动传播"的判断泄漏到
// lowering，而 lowering 已经够复杂了（§13.2 的单一实现位置判据）。
//
// 契约（IR-1 原语不变式）：发射原语后**必须**切换 l.cur 到新建的 After 块，
// 且 pre 块的 Term 保持 nil（由 desugar 填 Cbr；verifier 按 PrimAfter 走
// 可达性，见 verify.go 的 V2.3 IR-1 例外）。

package air

import (
	"aic/internal/parse"
	"aic/internal/types"
)

// fallibleElems 返回调用结果的**错误表**：多返回（末位 Err）或单个 Err。
// 返回 nil = 该调用不是 fallible 的（§5.6：传播只作用于这形态）。
//
// 两种形态必须一起认：`-> Err` 的函数与 `-> (T, Err)` 的函数都是 fallible。
// 只认 MultiType 会漏掉前者 —— 那正是 `log(x)` 这类"只报错不产值"的调用。
func fallibleElems(t types.Type) []types.Type {
	if m := types.MultiElems(t); len(m) >= 2 {
		return m
	}
	if types.IsErrType(t) {
		return []types.Type{t}
	}
	return nil
}

// isFallibleCall 报告表达式是否是**调用一个 fallible 函数**。
//
// 判据不能只看结果类型是不是 Err：`Err(E.Bad, "neg")` 是**构造器**（值形态），
// 它的类型也是 Err。把构造器当 fallible 调用会造出差一层的传播（实测：
// `return Err(...)` 被误判，IR 里多出一条永不执行的 checkfail）。
//
// 因此必须排除：内建 `Err(...)` 构造、`T(x)` 数值转换、`Class(...)` 类构造器、
// `Enum.Variant(...)` 变体直构。只认**本包/跨包函数**（info.Funcs 里的签名）。
func isFallibleCall(l *lowerer, x parse.Expr) bool {
	call, ok := x.(*parse.Call)
	if !ok {
		return false
	}
	switch fn := call.Fn.(type) {
	case *parse.Ident:
		// 类构造器 / 内建构造 / 转换：这些名字要么在 info.Classes 里，
		// 要么是基本类型名（Err / i32 / str / rune …），都不是 fallible 调用。
		if _, isCl := l.info.Classes[fn.Name]; isCl {
			return false
		}
		if sig, isFn := l.info.Funcs[fn.Name]; isFn {
			return sigResultsErr(sig)
		}
		// 不在 info.Funcs 里的裸名 = std 包函数 / 内建：std 无 fallible 调用
		// （fallible 面只有 Err 构造），故一律不算。
		return false
	case *parse.Index:
		// 显式类型实参的函数调用：`id[i64](x)` / `Box[i64].init(...)`
		if base, isID := fn.X.(*parse.Ident); isID {
			if sig, isFn := l.info.Funcs[base.Name]; isFn {
				return sigResultsErr(sig)
			}
		}
		return false
	case *parse.Field:
		// 方法调用 / 包函数：方法不会"只返回单个 Err 而不带前置位"，
		// 故只有多返回（末位 Err）这一种可能是 fallible。
		return len(types.MultiElems(l.typeOf(x))) >= 2
	}
	return false
}

func sigResultsErr(sig *types.FuncSig) bool {
	n := len(sig.Results)
	return n > 0 && types.IsErrType(sig.Results[n-1])
}

// emitPropagate 发射一条传播原语（§5.6），并把 l.cur 切到 After 块。
func (l *lowerer) emitPropagate(x parse.Expr, mode string) error {
	val, err := l.value(x)
	if err != nil {
		return err
	}
	mt := fallibleElems(l.typeOf(x))
	if len(mt) == 0 {
		return nil // 非 fallible 调用：无传播可做
	}
	after := l.newBlock("prop", parse.ExprPos(x))
	l.cur.Insts = append(l.cur.Insts, &PropagateInst{
		Operand: val, Mt: l.tyList(mt), Mode: mode, After: after.Label,
		Loc: LocOf(parse.ExprPos(x)),
	})
	l.cur = after
	return nil
}

// tyList 把类型表渲染成 AIR 类型文本表。
func (l *lowerer) tyList(ts []types.Type) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, l.ty(t))
	}
	return out
}
