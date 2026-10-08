package emit

import (
	"fmt"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// chan[T] 发射（核心设计 §七；运行时见 runtime/aic_l2_chan.h）。
//
//   chan[T].new()  →  aic_chan_new_<suf>(cap, line)
//   ch.send(v)     →  aic_chan_send_<suf>(ch, v, file, line)
//   ch.recv()      →  aic_chan_recv_<suf>(ch, file, line)  /* 返回 {_0, _1} 与合成返回结构同形 */
//   ch.len()       →  aic_chan_len_<suf>(ch)
//
// 后缀表与容器同一张（containerSuffix）：chan 的安全集 ⊂ 容器实例化集。
// L3：send/recv 会**阻塞**（有界满 / 空），故两处都带 file/line（trap 要指到源码位）。
// ---------------------------------------------------------------------------

// chanSuffix 取 chan[T] 的运行时后缀（唯一实现）。
func (c *Ctx) chanSuffix(t types.Type) (string, bool) {
	ch, ok := t.(*types.ChanT)
	if !ok {
		return "", false
	}
	return c.containerSuffix(ch.Elem)
}

// chanRecvStruct 登记 chan recv 的返回结构与运行时名字的对应（避免重复 typedef）。
func (c *Ctx) chanRecvStruct(t types.Type) (string, bool) {
	ch, ok := t.(*types.ChanT)
	if !ok {
		return "", false
	}
	suf, ok := c.chanSuffix(t)
	if !ok {
		return "", false
	}
	key := typeSeg(ch.Elem) + "_bool"
	name := "aic_chan_" + suf + "_recv_t"
	stdRetStructs[key] = name
	return name, true
}

// tryChanCall 发射 chan 的方法调用与构造。
func (c *Ctx) tryChanCall(v *parse.Call, want types.Type) (string, bool, error) {
	// 构造：chan[T].new()
	if field, ok := v.Fn.(*parse.Field); ok && field.Name == "new" {
		if ix, isIx := field.X.(*parse.Index); isIx {
			if id, isID := ix.X.(*parse.Ident); isID && id.Name == "chan" {
				ct := c.ti(v)
				suf, has := c.chanSuffix(ct)
				if !has {
					return "", true, fmt.Errorf("emit: chan[T].new() needs an instantiated element type (line %d)", v.Pos.Line)
				}
				c.need("l2")
				// N11：new() = 无界（cap 0）；new(cap) = 有界（满即背压点）。
				cap := "0"
				if len(v.Args) == 1 {
					e, err := c.expr(v.Args[0], types.TUsize)
					if err != nil {
						return "", true, err
					}
					cap = e
				} else if len(v.Args) > 1 {
					return "", true, fmt.Errorf("emit: chan.new takes at most one capacity (line %d)", v.Pos.Line)
				}
				return fmt.Sprintf("aic_chan_new_%s(%s, %du)", suf, cap, uint32(v.Pos.Line)), true, nil
			}
		}
	}
	// 方法：recv.send(...) / recv.recv() / recv.len()
	field, ok := v.Fn.(*parse.Field)
	if !ok {
		return "", false, nil
	}
	rt := c.ti(field.X)
	suf, has := c.chanSuffix(rt)
	if !has {
		return "", false, nil
	}
	c.need("l2")
	recv, err := c.expr(field.X, rt)
	if err != nil {
		return "", true, err
	}
	switch field.Name {
	case "send":
		ch := rt.(*types.ChanT)
		a, err := c.arg(v, 0, ch.Elem)
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("aic_chan_send_%s(%s, %s, %s, %d)", suf, recv, a, cstr(c.Path), v.Pos.Line), true, nil
	case "recv":
		c.chanRecvStruct(rt)
		// file/line 一并传：recv 现在会**阻塞**，死锁/取消的 trap 要指到源码位。
		return fmt.Sprintf("aic_chan_recv_%s(%s, %s, %d)", suf, recv, cstr(c.Path), v.Pos.Line), true, nil
	case "len":
		return fmt.Sprintf("aic_chan_len_%s(%s)", suf, recv), true, nil
	case "cap":
		// 容量（0 = 无界）；运行时的 aic_chan_cap_<suf> 早已存在，此前只是没接到语言面。
		return fmt.Sprintf("aic_chan_cap_%s(%s)", suf, recv), true, nil
	}
	return "", true, fmt.Errorf("emit: chan has no method %s (line %d)", field.Name, v.Pos.Line)
}
