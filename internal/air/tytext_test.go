package air

import (
	"testing"

	"aic/internal/types"
)

// TestTyTextUsesPackageIdentity 钉住「类型文本键跨模块唯一」：
// IR 的类型文本是**类型键**（emit 的 irTab、缓存的 param 文本都用它）。
// 若它只带包名，两个模块各自的 `util.Buf` 会得到同一个键、互相覆盖 ——
// 在超大工程里表现为"某个包的类型定义随机被另一个包顶掉"，且只在特定
// 构建顺序下复现。这里断言：路径不同 ⇒ 键不同。
func TestTyTextUsesPackageIdentity(t *testing.T) {
	coreBuf := &types.Class{Name: "Buf", Pkg: "util", PkgPath: "app/core/util"}
	uiBuf := &types.Class{Name: "Buf", Pkg: "util", PkgPath: "app/ui/util"}

	if TyText(coreBuf) == TyText(uiBuf) {
		t.Fatalf("同名类型在跨模块同名包里撞了类型文本键：%q", TyText(coreBuf))
	}
	if TyText(coreBuf) != "app_core_util_Buf*" {
		t.Fatalf("类型文本形态不符（路径应折成下划线）：%q", TyText(coreBuf))
	}

	// 无 PkgPath（std 类型 / 测试构造）时退回包名 —— 不能变成空前缀。
	bare := &types.Class{Name: "Buf", Pkg: "util"}
	if TyText(bare) != "util_Buf*" {
		t.Fatalf("缺 PkgPath 时应退回包名，得到 %q", TyText(bare))
	}

	// 枚举与接口同规。
	coreE := &types.Enum{Name: "E", Pkg: "util", PkgPath: "app/core/util"}
	uiE := &types.Enum{Name: "E", Pkg: "util", PkgPath: "app/ui/util"}
	if TyText(coreE) == TyText(uiE) {
		t.Fatalf("枚举撞键：%q", TyText(coreE))
	}
	coreI := &types.Interface{Name: "I", Pkg: "util", PkgPath: "app/core/util"}
	uiI := &types.Interface{Name: "I", Pkg: "util", PkgPath: "app/ui/util"}
	if TyText(coreI) == TyText(uiI) {
		t.Fatalf("接口撞键：%q", TyText(coreI))
	}
}
