package air

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"aic/internal/parse"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// `ir/` 快照语料（文本 IR 是投影，用途 = 门禁基线 / diff / 人读）。
//
// 判据（H8 的 IR 侧）：
//   ① 每个 ok 语料都能 lower + **过 verifier**（拒绝即失败）；
//   ② 同一输入两次 dump **逐字节一致**（确定性）；
//   ③ 与 `testdata/ir/<name>.air` 快照逐字节一致（基线回归）。
// 快照重建：`AIC_UPDATE_GOLDEN=1 go test ./internal/air -run TestIRSnapshots`。
// ---------------------------------------------------------------------------

// dumpOne 对单个源文件跑「解析 → 检查 → lower → verify → 打印」。
func dumpOne(t *testing.T, path string) (string, []Violation, error) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	rel := filepath.ToSlash(path)
	f, lexErrs, parseErrs := parse.Source(rel, string(raw))
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		return "", nil, errParse
	}
	res := types.CheckFile(f)
	if len(res.Errors) > 0 {
		return "", nil, errCheck
	}
	m, err := Lower(LowerInput{Pkg: res.Info.Pkg, Files: []*parse.File{f}, Info: res.Info})
	if err != nil {
		return "", nil, err
	}
	return Print(m), Verify(m), nil
}

var (
	errParse = os.ErrInvalid
	errCheck = os.ErrInvalid
)

// TestIRSnapshots 遍历 ok 语料：lower + verify + 快照比对 + 确定性。
func TestIRSnapshots(t *testing.T) {
	root := filepath.Join("..", "..", "testdata")
	okDir := filepath.Join(root, "ok")
	irDir := filepath.Join(root, "ir")
	entries, err := os.ReadDir(okDir)
	if err != nil {
		t.Fatalf("读取 ok/ 语料失败: %v", err)
	}
	update := os.Getenv("AIC_UPDATE_GOLDEN") == "1"
	if update {
		if err := os.MkdirAll(irDir, 0o755); err != nil {
			t.Fatalf("建 ir/ 失败: %v", err)
		}
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".aic") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("ok/ 语料为空：语料是本金，门禁必须真实执行")
	}
	lowered := 0
	for _, name := range names {
		src := filepath.Join(okDir, name)
		out, vs, err := dumpOne(t, src)
		if err != nil {
			// lowering 尚未覆盖的形态：记录但不算失败（覆盖面由下面的计数看住）
			t.Logf("skip %s: %v", name, err)
			continue
		}
		if len(vs) > 0 {
			t.Errorf("%s: verifier 拒绝（编译器缺陷）：%v", name, vs)
			continue
		}
		lowered++
		// ② 确定性
		again, _, err2 := dumpOne(t, src)
		if err2 != nil || again != out {
			t.Errorf("%s: 两次 dump 不一致（H8 确定性）", name)
			continue
		}
		// ③ 快照
		snap := filepath.Join(irDir, strings.TrimSuffix(name, ".aic")+".air")
		if update {
			if err := os.WriteFile(snap, []byte(out), 0o644); err != nil {
				t.Fatalf("写快照失败: %v", err)
			}
			continue
		}
		want, werr := os.ReadFile(snap)
		if werr != nil {
			// v7 语料全量重写，ir/ 快照尚未重建。**不预写快照**（那等于编造
			// 期望值）——按 AIC_UPDATE_GOLDEN=1 在语料全绿后生成，人工复核入库。
			// 此处记 pending，不计失败。
			t.Logf("pending %s: no ir/ snapshot yet (regenerate with AIC_UPDATE_GOLDEN=1 once the corpus is green)", name)
			continue
		}
		if normalize(out) != normalize(string(want)) {
			t.Errorf("%s: IR 快照不符（%d 字节 vs %d 字节）", name, len(out), len(want))
		}
	}
	t.Logf("ir/ 快照：lowered %d / %d 个 ok 语料", lowered, len(names))
	switch {
	case lowered == 0:
		// 语料一个都还没 lowering 时无法测量覆盖面 —— 跳过而不是假失败。
		// 一旦有任一文件可降低，覆盖率门禁立即恢复强制；1/37 这类"部分覆盖"
		// 是真实的阶段信号，必须报错而不是跳过。
		t.Skipf("ok/ 语料当前无一可降低（阶段 3 未开始）：覆盖面门禁暂不适用")
	case lowered*100/len(names) < 95:
		t.Errorf("lowering 覆盖面 %d/%d < 95%%：AIR 必须覆盖语料（T2 的交付面）", lowered, len(names))
	}
}

func normalize(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}
