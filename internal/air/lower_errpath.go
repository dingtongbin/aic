package air

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 错误通路 E2–E4 的去糖（§16）。
//
// 三者共享同一前缀：把操作数（末位 Err 的调用）降级成「多返回临时量」，再按末位
// Err 的 code ≠ 0 分支。差别只在失败路径做什么：
//
//   E2 `or`     失败 → 取默认值写进结果槽，继续
//   E3 `catch`  失败 → 绑定 e、执行块（块值写进结果槽），继续
//   E4 `!`      失败 → trap（块内指令，不是终结符：成功路径继续）
//
// 结果槽（`var <槽> : <负载类型>` + 两分支各一次 store）是 AIR 里表示"合流值"
// 的唯一手法（与返回槽同源：无 phi 也满足 —— 槽只定义一次，写入多次）。
// ---------------------------------------------------------------------------

// errSplit 把一个「末位 Err」的调用降级成 (多返回临时量, 负载值, Err值, 多返回表)。
// 负载 = 第 0 位；仅 Err 的调用在检查器已被拒，这里只做防御。
func (l *lowerer) errSplit(e parse.Expr, at parse.Pos) (res, payload, errVal string, mt []types.Type, err error) {
	res, err = l.value(e)
	if err != nil {
		return "", "", "", nil, err
	}
	mt = types.MultiElems(l.typeOf(e))
	if len(mt) < 2 {
		return "", "", "", nil, fmt.Errorf("air: an error path needs a call whose last result is Err (line %d)", at.Line)
	}
	last := len(mt) - 1
	payload = l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: payload, Ty: l.ty(mt[0]),
		Rhs: &MultiExtract{Val: res, Idx: 0}, Loc: LocOf(at)})
	errVal = l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: errVal, Ty: l.ty(mt[last]),
		Rhs: &MultiExtract{Val: res, Idx: last}, Loc: LocOf(at)})
	return res, payload, errVal, mt, nil
}

// errCodeIsSet 发射 `code != 0` 的判定值（Err 是 {code i32, msg str} 结构）。
func (l *lowerer) errCodeIsSet(errVal string, at parse.Pos) string {
	code := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: code, Ty: "i32",
		Rhs: &FieldRHS{Place: &VarPlace{Name: errVal}, Idx: 0}, Loc: LocOf(at)})
	bad := l.tmp("t")
	l.cur.Insts = append(l.cur.Insts, &Let{Tmp: bad, Ty: "bool",
		Rhs: &Cmp{Op: "ne", A: code, B: "const 0"}, Loc: LocOf(at)})
	return bad
}

// catchBody 发射 catch 块：有值形态（末条是表达式语句）把该表达式的值存进结果槽，
// 必退出形态（结尾是 return 等）整块照发、不写槽。判定与检查器同源
// （types.BlockAlwaysExits），禁第二份。
func (l *lowerer) catchBody(b *parse.Block, slot string, exits bool) error {
	if b == nil {
		return nil
	}
	if exits || len(b.Stmts) == 0 {
		return l.block(b)
	}
	last := b.Stmts[len(b.Stmts)-1]
	if es, isExprStmt := last.(*parse.ExprStmt); isExprStmt {
		if _, isAssign := es.X.(*parse.Assign); !isAssign {
			// 末条是表达式：先发前 n-1 条，再把末条的值存进槽。
			for i := 0; i+1 < len(b.Stmts); i++ {
				if err := l.stmt(b.Stmts[i]); err != nil {
					return err
				}
			}
			val, err := l.value(es.X)
			if err != nil {
				return err
			}
			l.cur.Insts = append(l.cur.Insts, &Store{
				Place: &VarPlace{Name: slot}, Val: val, Loc: LocOf(es.Pos)})
			return nil
		}
	}
	return l.block(b)
}
