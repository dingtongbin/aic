package main

import (
	"fmt"
	"os"
	"strings"

	"aic/internal/air"
	"aic/internal/cir"
	"aic/internal/emit"
	"aic/internal/escape"
	"aic/internal/intrange"
	"aic/internal/parse"
	"aic/internal/types"
)

// dump-ir：把整程序降级为 AIR 并打印（核心设计 §十：文本 IR 是投影）。
//
// 纪律：**发射前必过 verifier**，verifier 拒绝 = 编译失败（§10.5，无 --no-verify）。
// 当前只支持 `--stage air`（去糖后）；mair/eair/cir 随 T2 后续批次落地。
func cmdDumpIR(args []string) int {
	files := positional(args)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "aic dump-ir: no input file")
		return 2
	}
	stage := "air"
	if v, ok := flagValue(args, "--stage"); ok {
		stage = v
	}
	if stage != "tast" && stage != "air" && stage != "mair" && stage != "eair" && stage != "cir" {
		fmt.Fprintf(os.Stderr, "aic dump-ir: stage %q is not available yet (checkpoints: tast -> air -> mair -> eair -> cir)\n", stage)
		return 2
	}
	prog, diag := loadProgram(files[0])
	if diag != "" {
		fmt.Fprintln(os.Stderr, diag)
		return 1
	}
	// tast：只到**类型检查后**为止（投影检查器的输出面，不做任何降级）。
	// 判据 = types.VerifyTyped 的结构自洽（每个字段/形参/返回位都有已解析类型）。
	if stage == "tast" {
		deps := map[string]*types.Info{}
		var b strings.Builder
		for _, p := range prog.Unit.Order {
			asts := fileASTs(p.Files)
			res := types.CheckPackage(asts, deps)
			if len(res.Errors) > 0 {
				fmt.Fprintln(os.Stderr, strings.TrimRight(types.FormatAll(p.Name, res.Errors), "\n"))
				return 1
			}
			if vs := types.VerifyTyped(p.Name, asts, res.Info); len(vs) > 0 {
				fmt.Fprintf(os.Stderr, "aic dump-ir: tast verification failed (compiler defect):\n")
				for _, v := range vs {
					fmt.Fprintf(os.Stderr, "  %s\n", v)
				}
				return 1
			}
			deps[p.Name] = res.Info
			b.WriteString(types.DumpTyped(p.Name, asts, res.Info))
		}
		fmt.Print(b.String())
		return 0
	}
	var mods []*air.Module
	deps := map[string]*types.Info{}
	for _, p := range prog.Unit.Order {
		res := types.CheckPackage(fileASTs(p.Files), deps)
		if len(res.Errors) > 0 {
			fmt.Fprintln(os.Stderr, strings.TrimRight(types.FormatAll(p.Name, res.Errors), "\n"))
			return 1
		}
		deps[p.Name] = res.Info
		files := make([]*parse.File, 0, len(p.Files))
		for _, f := range p.Files {
			files = append(files, f.AST)
		}
		m, err := air.Lower(air.LowerInput{Pkg: p.Name, Files: files, Info: res.Info, Imports: p.Imports, Mono: stage == "mair" || stage == "cir"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "aic dump-ir: lowering failed: %v\n", err)
			return 1
		}
		// mair = **单态化后**：泛型函数/方法按实例各降一份体、泛型类按实例发声明，
		// 不留类型形参（V1.2）。降级时已带实例上下文，故这里只做校验。
		var mono []string
		if stage == "mair" || stage == "cir" {
			mono = air.VerifyMono(m, res.Info)
		}
		// eair = 分析决策后：先 prove 后 elim（E1–E4），消去只消费事实（§10.3）。
		if stage == "eair" {
			rep := escape.Analyze(m)
			rng := intrange.Prove(m)
			if hasFlag(args, "--report-guards") {
				fmt.Println(rep.Summary())
				for _, e := range rep.Elims {
					fmt.Printf("  eliminated %s %s: %s (%s)\n", e.Fn, e.Block, e.Rule, e.Reason)
				}
				fmt.Println(rng.Summary())
				for _, a := range rng.Accesses {
					mark := "checked"
					if a.InBound {
						mark = "unchecked"
					}
					fmt.Printf("  %s %s %s[%s]: %s\n", mark, a.Fn, a.Place, a.Index, a.Reason)
				}
			}
		}
		// verifier 是门禁：拒绝即编译失败（内部缺陷，不是用户代码错误）。
		if vs := air.Verify(m); len(vs) > 0 {
			fmt.Fprintf(os.Stderr, "aic dump-ir: IR verification failed (compiler defect):\n")
			for _, v := range vs {
				fmt.Fprintf(os.Stderr, "  %s\n", v.String())
			}
			return 1
		}
		if len(mono) > 0 {
			fmt.Fprintf(os.Stderr, "aic dump-ir: mair verification failed (compiler defect):\n")
			for _, v := range mono {
				fmt.Fprintf(os.Stderr, "  %s\n", v)
			}
			return 1
		}
		mods = append(mods, m)
	}
	// cir = C 形态：把 CFG 折成结构化语句（if/for/switch + 显式 region/守卫）。
	if stage == "cir" {
		var b strings.Builder
		for _, m := range mods {
			prog, err := cir.Build(m)
			if err != nil {
				fmt.Fprintf(os.Stderr, "aic dump-ir: CIR structuring failed (compiler defect): %v\n", err)
				return 1
			}
			b.WriteString(prog.Print())
		}
		fmt.Print(b.String())
		return 0
	}
	fmt.Print(air.PrintAll(mods))
	return 0
}

// 保留 emit 引用（dump-ir 与 emit 共用程序装载路径；后续 AIR→C 会替换 emit 入口）。
var _ = emit.EmitUnit
