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
// `ir/` 快照语料（核心设计 §10.1：文本 IR 是投影，用途 = 门禁基线 / diff / 人读）。
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
			t.Errorf("%s: 缺 ir/ 快照（AIC_UPDATE_GOLDEN=1 重建）", name)
			continue
		}
		if normalize(out) != normalize(string(want)) {
			t.Errorf("%s: IR 快照不符（%d 字节 vs %d 字节）", name, len(out), len(want))
		}
	}
	t.Logf("ir/ 快照：lowered %d / %d 个 ok 语料", lowered, len(names))
	if lowered*100/len(names) < 95 {
		t.Errorf("lowering 覆盖面 %d/%d < 95%%：AIR 必须覆盖语料（T2 的交付面）", lowered, len(names))
	}
}

func normalize(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}
