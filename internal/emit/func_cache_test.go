package emit

import (
	"testing"

	"aic/internal/types"
)

// TestFuncCacheKeyGlobalUnique 是阶段 3 退出条件③的单测：
// L1（函数级翻译缓存）键必须**含全局唯一符号的哈希**，且满足三条性质：
//
//	① 同名不同包（app/core.parse 与 app/util.parse）不撞键 —— 否则一个包改了
//	   另一个包命中脏产物（大项目里就是随机串味，且只在特定构建顺序下复现）；
//	② 同输入恒等（H2 确定性），且键是 sha256 形态（含符号哈希，不是裸符号串）；
//	③ 符号变 / IR 体变 ⇒ 键变（改名或改体 = 新键，旧的自然失效，不腐化）。
//
// ④ 加一个新依赖不改已有函数的键：键的输入里**没有**依赖集合 —— 这条由
//
//	函数签名（无依赖参数）钉住，测试把它写成断言以免将来有人把 deps 塞进去。
func TestFuncCacheKeyGlobalUnique(t *testing.T) {
	sig := &types.FuncSig{Name: "parse", Params: []string{"s"}, ParamTypes: []types.Type{types.TStr}}

	coreParse := MangleFunc("app/core", "", "parse")
	utilParse := MangleFunc("app/util", "", "parse")
	if coreParse == utilParse {
		t.Fatal("前提失效：同名不同包应得不同符号")
	}

	a := funcCacheKey(coreParse, sig, "internal", "irhash-1")
	b := funcCacheKey(utilParse, sig, "internal", "irhash-1")
	if a == b {
		t.Fatalf("同名不同包撞键：%s", a)
	}

	if again := funcCacheKey(coreParse, sig, "internal", "irhash-1"); again != a {
		t.Fatal("同输入键不稳定（H2 违背）")
	}

	if len(a) != 64 {
		t.Fatalf("键应为 sha256 十六进制（64 字符），得到 %d 字符：%q", len(a), a)
	}
	for _, r := range a {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("键不是小写十六进制：%q", a)
		}
	}

	if c := funcCacheKey(coreParse, sig, "internal", "irhash-2"); c == a {
		t.Fatal("IR 体变了键没变（缓存会喂旧产物）")
	}
	if d := funcCacheKey(MangleFunc("app/core", "", "parse2"), sig, "internal", "irhash-1"); d == a {
		t.Fatal("符号未进键")
	}
	if e := funcCacheKey(coreParse, sig, "export", "irhash-1"); e == a {
		t.Fatal("存储类未进键")
	}
	// ④ 依赖集合不进键：键只由（符号, 签名, 存储类, IR 体哈希, 编译器身份）决定。
	// 这里用"同参数双算"表达同一事实（若将来加入 deps 参数，本测试的签名会先红）。
	if f := funcCacheKey(coreParse, sig, "internal", "irhash-1"); f != a {
		t.Fatal("键引入了额外输入（依赖集合不该进键）")
	}
}
