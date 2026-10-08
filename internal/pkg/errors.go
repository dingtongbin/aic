package pkg

import (
	"fmt"
	"strings"

	"aic/internal/lex"
	"aic/internal/parse"
)

// Diag 是包管理器的三段式诊断（与 lex/parse/types 同形：位置+描述 / 上下文 / 修复）。
// 包图错误（找不到包、循环 import、import 名与目录名不一致）都是编译错，且都要
// **回指到那行 import**（位置段 = file:line:col，缺了就不是三段式）。
type Diag struct {
	// Pos 是相关位置（一般是引发该错误的 import 声明）。
	Pos parse.Pos
	// Desc 是错误描述（第一段冒号后的部分）。
	Desc string
	// Context 是证据行（缩进四空格）。
	Context string
	// Fix 是修复指引（第三段，不带「修复：」前缀）。
	Fix string
}

func (d *Diag) Error() string {
	pos := d.Pos
	if pos.File == "" {
		pos.File = "<package graph>"
	}
	if pos.Line <= 0 {
		pos.Line = 1
	}
	if pos.Col <= 0 {
		pos.Col = 1
	}
	return fmt.Sprintf("%s:%d:%d: error: %s\n    %s\n    fix: %s",
		pos.File, pos.Line, pos.Col, d.Desc, d.Context, d.Fix)
}

// diag 构造三段式诊断（位置缺省为 1:1，保证形状不缺段）。
func diag(pos parse.Pos, desc, context, fix string) *Diag {
	return &Diag{Pos: pos, Desc: desc, Context: context, Fix: fix}
}

// SyntaxError 是包内某个源文件的词法/语法错误（三段式诊断，红线 14：一次报全）。
// 收集阶段遇语法错即停：整程序收集不完整时禁止发射（红线 23）。
type SyntaxError struct {
	Path  string
	Lex   []lex.LexerError
	Parse []parse.ParseError
}

func (e *SyntaxError) Error() string {
	var b strings.Builder
	b.WriteString(lex.FormatAll(e.Path, e.Lex))
	if len(e.Lex) > 0 && len(e.Parse) > 0 {
		b.WriteString("\n")
	}
	b.WriteString(parse.FormatAll(e.Path, e.Parse))
	return strings.TrimRight(b.String(), "\n")
}
