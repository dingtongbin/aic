// match 的 IR-1 降低：发 MatchTerm / MatchExprRHS 原语（§13.2：match 是一等
// 原语，不被拆成 tag 取值 + switch）。
//
// **为什么必须是一等原语**：臂内绑定的 BindTy 含类型形参。泛型模板里的
// `match o { Option.Some(v) => … }`，v 的类型是 `T`。降低成 EnumTag +
// Switch + EnumPayload 之后，"哪个臂绑定什么类型"的对应关系就散了 ——
// 单态化无法把实例实参送到正确的臂上。IR-1 把 BindTy 与 arm 绑在一起，mono
// 只在原语节点上替换，去糖（desugar）才机械地按已实参化的 BindTy 发 Var。
//
// 契约（ir_prim.go 的 IR-1 不变式）：
//   - lowering 建：subject 值、每臂体块（Body，其 Term 定为 Br After）、After 块；
//   - 原语只发在 pre 块（Term 留 nil，由 desugar 填 Switch）；
//   - 不跨块改写：臂体块的 Term 由 lowering 设成 Br After，desugar 只管插
//     分发链与 payload 绑定。

package air

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// matchArms 把一个 match 的臂表降低成 IR-1 臂表：每臂建一个体块（块形分支
// 直接降低；表达式形分支折成"求值 + 存槽 + Br After"的隐式块）。
//
// slotTo 非空 = 表达式位 match（每臂把值存进该槽）；空 = 语句位。
func (l *lowerer) matchArms(arms []parse.MatchArm, en *types.Enum, inst *types.Instance, slotTo string, after *Block) ([]MatchArm, error) {
	out := make([]MatchArm, 0, len(arms))
	for i, a := range arms {
		pats, err := l.matchPatterns(en, inst, a)
		if err != nil {
			return nil, err
		}
		body := l.newBlock(fmt.Sprintf("marm%d", i+1), a.Pos)
		saved := l.cur
		l.cur = body
		switch {
		case a.Block != nil:
			if err := l.block(a.Block); err != nil {
				return nil, err
			}
		case a.Value != nil:
			val, err := l.value(a.Value)
			if err != nil {
				return nil, err
			}
			l.cur.Insts = append(l.cur.Insts, &Store{
				Place: &VarPlace{Name: slotTo}, Val: val, Loc: LocOf(a.Pos)})
		default:
			return nil, fmt.Errorf("air: a match arm carries neither a block nor a value (line %d)", a.Pos.Line)
		}
		// 臂体自己设了 Term（return / break / continue）就不覆盖 —— 只有
		// "顺序流到底"的臂才汇合到 After。
		if l.cur.Term == nil {
			l.cur.Term = &Br{Label: after.Label, Loc: LocOf(a.Pos)}
		}
		l.cur = saved
		out = append(out, MatchArm{Patterns: pats, Body: body.Label, Loc: LocOf(a.Pos)})
	}
	return out, nil
}

// matchPatterns 解析一个臂的多模式（`A | B =>`）。
//
// BindTy 是**已实参化**的绑定类型：`Option[i32].Some` 的绑定 = i32（泛型
// 模板里 = T，由 mono 替换）。这是 IR-1 必须携带的信息（见文件头）。
func (l *lowerer) matchPatterns(en *types.Enum, inst *types.Instance, a parse.MatchArm) ([]MatchPattern, error) {
	out := make([]MatchPattern, 0, len(a.Patterns))
	for _, p := range a.Patterns {
		if en == nil {
			return nil, fmt.Errorf("air: match arm on a non-enum subject (line %d)", p.Pos.Line)
		}
		idx, va, ok := types.VariantByName(en, p.Name)
		if !ok {
			return nil, fmt.Errorf("air: pattern %s does not name a variant of %s (line %d)",
				p.Name, en.Name, p.Pos.Line)
		}
		pat := MatchPattern{
			Variant: va.Name,
			Tag:     idx,
			Binding: p.Binding,
			HasPay:  va.Payload != nil,
			Loc:     LocOf(p.Pos),
		}
		if va.Payload != nil {
			pl := va.Payload
			if inst != nil {
				pl = types.Subst(pl, en.TypeParams, inst.Args)
			}
			pat.BindTy = l.ty(pl)
		}
		out = append(out, pat)
	}
	return out, nil
}

// matchSubject 求值 match 主体并解出枚举（泛型实例 → 基枚举 + 实参）。
func (l *lowerer) matchSubject(v parse.Expr) (string, *types.Enum, *types.Instance, error) {
	val, err := l.value(v)
	if err != nil {
		return "", nil, nil, err
	}
	en, inst := enumOf(l.typeOf(v))
	return val, en, inst, nil
}
