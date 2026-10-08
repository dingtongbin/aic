package emit

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// F3：`for i in 0..xs.len()` 且循环体内**不改变 xs 的长度/数据指针** ⇒ 体内每次
// `xs[i]` 都满足 `i < len(xs)`（循环头每轮刚检查过）⇒ 越界检查恒真 ⇒ 发 unchecked。
//
// 事实的收集方式（上一轮教训：不写 AST 扫描器）：
//   把循环体先走一遍「**探测遍**」（c.probe = true，line() 不产文本），在发射器自己
//   遇到下列操作时记进 c.lenMutated：
//     - 变更类方法：append/set/clear/insert/remove/pop（接收者 = 容器）
//     - 把容器当实参传出（可能被别名修改）
//     - 下标写，且下标不是本循环的归纳变量（可能触发增长/重分配）
//   探测遍之后 clean = !lenMutated[容器]；发射遍据 clean 决定是否发 unchecked。
//   发射是纯函数，走两遍无副作用（临时量编号会跳号，但同一输入仍逐字节确定，H2 不变）。
// ---------------------------------------------------------------------------

// lenBoundFact 是 F3 的循环事实。
type lenBoundFact struct {
	induct    string // 归纳变量名
	container string // 上界取自它的 len()
	clean     bool   // 探测遍结论：体内不改长度/数据指针
}

// probeBody 走一遍探测遍，返回「可能改变长度/数据指针」的容器集合。
func (c *Ctx) probeBody(body *parse.Block, fnHasErr bool) (map[string]bool, map[string]*appendCountFact, error) {
	savedProbe, savedMutated, savedAppends := c.probe, c.lenMutated, c.appendFacts
	savedDecl := c.declaredVars
	c.probe, c.lenMutated, c.appendFacts = true, map[string]bool{}, map[string]*appendCountFact{}
	// 探测无副作用：循环体里声明的变量不得污染 declaredVars（否则 F5 会把缓存声明
	// 发到循环外，而那个容器其实是在循环体内才声明的）。
	c.declaredVars = copyBoolMap(savedDecl)
	err := c.emitBody(body, fnHasErr)
	mutated, appends := c.lenMutated, c.appendFacts
	c.probe, c.lenMutated, c.appendFacts = savedProbe, savedMutated, savedAppends
	c.declaredVars = savedDecl
	return mutated, appends, err
}

// markLenMutated 在探测遍里记下「该容器可能被改变长度/数据指针」。
func (c *Ctx) markLenMutated(e parse.Expr) {
	if !c.probe || c.lenMutated == nil {
		return
	}
	if id, ok := e.(*parse.Ident); ok {
		c.lenMutated[id.Name] = true
	}
}

// markLenMutatedName 同上（按名字）。
func (c *Ctx) markLenMutatedName(name string) {
	if c.probe && c.lenMutated != nil && name != "" {
		c.lenMutated[name] = true
	}
}

// lenCallTargetOf：`xs.len()` → "xs"；其他形态返回空。
func lenCallTargetOf(e parse.Expr) string {
	call, ok := e.(*parse.Call)
	if !ok {
		return ""
	}
	f, isField := call.Fn.(*parse.Field)
	if !isField || f.Name != "len" {
		return ""
	}
	if id, isID := f.X.(*parse.Ident); isID {
		return id.Name
	}
	return ""
}

// provedByLenBound：F3 判定 —— 下标恰为归纳变量、容器与上界容器同名且探测遍干净。
func (c *Ctx) provedByLenBound(container, idx parse.Expr) bool {
	lb := c.loopLenBound
	if lb == nil || !lb.clean {
		return false
	}
	id, ok := idx.(*parse.Ident)
	if !ok || id.Name != lb.induct {
		return false
	}
	cn, ok := container.(*parse.Ident)
	if !ok {
		return false
	}
	return cn.Name == lb.container
}

// indexNamesOf：`xs[i] = …` 的 LHS → ("xs", "i")；形态不符返回空串（保守不证明）。
func indexNamesOf(lhs parse.Expr) (string, string) {
	ix, ok := lhs.(*parse.Index)
	if !ok {
		return "", ""
	}
	recv, ok := ix.X.(*parse.Ident)
	if !ok {
		return "", ""
	}
	idx, ok := ix.Index.(*parse.Ident)
	if !ok {
		return "", ""
	}
	return recv.Name, idx.Name
}

// provedInBoundsByName：与 provedInBounds 同规则，但按名字（下标写路径用）。
func (c *Ctx) provedInBoundsByName(recvName, idxName string) bool {
	if recvName == "" || idxName == "" {
		return false
	}
	// F3：上界 = recvName.len() 且探测遍干净
	if lb := c.loopLenBound; lb != nil && lb.clean && lb.induct == idxName && lb.container == recvName {
		return true
	}
	// F4：符号长度式（填满再用形态）
	if c.provedBySymLenByName(recvName, idxName) {
		return true
	}
	// F2：上界字面量 + 容器长度常量
	ind := c.loopInduct
	if ind == nil || !ind.hasHi || ind.name != idxName || c.lenFacts == nil {
		return false
	}
	k, ok := c.lenFacts[recvName]
	if !ok {
		return false
	}
	return ind.hiConst <= int64(k)
}

// probeIndexUsed：单独探测一遍，返回「被下标访问的容器名」（F5 用）。
func (c *Ctx) probeIndexUsed(body *parse.Block, fnHasErr bool) map[string]bool {
	savedProbe, savedIdx, savedDecl := c.probe, c.indexUsed, c.declaredVars
	c.probe, c.indexUsed = true, map[string]bool{}
	// 探测遍必须**无副作用**：它会发射循环体，从而登记 declaredVars —— 若不还原，
	// 紧随其后的 emitCacheDecls 会误以为「内层声明的容器在循环前已可见」。
	c.declaredVars = copyBoolMap(savedDecl)
	_ = c.emitBody(body, fnHasErr)
	used := c.indexUsed
	c.probe, c.indexUsed, c.declaredVars = savedProbe, savedIdx, savedDecl
	return used
}

// copyBoolMap 浅拷贝（nil → 空表）。
func copyBoolMap(m map[string]bool) map[string]bool {
	out := make(map[string]bool, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
