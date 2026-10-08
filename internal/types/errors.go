package types

import (
	"sort"
	"strings"
)

// CheckerError is one collected type-check failure in the frozen three-part
// shape (H5 / 核心设计 §三 报错三段式) — identical to the parse layer's shape.
type CheckerError struct {
	Pos     parsePos
	Desc    string
	Context string
	Fix     string
}

// parsePos mirrors parse.Pos without importing the package (checker consumes
// parse.AST; the position type is copied to keep the layer edge one-way at
// the value level too).
type parsePos struct {
	File string
	Line int
	Col  int
}

func (e CheckerError) Format() string {
	var b strings.Builder
	b.WriteString(e.Pos.File)
	b.WriteString(":")
	b.WriteString(itoa(e.Pos.Line))
	b.WriteString(":")
	b.WriteString(itoa(e.Pos.Col))
	b.WriteString(": error: ")
	b.WriteString(e.Desc)
	b.WriteString("\n    ")
	b.WriteString(e.Context)
	b.WriteString("\n    fix: ")
	b.WriteString(e.Fix)
	return b.String()
}

// FormatAll renders every diagnostic in source order (多错一次报, 红线 14).
//
// 两条纪律（诊断审计驱动）：
//   - **按源码位置排序**（行、列稳定序）：此前按检查顺序输出，于是 `a, b = f()` 里
//     右侧的错误会排到左侧之前 —— 与"按源码序"的承诺相反，人读起来是乱的；
//   - **逐字重复的诊断只留一条**：检查器有若干"同一节点被检查两次"的路径
//     （赋值右值 + 存储点标记），会把**完全相同**的一条诊断打印两遍。
//     去重只针对 (位置, 描述, 上下文, 修复) 全同的项，不同诊断一律保留。
func FormatAll(path string, errs []CheckerError) string {
	sorted := make([]CheckerError, 0, len(errs))
	seen := map[string]bool{}
	for _, e := range errs {
		if e.Pos.File == "" {
			e.Pos.File = path
		}
		key := e.Pos.File + ":" + itoa(e.Pos.Line) + ":" + itoa(e.Pos.Col) + "|" + e.Desc + "|" + e.Context + "|" + e.Fix
		if seen[key] {
			continue
		}
		seen[key] = true
		sorted = append(sorted, e)
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Pos.Line != sorted[j].Pos.Line {
			return sorted[i].Pos.Line < sorted[j].Pos.Line
		}
		return sorted[i].Pos.Col < sorted[j].Pos.Col
	})
	var blocks []string
	for _, e := range sorted {
		blocks = append(blocks, e.Format())
	}
	return strings.Join(blocks, "\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
