package emit

import (
	"strconv"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 「已证在界内」的证明义务（核心设计 §10.4 第 12 条②、红线 13）。
//
// 只有**可机械复核**的事实才允许发 unchecked 访问器，事实全部来自本函数源码形态：
//
//	F1 容器长度 = 常量 k：`var xs T[] = [a, b, c]`（容器字面量，k 个元素），
//	   且此后**无任何变更点**（append/set/clear/insert/remove/pop、或把 xs 传给
//	   调用/任务/lambda —— 后者可能别名修改）。
//	F2 循环归纳变量：`for i in lo..hi` 且 hi 是**字面量** → 体内 i ∈ [lo, hi)。
//
// 规则：下标恰为当前归纳变量、且 `hi <= k`（半开区间 → 最大下标 hi−1 < k）→ 证明成立。
// 证明不成立 = 一律发带检查形态（宁保守插检查，禁错误消除）。
// ---------------------------------------------------------------------------

// inductFact 是当前循环的归纳变量事实（栈式：嵌套取最内层）。
type inductFact struct {
	name    string
	hiConst int64
	hasHi   bool
}

// provedInBounds 判定 `container[idx]` 是否已证在界内。
func (c *Ctx) provedInBounds(container, idx parse.Expr) bool {
	// F3：上界 = 该容器的 len() 且探测遍证明体内不改长度/数据指针（见 proofscan.go）
	if c.provedByLenBound(container, idx) {
		return true
	}
	// F4：符号长度式（填满再用形态，见 proofsym.go）
	if c.provedBySymLen(container, idx) {
		return true
	}
	if c.loopInduct == nil || !c.loopInduct.hasHi {
		return false
	}
	id, ok := idx.(*parse.Ident)
	if !ok || id.Name != c.loopInduct.name {
		return false
	}
	name := containerName(container)
	if name == "" || c.lenFacts == nil {
		return false
	}
	k, ok := c.lenFacts[name]
	if !ok {
		return false
	}
	return c.loopInduct.hiConst <= int64(k)
}

// containerName 取容器表达式的根变量名（仅支持裸名；字段/下标形态保守放弃）。
func containerName(e parse.Expr) string {
	if id, ok := e.(*parse.Ident); ok {
		return id.Name
	}
	return ""
}

// noteLenFact 在 `var xs T[] = [a, b, c]` 时登记长度事实。
func (c *Ctx) noteLenFact(name string, init parse.Expr) {
	if c.lenFacts == nil {
		c.lenFacts = map[string]int{}
	}
	if lit, ok := init.(*parse.ArrayLit); ok {
		c.lenFacts[name] = len(lit.Elems)
		return
	}
	delete(c.lenFacts, name) // 非字面量初始化：长度未知
}

// invalidateLenFact 在任何可能的变更点作废长度事实（含把容器传给调用）。
func (c *Ctx) invalidateLenFact(name string) {
	if c.lenFacts != nil {
		delete(c.lenFacts, name)
	}
}

// invalidateContainerArg 在调用点作废被当实参传出的容器的长度事实（可能别名修改）。
func (c *Ctx) invalidateContainerArg(e parse.Expr) {
	if name := containerName(e); name != "" {
		c.invalidateLenFact(name)
	}
}

// isContainerType 报告表达式是否为容器类型（长度事实只对容器有意义）。
func isContainerType(t types.Type) bool {
	return types.IsSlice(t) || types.IsSet(t) || types.IsMap(t)
}

// litInt 取一个字面量整数的值（归纳上界识别用）。
func litInt(e parse.Expr) (int64, bool) {
	lit, ok := e.(*parse.IntLit)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(lit.Text, 0, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
