package types

import "aic/internal/pkg"

// ---------------------------------------------------------------------------
// 运行时容器/通道实例化后缀（**唯一实现**）。
//
// runtime 的 AIC_LIST_OP / AIC_SET_OP / AIC_CHAN_OP 表精确列出了可用后缀；
// emit 与 AIR 两侧都要按元素/键/值类型挑后缀（AIR 用来给构造调用命名，
// emit 用来选运行时符号），故后缀规则只能有一份 —— 本文件即那一份。
// ---------------------------------------------------------------------------

// RuntimeSuffix 报告元素类型在容器/通道实例化表里的短后缀。
// 规则：基本类型（除 Err）= 类型名；非 struct 类 = "Box"（void* 槽）；
// 接口 = "iface"（两字结构专用实例）；其余未实例化。
// 例外是 rune：语言里它是独立基本类型，运行期槽位就是 u32
// （aic_std_str_codepoints 交回 aic_list_u32 *）—— 没有 aic_list_rune
// 这一份实例，多开一份只会让同一个标量值有两种 C 形态。
func RuntimeSuffix(t Type) (string, bool) {
	if t == nil {
		return "", false
	}
	if b, ok := t.(*Basic); ok {
		switch b.Name {
		case "rune":
			return "u32", true
		case "i8", "i16", "i32", "i64", "u8", "u16", "u32", "u64", "usize",
			"f32", "f64", "bool", "str":
			return b.Name, true
		}
		return "", false
	}
	if cl, ok := t.(*Class); ok {
		if !cl.Struct {
			return "Box", true
		}
		// struct（值类型，按 C struct 布局）：无**预置**实例，但按类型唯一一份
		// —— 后缀是定长十六进制（pkg.RuntimeSym；无 `aic_` 前缀、无下划线，
		// 否则 `chan_<后缀>_recv` 的按 `_` 切段会乱），emit 见到这个后缀时
		// 为它发一份 AIC_CHAN_OP / AIC_LIST_OP / AIC_SET_OP / AIC_MAP_OP。
		if cl.PkgPath == "" && cl.Pkg == "" {
			return "", false
		}
		return pkg.RuntimeSym(cl.PkgPath, cl.Name), true
	}
	// 泛型实例：引用类实例 = 指针 ⇒ 与用户类指针共用 void* 槽位（Box）；
	// 泛型**枚举**实例（Option[T] / Result[T] / Value）是值类型（{tag, union}
	// 布局）⇒ 按类型唯一一份运行期实例（后缀含实参，不同实参不同实例）。
	// 缺这一条会让 `list[Result[i64]]` / `map[str]Value` 报"元素类型未实例化"。
	if inst, ok := t.(*Instance); ok {
		if cl, isCl := IsClass(inst.Base); isCl {
			if !cl.Struct {
				return "Box", true
			}
			return pkg.RuntimeSym(cl.PkgPath, instSegName(inst)), true
		}
		if en, isEn := inst.Base.(*Enum); isEn {
			return pkg.RuntimeSym(en.PkgPath, instSegName(inst)), true
		}
		return "", false
	}
	if _, ok := t.(*Interface); ok {
		return "iface", true
	}
	// audit ③：**嵌套容器元素走 Box 句柄槽** —— `i64[][]` = list of `void*`
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
	return false
}

// instSegName 是泛型实例的名字段（`Name[arg, arg]`）。运行期后缀与 C 类型名
// 都按它派生，所以它必须**只由类型结构决定**（同一实例永远同一段文本）。
func instSegName(inst *Instance) string {
	base := ""
	switch b := inst.Base.(type) {
	case *Class:
		base = b.Name
	case *Enum:
		base = b.Name
	case *Interface:
		base = b.Name
	}
	args := make([]string, 0, len(inst.Args))
	for _, a := range inst.Args {
		args = append(args, a.String())
	}
	return base + "[" + joinCommaText(args) + "]"
}

func joinCommaText(args []string) string {
	out := ""
	for i, a := range args {
		if i > 0 {
			out += ", "
		}
		out += a
	}
	return out
}

// RuntimeMapKeySuffix：map[K]V 的键后缀 = runtime/aic_l1_map.h 实例表的**精确互射**
// （POD 键 i32/i64/u32/u64/usize/bool + str 键按内容哈希）。
// enum / struct / i8/u8 等窄整型键暂无运行期实例（per-type 哈希属单态化面，P3）
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
	if cl, ok := t.(*Class); ok && !cl.Struct {
		return "Box", true
	}
	// 泛型枚举实例（Option[T] / Result[T] / Value）作 map 值：走 Box 句柄槽。
	// 判据：Value 动态值枚举的 Obj(map[str]Value) 需要它；否则"动态值"这类
	// 最基本的数据形态写不出来（场景验证 §3）。
	if _, ok := t.(*Instance); ok {
		return "Box", true
	}
	if en, ok := t.(*Enum); ok && en.HasData {
		return "Box", true
	}
	// 接口值槽：两机器字结构，专用实例（与列表的 iface 实例同款）。
	if _, ok := t.(*Interface); ok {
		return "iface", true
	}
	// audit ③：嵌套容器作 map 值同样走 Box 句柄槽（`map[str]i64[]` =
	// list 句柄塞 void* 值槽）；强转在 emit 侧。
	if isContainerTy(t) {
		return "Box", true
	}
	return "", false
}
