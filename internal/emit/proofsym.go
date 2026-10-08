package emit

import (
	"strconv"
	"strings"

	"aic/internal/parse"
)

// ---------------------------------------------------------------------------
// F4：**符号长度不变量**（§10.4 第 2 条的前置）——把「填满再用」这一主导形态钉住：
//
//	var xs T[] = []
//	for i in 0..limit+1 { xs.append(v) }      ⇒ len(xs) = limit + 1（线性式）
//	for m <= limit { xs[m] = v }              ⇒ m ≤ limit < limit+1 = len(xs) ⇒ 检查恒真
//
// 表示：线性式 = map[项]int64（项 = 标识符文本；空串 = 常数项）。
// 判定：`len(xs) − ub(idx)` 的符号恒正 ⇒ 证明成立。
// 线性项相消（limit 与 limit 抵掉、只剩常数）是这套表示的全部威力所在 —— 不做乘除、
// 不做区间合并；证不出来就一律发带检查形态（宁保守插检查，禁错误消除，红线 13）。
// ---------------------------------------------------------------------------

// lin 是线性式：terms["x"]=2 表示 2x；terms[""] = 常数项。
type lin map[string]int64

func linConst(k int64) lin { return lin{"": k} }

// linOf 把源码表达式转成线性式（只认：字面量、标识符、加减）。
func linOf(e parse.Expr) (lin, bool) {
	switch v := e.(type) {
	case *parse.IntLit:
		n, err := strconv.ParseInt(v.Text, 0, 64)
		if err != nil {
			return nil, false
		}
		return linConst(n), true
	case *parse.Ident:
		return lin{v.Name: 1}, true
	case *parse.Binary:
		a, ok1 := linOf(v.Left)
		b, ok2 := linOf(v.Right)
		if !ok1 || !ok2 {
			return nil, false
		}
		switch v.Op {
		case "+":
			return linAdd(a, b, 1), true
		case "-":
			return linAdd(a, b, -1), true
		}
	}
	return nil, false
}

func linAdd(a, b lin, sign int64) lin {
	out := lin{}
	for k, v := range a {
		out[k] += v
	}
	for k, v := range b {
		out[k] += sign * v
	}
	for k, v := range out {
		if v == 0 {
			delete(out, k)
		}
	}
	return out
}

// signOf 判定线性式的符号：只有**常数项**（无自由变量）时才能定号。
// 1 = 恒正；-1 = 恒负；0 = 恒零；2 = 未知（含自由变量）。
func signOf(l lin) int {
	for k := range l {
		if k != "" {
			return 2 // 含自由变量：不判定（保守）
		}
	}
	c := l[""]
	switch {
	case c > 0:
		return 1
	case c < 0:
		return -1
	default:
		return 0
	}
}

// lenSymFact 是一个容器的符号长度（线性式）。
type lenSymFact struct{ l lin }

// appendCountFact：探测遍里对某个容器的 append 次数与「是否只有 append 这一种变更」。
type appendCountFact struct {
	appends int
	other   bool // 有其他变更（set/clear/remove/pop/传出）→ 长度式不可推
}

// noteAppend 在探测遍里登记一次 append。
func (c *Ctx) noteAppend(name string) {
	if !c.probe || c.appendFacts == nil {
		return
	}
	f := c.appendFacts[name]
	if f == nil {
		f = &appendCountFact{}
		c.appendFacts[name] = f
	}
	f.appends++
}

// noteOtherMutation 在探测遍里登记「非 append 的变更」。
func (c *Ctx) noteOtherMutation(name string) {
	if !c.probe || c.appendFacts == nil {
		return
	}
	f := c.appendFacts[name]
	if f == nil {
		f = &appendCountFact{}
		c.appendFacts[name] = f
	}
	f.other = true
}

// updateLenSymAfterLoop：循环结束后按「每轮恰好一次 append」推进符号长度式。
// iters = hi − lo 的线性式（由调用方给出）；只在「只有 append、没有其他变更」时成立。
func (c *Ctx) updateLenSymAfterLoop(name string, iters lin) {
	if c.lenSym == nil {
		c.lenSym = map[string]lin{}
	}
	cur, ok := c.lenSym[name]
	if !ok {
		cur = linConst(0) // 未知长度：以 0 为基（append 计数从零起算）
	}
	c.lenSym[name] = linAdd(cur, iters, 1)
}

// provedBySymLen：F4 判定 —— 循环条件给出 idx 的上界，容器的符号长度给出下界，
// 两者相减恒正 ⇒ idx < len(xs)。
func (c *Ctx) provedBySymLen(container, idx parse.Expr) bool {
	cb := c.loopCondBound
	if cb == nil {
		return false
	}
	id, ok := idx.(*parse.Ident)
	if !ok || id.Name != cb.name {
		return false
	}
	cn, ok := container.(*parse.Ident)
	if !ok {
		return false
	}
	ln, ok := c.lenSym[cn.Name]
	if !ok {
		return false
	}
	// idx ≤ ub（`<=` 时上界就是 ub；`<` 时上界是 ub−1，两者都满足 idx < ub+1）
	diff := linAdd(ln, cb.ub, -1)
	if cb.strict { // `idx < ub` → idx ≤ ub−1 → 比较 len − (ub−1) = diff + 1
		diff = linAdd(diff, linConst(1), 1)
	}
	return signOf(diff) == 1
}

// provedBySymLenByName：按名字版本（下标写路径用）。
func (c *Ctx) provedBySymLenByName(recvName, idxName string) bool {
	cb := c.loopCondBound
	if cb == nil || cb.name != idxName {
		return false
	}
	ln, ok := c.lenSym[recvName]
	if !ok {
		return false
	}
	diff := linAdd(ln, cb.ub, -1)
	if cb.strict {
		diff = linAdd(diff, linConst(1), 1)
	}
	return signOf(diff) == 1
}

// condBoundOf：从 `for idx <= ub` / `for idx < ub` 的**条件形态**取上界事实。
func condBoundOf(cond parse.Expr) *condBound {
	bin, ok := cond.(*parse.Binary)
	if !ok {
		return nil
	}
	id, ok := bin.Left.(*parse.Ident)
	if !ok {
		return nil
	}
	switch bin.Op {
	case "<=", "<":
	default:
		return nil
	}
	ub, ok := linOf(bin.Right)
	if !ok {
		return nil
	}
	return &condBound{name: id.Name, ub: ub, strict: bin.Op == "<"}
}

// condBound 是循环条件给出的上界事实。
type condBound struct {
	name   string
	ub     lin
	strict bool
}

// loopItersOf：`for i in lo..hi` 的迭代次数（线性式 hi − lo）。
func loopItersOf(lo, hi parse.Expr) (lin, bool) {
	a, ok1 := linOf(lo)
	b, ok2 := linOf(hi)
	if !ok1 || !ok2 {
		return nil, false
	}
	return linAdd(b, a, -1), true
}

// isAppendCallOf：`xs.append(…)` → 返回 "xs"。
func isAppendCallOf(e parse.Expr) (string, bool) {
	call, ok := e.(*parse.Call)
	if !ok {
		return "", false
	}
	f, isField := call.Fn.(*parse.Field)
	if !isField || f.Name != "append" {
		return "", false
	}
	id, isID := f.X.(*parse.Ident)
	if !isID {
		return "", false
	}
	return id.Name, true
}

// trimSpaceAll 便于测试打印。
func trimSpaceAll(s string) string { return strings.TrimSpace(s) }
