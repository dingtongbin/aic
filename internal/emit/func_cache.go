package emit

import (
	"aic/internal/air"
	"aic/internal/pkg"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// F1 增量翻译：函数级 C 文本缓存（R23 F1）。
//
// 实测依据（build/f1_measure2.py）：大程序翻译段 47ms、C 编译段仅 3.6ms ——
// 缓存的杠杆全在翻译。函数级粒度 = "改一个函数只重翻一个函数"的最小可用单位，
// 与 emit 现状（每函数独立发射）天然对齐。
//
// 键 = IR 函数体内容哈希 + 签名 + C 符号 + 存储类 + 编译器身份。
// 值 = 该函数整段 C 文本。命中即原文落盘，跳过全部发射。
//
// 安全口径（与产物缓存同纪律）：
//   - `--emit-c`（门禁 H2/H3 判发射确定性）**永不走缓存**（No=true）；
//   - `--no-cache` 关闭；缓存写失败不影响构建；
//   - 编译器身份进键（K6）。
// ---------------------------------------------------------------------------

// FuncCache 是函数级翻译缓存的句柄（nil = 不用缓存）。
type FuncCache struct {
	store  *pkg.CacheStore
	prefix string
	No     bool
	hits   int
	misses int
}

// NewFuncCache 建缓存（dir = 缓存根；prefix = 键前缀，含编译器身份）。
func NewFuncCache(dir, prefix string, no bool) *FuncCache {
	return &FuncCache{store: pkg.NewCacheStore(dir, prefix), prefix: prefix, No: no}
}

// Stats 返回命中/未命中计数（诊断用）。
func (fc *FuncCache) Stats() (hits, misses int) {
	if fc == nil {
		return 0, 0
	}
	return fc.hits, fc.misses
}

// Get 查一片函数 C 文本。
func (fc *FuncCache) Get(key string) ([]byte, bool) {
	if fc == nil || fc.No || key == "" {
		return nil, false
	}
	if b, ok := fc.store.Get(key); ok {
		fc.hits++
		return b, true
	}
	fc.misses++
	return nil, false
}

// Put 写一片函数 C 文本（失败静默：缓存是优化不是正确性）。
func (fc *FuncCache) Put(key string, data []byte) {
	if fc == nil || fc.No || key == "" {
		return
	}
	_ = fc.store.Put(key, data)
}

// funcCacheKey 是函数级缓存的键（稳定、含身份、含结构）。
func funcCacheKey(sym string, sig *types.FuncSig, store, irHash string) string {
	parts := []string{
		"funccache-v1",
		pkg.CompilerID(),
		"sym:" + sym,
		"store:" + store,
		"recv:" + sig.Recv,
		"name:" + sig.Name,
	}
	for i, p := range sig.Params {
		ty := ""
		if i < len(sig.ParamTypes) {
			ty = air.TyText(sig.ParamTypes[i])
		}
		parts = append(parts, "param:"+p+":"+ty)
	}
	for _, r := range sig.Results {
		parts = append(parts, "ret:"+air.TyText(r))
	}
	if irHash != "" {
		parts = append(parts, "ir:"+irHash)
	}
	return pkg.Hash(parts...)
}

// ---------------------------------------------------------------------------
// 包级当前缓存（CLI 侧设置；emitIRUnit 建 Ctx 时取用）。
// 包级而不是 Ctx 字段的理由：emit 入口在 emit 包内、CLI 在 cmd 包外，
// 走参数要改三处签名；包级 + 显式 Set/Reset 语义清晰（一次构建一个缓存）。
// ---------------------------------------------------------------------------

var curFuncCache *FuncCache

// SetFuncCache 设置/清空当前函数缓存（nil = 关闭）。CLI 在构建前后调用。
func SetFuncCache(fc *FuncCache) { curFuncCache = fc }

// FuncCacheStats 返回当前缓存的命中统计（CLI 的 --cache-stats 用）。
func FuncCacheStats() (hits, misses int) { return curFuncCache.Stats() }
