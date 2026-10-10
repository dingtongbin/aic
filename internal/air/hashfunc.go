package air

import (
	"aic/internal/pkg"
)

// ---------------------------------------------------------------------------
// HashFunc：IR 函数体的内容哈希（F1 函数级翻译缓存的键原料）。
//
// 为什么不用 reflect.DeepEqual 之类的逐字段比：降级结果体量大（嵌套 Block /
// RHS / Place），逐字段序列化慢且容易在新形态上漏字段。这里走**打印-
// 再哈希**：Print 是 IR 的唯一权威文本形态（H8 门禁钉的就是它逐字节稳定），
// 于是"打印变 = 哈希变"，缓存随 IR 形态变化自然失效，零额外维护。
// ---------------------------------------------------------------------------

// HashFunc 返回一个函数体的稳定内容哈希（同一 IR 体两次调用结果一致）。
func HashFunc(f *Func) string {
	if f == nil {
		return pkg.Hash("nil-func")
	}
	return pkg.Hash("airfunc-v1", f.Sym, Print(&Module{Pkg: f.Sym, Funcs: []*Func{f}}))
}
