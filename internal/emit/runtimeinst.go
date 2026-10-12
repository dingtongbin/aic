package emit

import (
	"sort"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 运行期容器/通道的**值类型元素**实例（emit 生成，按需）。
//
// 元素的运行期实例由 types.RuntimeSuffix 定名（唯一实现）。基本类型/接口/容器
// 元素在 runtime 头文件里**预置**了实例（AIC_LIST_OP(i64, aic_i64) 那一串），
// 但 struct 元素不可能预置 —— 用户类型有无限多个，按需生成：每个用到的 struct
// 元素发一份 `AIC_LIST_OP(<后缀>, <C 类型>)` / `AIC_CHAN_OP(...)`。
//
// 三条纪律：
//   ① 按需：没人引用的实例不发（门禁的覆盖率报告会盯上死代码）—— 数据源是
//      irTab（= 程序里出现过的全部类型）+ irRetTypes（合成返回结构）；
//   ② 确定性：发之前按 (kind, 后缀) 排序（H2：同输入两次编译逐字节相同）；
//   ③ 顺序：必须发在用户 struct typedef **之后**（宏体里 `sizeof(ctype)`
//      要求元素类型已完整）。
// ---------------------------------------------------------------------------

// rtInst 是一条运行期容器实例。
type rtInst struct {
	kind string // "chan" | "list"
	suf  string // types.RuntimeSuffix 给的定长十六进制后缀
	elem string // 元素的 C 类型文本
}

// registerRuntimeInsts 递归登记容器/通道的值类型元素实例。
func (c *Ctx) registerRuntimeInsts(t types.Type) {
	switch v := t.(type) {
	case *types.ChanT:
		c.registerRTElem("chan", v.Elem)
		// `chan[T].drain(n)` 的返回就是 `T[]` —— 同一个元素类型的 list 实例
		// 必须同时存在（chan 的宏体里直接调 aic_list_*_<后缀>）。
		// 隐式类型不进 irTab（它藏在方法签名里），所以在这里补注册。
		c.registerRTElem("list", v.Elem)
		c.registerRuntimeInsts(v.Elem)
	case *types.Slice:
		c.registerRTElem("list", v.Elem)
		c.registerRuntimeInsts(v.Elem)
	case *types.MapT:
		c.registerRuntimeInsts(v.Key)
		c.registerRuntimeInsts(v.Value)
	case *types.SetT:
		c.registerRuntimeInsts(v.Elem)
	case *types.ArrayT:
		c.registerRuntimeInsts(v.Elem)
	}
}

// registerRTElem 登记一条（只对 struct 元素；其余在 runtime 头里已预置）。
func (c *Ctx) registerRTElem(kind string, elem types.Type) {
	cl, ok := elem.(*types.Class)
	if !ok || !cl.Struct {
		return
	}
	suf, ok := types.RuntimeSuffix(cl)
	if !ok || suf == "" {
		return
	}
	key := kind + "|" + suf
	if c.rtSeen[key] {
		return
	}
	if c.rtSeen == nil {
		c.rtSeen = map[string]bool{}
	}
	c.rtSeen[key] = true
	c.rtInsts = append(c.rtInsts, rtInst{kind: kind, suf: suf, elem: c.cTypeName(cl)})
}

// collectRuntimeInsts 从程序的类型全集收集需要的实例。
func (c *Ctx) collectRuntimeInsts() {
	for _, t := range c.irTab {
		c.registerRuntimeInsts(t)
	}
	for _, ts := range c.irRetTypes {
		for _, t := range ts {
			c.registerRuntimeInsts(t)
		}
	}
}

// emitRuntimeInstances 发实例定义（在 emitInstances 之后，用户 struct 已声明）。
func (c *Ctx) emitRuntimeInstances() {
	if len(c.rtInsts) == 0 {
		return
	}
	sort.Slice(c.rtInsts, func(i, j int) bool {
		if c.rtInsts[i].kind != c.rtInsts[j].kind {
			return c.rtInsts[i].kind < c.rtInsts[j].kind
		}
		return c.rtInsts[i].suf < c.rtInsts[j].suf
	})
	c.line("/* --- 运行期容器实例：元素是 struct（用户类型无法预置，emit 按需生成）--- */")
	// **依赖序**：list 必须在 chan 之前 —— chan 的 `drain` 返回 `aic_list_<后缀> *`，
	// 反过来发就是"unknown type name"（036 实测）。所以按 kind 分**两趟**发，
	// 不是在一个 switch 里挑输出行（那仍是按 kind 字母序单趟）。
	for pass := 0; pass < 2; pass++ {
		for _, e := range c.rtInsts {
			switch {
			case pass == 0 && e.kind == "list":
				c.needed["l1"] = true
				c.line("AIC_LIST_OP(%s, %s)", e.suf, e.elem)
			case pass == 1 && e.kind == "chan":
				c.needed["l2"] = true
				c.line("AIC_CHAN_OP(%s, %s)", e.suf, e.elem)
			}
		}
	}
	c.line("")
}
