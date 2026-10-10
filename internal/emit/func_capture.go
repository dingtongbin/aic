package emit

import "strings"

// ---------------------------------------------------------------------------
// 函数体发射的捕获机制（F1 缓存用）。
//
// Ctx.buf 是 strings.Builder 值，c.w/c.line 一律写它。把 buf 换成捕获缓冲即可
// 把"一个函数的全部输出"整段收进来（不影响任何其他路径：capture 只在
// irOneFunc 的一进一出之间活着——函数体发射不会再调 irOneFunc）。
// ---------------------------------------------------------------------------

// funcCapture 是一次函数体发射的捕获上下文（saved = 主输出的快照）。
type funcCapture struct {
	saved strings.Builder
}

// beginFuncCapture 打开捕获；on=false 返回 nil（不启用）。
func (c *Ctx) beginFuncCapture(on bool) *funcCapture {
	if !on {
		return nil
	}
	fc := &funcCapture{saved: c.buf} // 主输出快照
	c.buf = strings.Builder{}        // 改道
	return fc
}

// endFuncCapture 结束捕获：取文本 → 还原主输出 → 原样追加回主输出 → 写缓存。
// 文本必须**原样追加回主输出**（调用方本来就要这段 C），缓存只是顺手。
// key 为空 = 没开缓存（直接还原 + 追加）。
func (c *Ctx) endFuncCapture(fc *funcCapture, key string) {
	if fc == nil {
		return
	}
	text := c.buf.String()
	c.buf = fc.saved // 还原主输出
	c.buf.WriteString(text)
	if key != "" {
		curFuncCache.Put(key, []byte(text))
	}
}