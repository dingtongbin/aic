package types

import "aic/internal/parse"

// ---------------------------------------------------------------------------
// chan[T]：channel 只传值，元素必须在**深拷贝安全集**内
// （基本类型 / str / struct / 无负载 enum 及其容器——该集合无环，深拷贝必终止）。
// 方法集：send(v) / recv() -> (T, Err) / tryRecv() -> Option[T] / drain(n) /
// len() / cap()。recv 的失败位是 **Err**（std.Code 的 ChanExhausted），不是 bool ——
// 错误必须进统一错误处理（标准库规范）；"拿不到"与"不会再有"由 Err 与 Option 区分。
// ---------------------------------------------------------------------------

// ChanT 是 chan[T] 的语义类型。
type ChanT struct{ Elem Type }

func (*ChanT) typeNode() {}
func (t *ChanT) String() string {
	return "chan[" + t.Elem.String() + "]"
}

// moveOK 报告类型是否满足 R-Move（《AIC 语言规范》§6.3）：可跨任务边界
// （spawn 实参 / chan 消息）。
//
// 集合：数值 / bool / str / rune / enum（载荷递归）/ chan / [T;N]（元素递归）/
// **slice / map / set** / 全字段 Move 的 struct / 全字段 Move 的 class。
//
// 判据：**禁的是 class 共享引用**——两个任务同持一个可变对象 = 共享可变
// 第三形态（§9.1）。slice/map/set 不禁：chan 的 send 语义本来就把元素
// **深拷贝进通道区域**，不存在共享。旧实现把 slice 一并禁掉，直接掐死
// 游戏/网络场景（场景验证 §6 实测）。
func moveOK(t Type) bool {
	switch v := t.(type) {
	case *Basic:
		return true // 全部 15 个基本类型都是值
	case *Enum:
		for _, va := range v.Variants {
			if va.Payload != nil && !moveOK(va.Payload) {
				return false
			}
		}
		return true
	case *ArrayT:
		return moveOK(v.Elem)
	case *Slice, *MapT, *SetT, *ChanT:
		// chan 深拷贝元素；容器按引用传递但不构成跨边界共享
		return true
	case *Instance:
		if cl, ok := v.Base.(*Class); ok {
			return moveClass(cl, v.Args)
		}
		if en, ok := v.Base.(*Enum); ok {
			for _, va := range en.Variants {
				if va.Payload != nil && !moveOK(subst(va.Payload, en.TypeParams, v.Args)) {
					return false
				}
			}
			return true
		}
		return false
	case *Class:
		return moveClass(v, nil)
	}
	return false
}

// moveClass 判定类（含泛型实例）是否 Move：全字段递归 Move 才可跨边界。
func moveClass(cl *Class, args []Type) bool {
	for i := range cl.Fields {
		ft := cl.Fields[i].Type
		if args != nil && len(cl.TypeParams) > 0 {
			ft = subst(ft, cl.TypeParams, args)
		}
		if !moveOK(ft) {
			return false
		}
	}
	return true
}

// firstNonMoveField 返回第一个非 Move 的字段（诊断用：列违规字段，§6.3）。
func firstNonMoveField(cl *Class, args []Type) string {
	for i := range cl.Fields {
		ft := cl.Fields[i].Type
		if args != nil && len(cl.TypeParams) > 0 {
			ft = subst(ft, cl.TypeParams, args)
		}
		if !moveOK(ft) {
			return cl.Name + "." + cl.Fields[i].Name + " (" + ft.String() + ")"
		}
	}
	return ""
}

// chanSafeElem 报告元素类型是否满足 R-Move（§6.3）。
func chanSafeElem(t Type) bool {
	return moveOK(t)
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
			"a channel carries values only: basic types / str / struct / payload-free enums and their containers (core design §7)")
		return nil
	}
	return &ChanT{Elem: elem}
}

// chanMethod 返回 channel 内建方法签名。
func chanMethod(t Type, name string) (*FuncSig, bool) {
	ch, ok := t.(*ChanT)
	if !ok {
		return nil, false
	}
	switch name {
	case "send":
		return &FuncSig{Name: name, Params: []string{"v"}, ParamTypes: []Type{ch.Elem}}, true
	case "recv":
		// recv() -> (T, Err)：Err = std.Code.ChanExhausted（无存活发送方且缓冲排空）。
		// 不用 bool：错误必须是 Err 才能进统一错误处理（标准库规范）。
		return &FuncSig{Name: name, Results: []Type{ch.Elem, TErr}}, true
	case "tryRecv":
		// 不存在 = Option，不是零值
		return &FuncSig{Name: name, Results: []Type{optionInstance(ch.Elem)}}, true
	case "drain":
		// 非阻塞取至多 n 个
		return &FuncSig{Name: name, Params: []string{"n"}, ParamTypes: []Type{TI64},
			Results: []Type{&Slice{Elem: ch.Elem}}}, true
	case "len":
		return &FuncSig{Name: name, Results: []Type{TUsize}}, true
	case "cap":
		// 容量（0 = 无界）：背压上界是**可观察事实**（"有界/无界"由此可判）。
		return &FuncSig{Name: name, Results: []Type{TUsize}}, true
	}
	return nil, false
}
