package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// chan[T]（核心设计 §七）：channel 只传值，元素必须在**深拷贝安全集**内
// （基本类型 / str / @packed / 无负载 enum 及其容器——该集合无环，深拷贝必终止）。
// T1 面：send(v) / recv() -> (T, bool) / len()；无 select、无取消（【O3】【O4】）。
// ---------------------------------------------------------------------------

// ChanT 是 chan[T] 的语义类型。
type ChanT struct{ Elem Type }

func (*ChanT) typeNode() {}
func (t *ChanT) String() string {
	return "chan[" + t.Elem.String() + "]"
}

// chanSafeElem 报告元素类型是否在 channel 安全集内（§七：类引用不进 channel）。
func chanSafeElem(t Type) bool {
	if t == nil {
		return false
	}
	if b, ok := t.(*Basic); ok {
		return b.Name != "Err" // Err 按值可传，但语义上不是数据——一并允许亦可
	}
	if _, ok := t.(*Class); ok {
		return false // 类引用不进 channel（§七）
	}
	if en, ok := t.(*Enum); ok {
		return !en.HasData
	}
	if arr, ok := t.(*ArrayT); ok {
		return chanSafeElem(arr.Elem)
	}
	return false
}

// resolveChanType 处理 `chan[T]`（parse 把它解析成 NamedType{Name:"chan", Args:[T]}）。
func (c *Checker) resolveChanType(v *parse.NamedType) Type {
	if len(v.Args) != 1 {
		c.errorAt(v.Pos, "chan needs exactly one element type", "chan[…]",
			"write chan[T], e.g. chan[i32] (core design §7)")
		return nil
	}
	elem := c.resolveType(v.Args[0])
	if elem == nil {
		return nil
	}
	if !chanSafeElem(elem) {
		c.errorAt(v.Pos, "this element type cannot cross a channel", "chan["+elem.String()+"]",
			"a channel carries values only: basic types / str / @packed / payload-free enums and their containers (core design §7)")
		return nil
	}
	return &ChanT{Elem: elem}
}

// chanMethod 返回 channel 内建方法签名（§七）。
func chanMethod(t Type, name string) (*FuncSig, bool) {
	ch, ok := t.(*ChanT)
	if !ok {
		return nil, false
	}
	switch name {
	case "send":
		return &FuncSig{Name: name, Params: []string{"v"}, ParamTypes: []Type{ch.Elem}}, true
	case "recv":
		// recv() -> (T, bool)：空队列 = (零值, false)，非 trap（§七）
		return &FuncSig{Name: name, Results: []Type{ch.Elem, TBool}}, true
	case "len":
		return &FuncSig{Name: name, Results: []Type{TUsize}}, true
	case "cap":
		// 容量（0 = 无界）：背压上界是**可观察事实**（N11 的"有界/无界"由此可判）。
		return &FuncSig{Name: name, Results: []Type{TUsize}}, true
	}
	return nil, false
}
