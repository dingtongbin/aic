package emit

import (
	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 语句发射 (核心设计 §三/§五/§六)。
//
// 每条语句前带 #line (红线 9); 块 = C 块; 遮蔽规则由检查器保证, 发射只按
// 已解析的名字走 (同块重名已在检查期拒绝, 故 C 变量名可直接用 AIC 名)。
// ---------------------------------------------------------------------------

// declType 取声明类型: 显式类型优先, 否则用检查器记下的初值类型。
func (c *Ctx) declType(v *parse.VarDecl) types.Type {
	if v.Type != nil {
		if t := c.typeOfTypeExpr(v.Type); t != nil {
			return t
		}
	}
	if t := c.ti(v.Init); t != nil {
		return t
	}
	return types.TI32 // 兜底 (检查器已保证可推断, 走到这里说明类型表缺项)
}

// retStructNameFor 由返回类型列表合成结构名 (与 retStructName 同一规则)。
func (c *Ctx) retStructNameFor(elems []types.Type) string {
	if n, ok := stdRetStruct(elems); ok {
		return n
	}
	parts := make([]string, 0, len(elems))
	for _, r := range elems {
		parts = append(parts, sanitize(c.cTypeName(r)))
	}
	return "aic_r_" + joinUnder(parts)
}

func joinUnder(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "_"
		}
		out += p
	}
	return out
}
