package emit

import (
	"fmt"

	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 调用发射 (核心设计 §二.2 容器方法 / §二.3 str / §四 构造 / §六 Err / §十四 std)。
//
// 约定: 一切调用都在编译期定型 (泛型单态化 + 静态派发, §〇 CPU 条款),
// 运行时函数名来自各自的单表 (容器后缀表 / std 名表 / mangling 表)。
// ---------------------------------------------------------------------------

// lookupVariant 在本包 enum 中找变体构造名 (单态化: Option 的 Some 亦在此二义,
// 由检查器已定型的负载类型区分)。
func (c *Ctx) lookupVariant(name string) (*types.Enum, types.Type, bool) {
	if c.Info == nil {
		return nil, nil, false
	}
	for _, en := range c.Info.Enums {
		if p, ok := en.VariantPayload(name); ok {
			return en, p, true
		}
	}
	return nil, nil, false
}

// variantNoData 发射无数据变体的**值**：无数据 enum 就是该 C 枚举常量本身；
// 带数据 enum 的值类型是 {tag, union} 结构，必须补成结构字面量——直接把
// tag 常量当值用会让 C 报「参数类型不符」（枚举常量是 tag 枚举类型，不是结构）。
func (c *Ctx) variantNoData(en *types.Enum, ty, variant string) string {
	if en.HasData {
		return fmt.Sprintf("((%s){ .tag = %s_%s })", ty, ty, variant)
	}
	return fmt.Sprintf("%s_%s", ty, variant)
}

// initMethod 取类的 init 构造函数 (§四: 至多一个、无重载、可 -> Err)。
func (c *Ctx) initMethod(cl *types.Class) (*types.FuncSig, bool) {
	for name := range cl.Methods {
		if sig, ok := cl.Method(name); ok && sig.IsInit {
			return sig, true
		}
	}
	return nil, false
}
