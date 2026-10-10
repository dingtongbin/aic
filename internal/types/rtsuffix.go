package types

// ---------------------------------------------------------------------------
// 运行时容器/通道实例化后缀（**唯一实现**）。
//
// runtime 的 AIC_LIST_OP / AIC_SET_OP / AIC_CHAN_OP 表精确列出了可用后缀；
// emit 与 AIR 两侧都要按元素/键/值类型挑后缀（AIR 用来给构造调用命名，
// emit 用来选运行时符号），故后缀规则只能有一份 —— 本文件即那一份。
// ---------------------------------------------------------------------------

// RuntimeSuffix 报告元素类型在容器/通道实例化表里的短后缀。
// 规则：基本类型（除 Err）= 类型名；非 @packed 类 = "Box"（void* 槽）；
// 接口 = "iface"（两字结构专用实例）；其余未实例化。
func RuntimeSuffix(t Type) (string, bool) {
	if t == nil {
		return "", false
	}
	if b, ok := t.(*Basic); ok {
		switch b.Name {
		case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize",
			"f32", "f64", "bool", "str":
			return b.Name, true
		}
		return "", false
	}
	if cl, ok := t.(*Class); ok {
		if !cl.Packed {
			return "Box", true
		}
		return "", false
	}
	// 泛型实例：引用类实例 = 指针 ⇒ 与用户类指针共用 void* 槽位（Box）；
	// @packed 实例是值类型（按 C struct 布局）⇒ 无运行期实例后缀。
	// 缺这一条会让 `Box[i32][]` / `map[str]Box[i32]` 报"元素类型未实例化"。
	if inst, ok := t.(*Instance); ok {
		if cl, isCl := IsClass(inst.Base); isCl && !cl.Packed {
			return "Box", true
		}
		return "", false
	}
	if _, ok := t.(*Interface); ok {
		return "iface", true
	}
	// R21 audit ③：**嵌套容器元素走 Box 句柄槽** —— `i64[][]` = list of `void*`
	// （内层 list 本身就是句柄），map/set 值同理。语义类型与槽位之间的强转在
	// emit 侧（irBoxElem/irUnboxElem）。
	if isContainerTy(t) {
		return "Box", true
	}
	return "", false
}

// isContainerTy 报告类型是否为"能被 Box 句柄槽容纳"的容器（slice/map/set/chan/bytes）。
// 定长数组 [T;N] 是值类型（C 内联布局）不走句柄槽。
func isContainerTy(t Type) bool {
	switch t.(type) {
	case *Slice, *MapT, *SetT, *ChanT:
		return true
	}
	return IsBytes(t)
}

// RuntimeMapKeySuffix：map[K]V 的键后缀 = runtime/aic_l1_map.h 实例表的**精确互射**
// （POD 键 i32/i64/u32/u64/usize/bool + str 键按内容哈希）。
// enum / @packed / i8/u8 等窄整型键暂无运行期实例（per-type 哈希属单态化面，P3）
// ⇒ 检查器必须按本表提前拒（审计 ③：旧行为是检查器放行、emit 才炸
// "malformed map symbol" —— 不可操作的晚期错误）。
func RuntimeMapKeySuffix(t Type) (string, bool) {
	b, ok := t.(*Basic)
	if !ok {
		return "", false
	}
	switch b.Name {
	case "i32", "i64", "u32", "u64", "usize", "bool", "str":
		return b.Name, true
	}
	return "", false
}

// RuntimeMapValueSuffix：map 的值槽位表 = aic_l1_map.h 实例表的**精确互射**：
// i32/i64/f64/bool/str + 引用槽 Box + 两字接口 iface。
// u8/u16/u32/u64/usize/f32 与容器值暂无实例 ⇒ 检查器提前拒并给可操作替代
// （审计 ③；bool 是 2026-10 补的最高频缺口）。
func RuntimeMapValueSuffix(t Type) (string, bool) {
	if b, ok := t.(*Basic); ok {
		switch b.Name {
		case "i32", "i64", "f64", "bool", "str":
			return b.Name, true
		}
		return "", false
	}
	if cl, ok := t.(*Class); ok && !cl.Packed {
		return "Box", true
	}
	// 接口值槽：两机器字结构，专用实例（与列表的 iface 实例同款）。
	if _, ok := t.(*Interface); ok {
		return "iface", true
	}
	// R21 audit ③：嵌套容器作 map 值同样走 Box 句柄槽（`map[str]i64[]` =
	// list 句柄塞 void* 值槽）；强转在 emit 侧。
	if isContainerTy(t) {
		return "Box", true
	}
	return "", false
}
