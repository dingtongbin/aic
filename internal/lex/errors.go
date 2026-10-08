package lex

import "strings"

// LexerError is one collected scanner failure. Format renders the frozen
// three-part diagnostic required by H5 (核心设计 §六):
//
//	<file>:<line>:<col>: error:<描述>
//	    <上下文：实际文本/实际值>
//	    fix: <可执行的修复动作>
type LexerError struct {
	Pos     Pos    // where the failure starts (1-based)
	Desc    string // 描述 — never repeats the path
	Context string // 上下文 — what was actually there
	Fix     string // 修复 — an actionable instruction, never empty
}

func (e LexerError) Error() string {
	return e.Pos.String() + ": error: " + e.Desc
}

// Format renders the three-part shape. No diagnostic may be emitted without
// all three parts (执行规划 §八 防坑清单 #2).
func (e LexerError) Format() string {
	var b strings.Builder
	b.WriteString(e.Pos.String())
	b.WriteString(": error: ")
	b.WriteString(e.Desc)
	b.WriteString("\n    ")
	b.WriteString(e.Context)
	b.WriteString("\n    fix: ")
	b.WriteString(e.Fix)
	return b.String()
}

// FormatAll renders every collected error in scan order, one block each.
// 红线#16: a single scan reports all errors, not just the first.
func FormatAll(path string, errs []LexerError) string {
	var blocks []string
	for _, e := range errs {
		if e.Pos.File == "" {
			e.Pos.File = path
		}
		blocks = append(blocks, e.Format())
	}
	return strings.Join(blocks, "\n")
}
