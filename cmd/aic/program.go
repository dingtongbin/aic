package main

import (
	"fmt"
	"strings"

	"aic/internal/emit"
	"aic/internal/parse"
	"aic/internal/pkg"
	"aic/internal/types"
)

// ---------------------------------------------------------------------------
// 程序装载（核心设计 §十 阶段 1 收集）：入口文件 → import 闭包 → 逐包类型检查
// → 整程序发射单元。红线 23：收集不完整（缺包/循环/语法错/类型错）时禁止发射。
// ---------------------------------------------------------------------------

// Program 是一个已收集并检查完的程序。
type Program struct {
	Unit *pkg.Unit
	Emit *emit.Unit
}

// loadProgram 从入口文件收集整程序并逐包检查（拓扑序：依赖先检查）。
// 返回的第二个值是非空诊断（三段式，红线 14：一次报全）。
func loadProgram(entry string) (*Program, string) {
	u, err := pkg.Load(entry)
	if err != nil {
		return nil, err.Error()
	}
	deps := map[string]*types.Info{}
	prog := &Program{Unit: u, Emit: &emit.Unit{Entry: u.Entry.Name}}
	var diags []string
	for _, p := range u.Order {
		res := types.CheckPackage(fileASTs(p.Files), deps)
		if len(res.Errors) > 0 {
			diags = append(diags, strings.TrimRight(types.FormatAll(p.Name, res.Errors), "\n"))
			continue // 该包检查失败：不发它的 C，也不给下游当依赖（避免级联噪音）
		}
		deps[p.Name] = res.Info
		for _, f := range p.Files {
			prog.Emit.Files = append(prog.Emit.Files,
				&emit.FileUnit{Path: f.Path, File: f.AST, Info: res.Info})
		}
	}
	if len(diags) > 0 {
		return nil, strings.Join(diags, "\n")
	}
	return prog, ""
}

// fileASTs 取包内文件的 AST（发射单元与检查器的共同输入）。
func fileASTs(files []*pkg.File) []*parse.File {
	out := make([]*parse.File, 0, len(files))
	for _, f := range files {
		out = append(out, f.AST)
	}
	return out
}

// summarize 返回一行程序结构摘要（诊断/自检用）。
func (p *Program) summarize() string {
	return fmt.Sprintf("%d packages: %s", len(p.Unit.Order), p.Unit.PkgList())
}
