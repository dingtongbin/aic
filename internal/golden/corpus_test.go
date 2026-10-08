// Package golden is the test-only corpus harness.
//
// R1 stage (黄金执行规划 §四.3 SKIP 白名单)：本阶段只承载 lex+parse 层门禁 —
//   - ok/ 语料必须词法+语法干净（H1–H4 属 emit/CLI 门，按白名单合法未亮）；
//   - err/ 语料中解析层产生的诊断必须过三段式（H5 解析层）并与 .err 快照
//     逐字节一致；检查器层语料（R2 回收）此时不产生解析诊断，记录数量。
//
// R2/R3 重建完整管线后，本文件按对应 R 档扩回运行门禁。
package golden

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aic/internal/lex"
	"aic/internal/parse"
	"aic/internal/types"
)

// renderAll merges all layer diagnostics in the canonical single-\n block
// format produced by each package's FormatAll — 快照只有一种格式。
// err/ 语料在语法干净时进检查器（R2 起全管线视角，与 aic check 一致）。
func renderAll(path string, src string, lexErrs []lex.LexerError, parseErrs []parse.ParseError) string {
	var parts []string
	if s := lex.FormatAll(path, lexErrs); s != "" {
		parts = append(parts, s)
	}
	if s := parse.FormatAll(path, parseErrs); s != "" {
		parts = append(parts, s)
	}
	if len(lexErrs) == 0 && len(parseErrs) == 0 {
		f, _, perr := parse.Source(path, src)
		if len(perr) == 0 {
			if s := types.FormatAll(path, types.CheckFile(f).Errors); s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("cwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "..", ".."))
}

// relPath renders a corpus path relative to the repo root — 诊断与快照只允许
// 相对路径（H2/H3 精神：产物不携带机器相关内容）。
func relPath(t *testing.T, abs string) string {
	t.Helper()
	root := repoRoot(t)
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		t.Fatalf("相对化 %s: %v", abs, err)
	}
	return filepath.ToSlash(rel)
}

func corpusDir(t *testing.T, name string) []string {
	t.Helper()
	root := filepath.Join(repoRoot(t), "testdata", name)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("读取 %s/ 语料失败: %v", name, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".aic") {
			out = append(out, filepath.Join(root, e.Name()))
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s/ 语料为空：语料是本金，门禁必须真实执行", name)
	}
	return out
}

// assertThreePart enforces H5: the rendered output is a sequence of 3-line
// blocks (header / context / fix).
func assertThreePart(t *testing.T, out string) {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines)%3 != 0 {
		t.Errorf("三段式块数应为 3 的倍数，实际 %d 行：\n%s", len(lines), out)
		return
	}
	for i := 0; i+2 < len(lines); i += 3 {
		header, ctx, fix := lines[i], lines[i+1], lines[i+2]
		if !strings.Contains(header, ": error: ") {
			t.Errorf("第一段格式不符：\n%s", header)
		}
		if !strings.HasPrefix(ctx, "    ") {
			t.Errorf("第二段（上下文）须缩进：\n%s", ctx)
		}
		if !strings.HasPrefix(fix, "    fix: ") || strings.TrimSpace(strings.TrimPrefix(fix, "    fix: ")) == "" {
			t.Errorf("第三段（修复）缺失或为空：\n%s", fix)
		}
	}
}

// TestOkCorpusParses: every ok/ program must lex and parse clean.
func TestOkCorpusParses(t *testing.T) {
	for _, src := range corpusDir(t, "ok") {
		src := src
		t.Run(filepath.Base(src), func(t *testing.T) {
			raw, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			rel := filepath.ToSlash(src)
			_, lexErrs, parseErrs := parse.Source(rel, string(raw))
			if len(lexErrs) > 0 || len(parseErrs) > 0 {
				for _, e := range lexErrs {
					t.Errorf("%s", e.Format())
				}
				for _, e := range parseErrs {
					t.Errorf("%s", e.Format())
				}
			}
		})
	}
}

// TestErrCorpusParseLayer: err/ diagnostics produced at lex+parse time must be
// three-part (H5) and match the .err snapshot byte for byte. Files that parse
// clean belong to the checker layer (R2 回收) — they must still carry an .err
// snapshot so the layer hand-off is explicit.
func TestErrCorpusParseLayer(t *testing.T) {
	deferred := 0
	checked := 0
	for _, src := range corpusDir(t, "err") {
		src := src
		t.Run(filepath.Base(src), func(t *testing.T) {
			raw, err := os.ReadFile(src)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			rel := relPath(t, src)
			_, lexErrs, parseErrs := parse.Source(rel, string(raw))
			blocks := renderAll(rel, string(raw), lexErrs, parseErrs)
			if blocks == "" {
				deferred++
				t.Errorf("err/ 语料全管线通过（既非语法错也非类型错）——语料前提失效，需退役或改写")
				return
			}
			checked++
			got := blocks + "\n"
			want, werr := os.ReadFile(strings.TrimSuffix(src, ".aic") + ".err")
			if werr != nil {
				t.Errorf("缺少 .err 快照（红线 15：报错必须进快照）\n实际：\n%s", got)
				return
			}
			assertThreePart(t, blocks)
			if normalizeNL(got) != normalizeNL(string(want)) {
				t.Errorf(".err 快照不符\n--- 实际 ---\n%s\n--- 期望 ---\n%s", got, string(want))
			}
		})
	}
	t.Logf("err/ 语料：解析层比对 %d 个，检查器层待 R2 %d 个", checked, deferred)
}

// TestUpdateErrGoldens 显式重建 err/ 快照（仅解析层会产生的诊断）：
//
//	PowerShell: $env:AIC_UPDATE_GOLDEN=1; go test ./internal/golden -run TestUpdateErrGoldens
func TestUpdateErrGoldens(t *testing.T) {
	if os.Getenv("AIC_UPDATE_GOLDEN") != "1" {
		t.Skip("快照重建需显式设置 AIC_UPDATE_GOLDEN=1")
	}
	updated := 0
	for _, src := range corpusDir(t, "err") {
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatalf("读取失败: %v", err)
		}
		rel := relPath(t, src)
		_, lexErrs, parseErrs := parse.Source(rel, string(raw))
		got := renderAll(rel, string(raw), lexErrs, parseErrs)
		if got == "" {
			t.Logf("警告: %s 全管线通过", rel)
			continue
		}
		if werr := os.WriteFile(strings.TrimSuffix(src, ".aic")+".err", []byte(got+"\n"), 0o644); werr != nil {
			t.Fatalf("写入 %s: %v", src, werr)
		}
		updated++
	}
	t.Logf("重建 %d 个 .err 快照", updated)
}

// TestRetiredSyntaxStaysRejected anchors 防坑清单 1: v1-only syntax must keep
// failing with the targeted retirement diagnostic, never parse silently.
func TestRetiredSyntaxStaysRejected(t *testing.T) {
	for _, src := range []string{
		"while", "elseif", "implements", "handle", "await", "del", "mut",
	} {
		raw := "func f() {\n    " + src + " x\n}\n"
		_, lexErrs, parseErrs := parse.Source("t.aic", raw)
		found := false
		for _, e := range parseErrs {
			if strings.Contains(e.Desc, "is retired") {
				found = true
			}
		}
		if !found && len(lexErrs) == 0 {
			t.Errorf("退役词 %s 未被定向拒绝", src)
		}
	}
}

func normalizeNL(s string) string {
	return strings.TrimRight(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}
