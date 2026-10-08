package emit

import (
	"fmt"
	"sort"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// F5：循环体内容器访问的**缓存形态**（§10.4 第 2 条的无证明健全版）。
//
// 适用条件（都可机械检查）：容器是**本函数的局部切片变量**（裸名）、循环体内**句柄不变**
// （不被整体重新赋值）、且体内按下标访问它。缓存 len/data 后热路径只比较 + 读写；慢路径
// （越界/增长）走原访问器并刷新缓存 ⇒ 语义完全不变（越界写仍增长 + 补零、越界读仍 trap）。
// ---------------------------------------------------------------------------

// noteIndexAccess：探测遍里登记「被下标访问的容器名」。
func (c *Ctx) noteIndexAccess(name string) {
	if !c.probe || c.indexUsed == nil || name == "" {
		return
	}
	c.indexUsed[name] = true
}

// sliceCTypeOf：本地切片变量 → (后缀, C 元素类型)；非切片/未定型返回 false。
func (c *Ctx) sliceCTypeOf(name string) (string, string, bool) {
	t := c.nameType(name)
	if t == nil || !types.IsSlice(t) {
		return "", "", false
	}
	elem := types.SliceElem(t)
	suf, ok := c.containerSuffix(elem)
	if !ok {
		return "", "", false
	}
	return suf, c.cTypeName(elem), true
}

// reAssignedContainers：循环体内被**整体重新赋值**的变量（句柄会变 → 不能缓存）。
// 注意：Block 是语句**列表**，必须遍历 Stmts（不能把 Block 当单条语句递归 —— 会自递归）。
func reAssignedContainers(body *parse.Block) map[string]bool {
	out := map[string]bool{}
	var walk func(s parse.Stmt, depth int)
	walk = func(s parse.Stmt, depth int) {
		if s == nil || depth > 64 {
			return
		}
		switch v := s.(type) {
		case *parse.Block:
			if v == nil {
				return
			}
			for _, st := range v.Stmts {
				walk(st, depth+1)
			}
		case *parse.IfStmt:
			if v == nil {
				return
			}
			walk(v.Then, depth+1)
			walk(v.Else, depth+1)
		case *parse.ForStmt:
			if v == nil {
				return
			}
			walk(v.Body, depth+1)
		case *parse.RegionStmt:
			if v == nil {
				return
			}
			walk(v.Body, depth+1)
		case *parse.ExprStmt:
			if v == nil {
				return
			}
			if as, ok := v.X.(*parse.Assign); ok {
				if id, isID := as.LHS.(*parse.Ident); isID {
					out[id.Name] = true
				}
			}
		}
	}
	walk(body, 0)
	return out
}

// emitCacheDecls：为「被下标访问且句柄不变」的局部切片发缓存声明，并登记到 c.cached。
// 声明直接展开（不依赖宏里的 ctype 参数），三行一组、名字确定性。
func (c *Ctx) emitCacheDecls(body *parse.Block) {
	if len(c.indexUsed) == 0 {
		return
	}
	if c.cached == nil {
		c.cached = map[string]string{}
	}
	if c.cachedCT == nil {
		c.cachedCT = map[string]string{}
	}
	reassigned := reAssignedContainers(body)
	names := make([]string, 0, len(c.indexUsed))
	for n := range c.indexUsed {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if reassigned[n] {
			continue // 句柄可能变 → 不缓存（保守）
		}
		if !c.declaredVars[n] {
			continue // 循环开始前还不可见（内层声明的容器）→ 不能在这里发缓存声明
		}
		suf, ct, ok := c.sliceCTypeOf(n)
		if !ok {
			continue
		}
		c.cacheSeq++
		id := fmt.Sprintf("aic_cc_%d", c.cacheSeq)
		c.line("aic_list_%s *%s_h = %s;", suf, id, n)
		c.line("aic_usize %s_n = %s_h->len;", id, id)
		c.line("%s *%s_p = %s_h->data;", ct, id, id)
		c.cached[n] = id
		c.cachedCT[n] = ct
	}
}

// endCache：循环结束后撤销缓存登记（避免泄漏到循环外的访问点）。
func (c *Ctx) endCache() {
	c.cached = nil
	c.cachedCT = nil
}
