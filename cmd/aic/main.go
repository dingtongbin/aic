// Command aic is the AIC compiler driver (核心设计 §十: cmd/aic CLI).
//
// 子命令: check / build / dump-c / fmt / test / version。
// 纪律: C 编译器一律 external process (红线 18 全仓禁 cgo); build 的
// 默认路径 = emit → 内部 C 编译器封装 → 运行产物; 门禁需要自掌四配置时
// 用 `--emit-c` 只取 C 文本 (黄金执行规划 §四.1 H3)。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"aic/internal/air"
	"aic/internal/cir"
	"aic/internal/emit"
	"aic/internal/lex"
	"aic/internal/parse"
	"aic/internal/pkg"
	"aic/internal/types"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[2:]
	var code int
	switch cmd := os.Args[1]; cmd {
	case "check":
		code = cmdCheck(args)
	case "build":
		reportGuards = hasFlag(args, "--report-guards")
		code = cmdBuild(args)
	case "dump-c":
		code = cmdDumpC(args)
	case "dump-ast":
		code = cmdDumpAST(args)
	case "dump-ir":
		code = cmdDumpIR(args)
	case "fmt":
		code = cmdFmt(args)
	case "test":
		code = cmdTest(args)
	case "version":
		fmt.Println("aic dev (v2 rebase, R3: emit + CLI)")
	default:
		if cmd == "-h" || cmd == "--help" || cmd == "help" {
			usage()
			return
		}
		fmt.Fprintf(os.Stderr, "aic: unknown subcommand %q\n\n", cmd)
		usage()
		code = 2
	}
	if code != 0 {
		os.Exit(code)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `aic - the AIC compiler driver (v2 rebase)

usage:
  aic check <file.aic>...          lex + parse + type check, three-part diagnostics (all errors at once)
  aic build <file.aic> [-o out]    build an executable (emit -> external C compiler)
  aic build --emit-c <f> -o out.c  emit C text only (used by the gates' own four configs)
  aic dump-c <file.aic>            print the emitted C text to stdout
  aic dump-ast <file.aic>          AST summary (for external generators; core design §10)
  aic dump-ir <file.aic> [--stage] print the typed core IR (air checkpoint; verifier-gated)
  aic fmt <file.aic>...            print the canonicalised source (currently: echo as-is)
  aic test [dir]                   discover and run *_test.aic
  aic version                      version information
`)
}

// --- 包装载 -------------------------------------------------------------------

// loaded 是一个已检查完的**单文件**单元（旧路径；多包走 loadProgram）。
type loaded struct {
	path  string
	file  *parse.File
	info  *types.Info
	guard map[parse.Node]types.GuardSpec
}

// loadUnit 解析 + 检查一个翻译单元（单文件粒度）。
//
// 包级装载（目录即包 + import 闭包 + 循环检测）在 internal/pkg，经 loadProgram
// 走整程序发射（红线 23）。这里保留单文件粒度给 dump-ast 之类只关心一个文件的
// 子命令用。
func loadUnit(path string) (*loaded, string) {
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("aic: cannot read %s: %v", path, err)
	}
	rel := filepath.ToSlash(path)
	f, lexErrs, parseErrs := parse.Source(rel, string(src))
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		return nil, strings.TrimRight(
			lex.FormatAll(rel, lexErrs)+"\n"+parse.FormatAll(rel, parseErrs), "\n")
	}
	res := types.CheckFile(f)
	if len(res.Errors) > 0 {
		return nil, strings.TrimRight(types.FormatAll(rel, res.Errors), "\n")
	}
	return &loaded{path: rel, file: f, info: res.Info, guard: res.Guards}, ""
}

// --- check --------------------------------------------------------------------

// cmdCheck 检查一个程序（含 import 闭包的全部包，拓扑序）。
// 单文件程序 = 单包程序，行为与旧版一致（多包才多出跨包解析）。
func cmdCheck(paths []string) int {
	if len(paths) == 0 {
		fmt.Fprintln(os.Stderr, "aic check: no input file")
		fmt.Fprintln(os.Stderr, "fix: pass at least one .aic file, e.g. aic check main.aic")
		return 2
	}
	code := 0
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			fmt.Fprintf(os.Stderr, "aic: cannot read %s: %v\n", path, err)
			fmt.Fprintln(os.Stderr, "fix: confirm the path exists and is readable")
			code = 2
			continue
		}
		if _, diag := loadProgram(path); diag != "" {
			fmt.Fprintln(os.Stderr, diag)
			code = 1
		}
	}
	return code
}

// --- build / dump-c -----------------------------------------------------------

func flagValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"="), true
		}
	}
	return "", false
}

func positional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			// 只有 -o 取值; --emit-c 是无值开关 (循环自身的 i++ 只推进一格,
			// 故取值型开关要再进一格)。
			if a == "-o" {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func cmdBuild(args []string) int {
	files := positional(args)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "aic build: no input file")
		fmt.Fprintln(os.Stderr, "fix: aic build main.aic [-o main.exe]")
		return 2
	}
	prog, diag := loadProgram(files[0])
	if diag != "" {
		fmt.Fprintln(os.Stderr, diag)
		return 1
	}
	// 只发射 C 文本（门禁自掌四配置）：**不过缓存**——H2/H3 判的就是发射文本的
	// 确定性与可编译性，走缓存会把门禁判据变成缓存命中（门禁是规划者的资产）。
	if flagValueIsEmitC(args) {
		// 门禁路径**永不走函数缓存**：H2/H3 判的就是发射文本的确定性与可编译性，
		// 走缓存等于把判据换成缓存命中（缓存是构建优化，不是正确性证明）。
		res, err := emitProgramCached(prog, args, false)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aic build: emit failed: %v\n", err)
			fmt.Fprintln(os.Stderr, "fix: this construct is not supported by the current code generator yet; rewrite it with the constructs listed in core design §16.0 (the IR path supports more than the C backend does today)")
			return 1
		}
		for _, w := range res.Warnings {
			fmt.Fprintf(os.Stderr, "aic build: note: %s\n", w)
		}
		if reportGuards {
			fmt.Printf("guards: kept=%d (store=%d, return=%d)\n",
				res.Guards.Kept, res.Guards.Store, res.Guards.Return)
		}
		if out, ok := flagValue(args, "-o"); ok {
			if err := os.WriteFile(out, []byte(res.C), 0o644); err != nil {
				fmt.Fprintf(os.Stderr, "aic build: cannot write %s: %v\n", out, err)
				return 2
			}
			return 0
		}
		fmt.Print(res.C)
		return 0
	}
	out := outName(args, files[0])
	if err := buildCached(prog, out, hasFlag(args, "--no-cache")); err != nil {
		fmt.Fprintf(os.Stderr, "aic build: %v\n", err)
		return 1
	}
	return 0
}

// reportGuards 由 `aic build --report-guards` 打开（§10.3 可验证性报告）。
var reportGuards bool

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

// buildCached 构建可执行产物：内容哈希缓存命中则直接落盘，否则发射 + 编译 + 入缓存。
// 缓存键 = 整程序内容哈希 + C 编译器身份 + 运行时源码哈希（任一变化即失效）。
func buildCached(prog *Program, out string, noCache bool) error {
	args := os.Args[2:]
	cc := findCC()
	if cc == "" {
		return fmt.Errorf("no C compiler found (set AIC_CC, or put gcc/clang on PATH)")
	}
	rt := repoRuntimeDir()
	key := ""
	if !noCache {
		key = pkg.Hash("build", prog.Unit.UnitHash(), cc, runtimeHash(rt))
		if data, ok := pkg.CacheGet(key); ok {
			return os.WriteFile(out, data, 0o755)
		}
	}
	res, err := emitProgramCached(prog, args, !noCache)
	if err != nil {
		return fmt.Errorf("emit failed: %v\nfix: this construct is not supported by the current code generator yet; rewrite it with the constructs listed in core design §16.0 (the IR path supports more than the C backend does today)", err)
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(os.Stderr, "aic build: note: %s\n", w)
	}
	if reportGuards {
		// §10.3 可验证性：total / eliminated / kept 分桶（eliminated 由检查器静态证明承担）。
		fmt.Printf("guards: kept=%d (store=%d, return=%d)\n",
			res.Guards.Kept, res.Guards.Store, res.Guards.Return)
	}
	if err := buildExecutable(res.C, out, needsL2(res.Needed)); err != nil {
		return err
	}
	if key != "" {
		if data, err := os.ReadFile(out); err == nil {
			_ = pkg.CachePut(key, data) // 缓存写失败不影响构建结果
		}
	}
	return nil
}

// runtimeHash 汇总 runtime/ 全部 .c/.h 的内容哈希（运行时改动必须让缓存失效）。
func runtimeHash(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "no-runtime"
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".c") || strings.HasSuffix(e.Name(), ".h") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names)*2)
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			continue
		}
		parts = append(parts, n, pkg.Hash(string(b)))
	}
	return pkg.Hash(parts...)
}

func flagValueIsEmitC(args []string) bool {
	for _, a := range args {
		if a == "--emit-c" {
			return true
		}
	}
	return false
}

func outName(args []string, src string) string {
	if out, ok := flagValue(args, "-o"); ok {
		return out
	}
	base := strings.TrimSuffix(filepath.Base(src), ".aic")
	if isWindows() {
		return base + ".exe"
	}
	return base
}

func cmdDumpC(args []string) int {
	files := positional(args)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "aic dump-c: no input file")
		return 2
	}
	prog, diag := loadProgram(files[0])
	if diag != "" {
		fmt.Fprintln(os.Stderr, diag)
		return 1
	}
	res, err := emitProgram(prog, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aic dump-c: emit failed: %v\n", err)
		return 1
	}
	fmt.Print(res.C)
	return 0
}

func cmdDumpAST(args []string) int {
	files := positional(args)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "aic dump-ast: no input file")
		return 2
	}
	src, err := os.ReadFile(files[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "aic: cannot read %s: %v\n", files[0], err)
		return 2
	}
	f, lexErrs, parseErrs := parse.Source(files[0], string(src))
	if len(lexErrs) > 0 || len(parseErrs) > 0 {
		fmt.Fprint(os.Stderr, lex.FormatAll(files[0], lexErrs))
		fmt.Fprint(os.Stderr, parse.FormatAll(files[0], parseErrs))
		return 1
	}
	for _, d := range f.Decls {
		fmt.Printf("%T\n", d)
	}
	return 0
}

func cmdFmt(args []string) int {
	files := positional(args)
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "aic fmt: no input file")
		return 2
	}
	for _, p := range files {
		src, err := os.ReadFile(p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aic: cannot read %s: %v\n", p, err)
			return 2
		}
		// R3: 规范化打印待补（需 parse 侧保留注释/空行信息）；当前原样回显。
		os.Stdout.Write(src)
	}
	return 0
}

// cmdTest 运行 *_test.aic（核心设计 §十一：func testXxx 约定，testing 失败即 trap）。
//
// 每个测试文件 = 一个测试程序：包内普通源文件 + 该测试文件 + 合成 main
// （按声明序调用 testXxx）。全部通过 = 退出码 0；任一失败 = 打印 trap 消息并返回 1。
func cmdTest(args []string) int {
	dir := "."
	if len(positional(args)) > 0 {
		dir = positional(args)[0]
	}
	var files []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if strings.HasSuffix(p, "_test.aic") {
			files = append(files, filepath.ToSlash(p))
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "aic test: %v\n", err)
		return 2
	}
	sort.Strings(files) // 运行顺序确定性（H2/H3 精神：禁目录序）
	if len(files) == 0 {
		fmt.Println("aic test: no *_test.aic found")
		return 0
	}
	passed, failed := 0, 0
	for _, path := range files {
		tu, err := pkg.LoadTest(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "aic test: %s failed to load:\n%s\n", path, err.Error())
			failed++
			continue
		}
		prog := &Program{Unit: tu.Unit, Emit: &emit.Unit{Entry: tu.Unit.Entry.Name}}
		deps := map[string]*types.Info{}
		bad := false
		for _, p := range tu.Unit.Order {
			res := types.CheckPackage(fileASTs(p.Files), deps)
			if len(res.Errors) > 0 {
				fmt.Fprintf(os.Stderr, "%s\n", strings.TrimRight(types.FormatAll(p.Name, res.Errors), "\n"))
				bad = true
				continue
			}
			deps[p.Name] = res.Info
			for _, f := range p.Files {
				prog.Emit.Files = append(prog.Emit.Files,
					&emit.FileUnit{Path: f.Path, File: f.AST, Info: res.Info})
			}
		}
		if bad {
			failed++
			continue
		}
		exe := filepath.Join(os.TempDir(), "aic-test-"+sanitizeName(path))
		if err := buildCached(prog, exe, false); err != nil {
			fmt.Fprintf(os.Stderr, "aic test: %s failed to build: %v\n", path, err)
			failed++
			continue
		}
		out, err := runProgram(exe)
		if out != "" {
			fmt.Print(out)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "aic test: FAIL %s\n%s\n", path, err.Error())
			failed++
			continue
		}
		fmt.Printf("aic test: ok   %s (%d cases)\n", path, len(tu.Tests))
		passed++
	}
	fmt.Printf("aic test: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// sanitizeName 把路径压成可用作文件名的一段（测试产物名确定性）。
func sanitizeName(path string) string {
	var b strings.Builder
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

// needsL2 报告产物是否用到调度层（红线 16：按需链接 aic_l2.c）。
func needsL2(needed []string) bool {
	for _, n := range needed {
		if n == "l2" {
			return true
		}
	}
	return false
}

// emitProgram 是**唯一**的 C 后端入口：自有 IR 下降（tast → air → mair → eair → cir → C）。
//
// 用户直令（2026-10）：**直译路径去掉，不能再用了** —— 不存在"模式选择"，也不存在回退。
// 未覆盖的 IR 形态一律**编译失败**（红线 23：绝不发半截），失败清单就是施工清单。
func emitProgram(prog *Program, args []string) (*emit.Result, error) {
	return emitProgramCached(prog, args, false)
}

// emitProgramCached 与 emitProgram 同管线，区别是可按 open 开函数级增量翻译
// 缓存（F1：翻译段 47ms → 命中段 ~0ms；键含编译器身份与 IR 体哈希）。
// open=false（门禁 --emit-c / dump-c / --no-cache）时行为与裸管线完全一致。
func emitProgramCached(prog *Program, args []string, open bool) (*emit.Result, error) {
	if !open {
		return emitIRUnit(prog)
	}
	emit.SetFuncCache(emit.NewFuncCache(pkg.CacheDir(), pkg.CompilerID()+"/funccache-v1", false))
	defer emit.SetFuncCache(nil)
	return emitIRUnit(prog)
}

// emitIRUnit 走 IR 路径：降级 → 单态化 → C 形态 → 由 emit 的 IR 后端发 C。
// 包分组取自 prog.Emit.Files（每个文件单元带检查器 Info，包名 = Info.Pkg）；
// 函数体按 IR 符号登记，后端再从签名侧把两者对上（对不上 = 硬错，不静默丢）。
func emitIRUnit(prog *Program) (*emit.Result, error) {
	byPkg := map[string][]*emit.FileUnit{}
	var order []string
	for _, fu := range prog.Emit.Files {
		if fu.Info == nil {
			continue
		}
		if _, ok := byPkg[fu.Info.Pkg]; !ok {
			order = append(order, fu.Info.Pkg)
		}
		byPkg[fu.Info.Pkg] = append(byPkg[fu.Info.Pkg], fu)
	}
	ir := emit.NewIRProg()
	for _, name := range order {
		fus := byPkg[name]
		files := make([]*parse.File, 0, len(fus))
		imports := []string{}
		var info *types.Info
		for _, fu := range fus {
			files = append(files, fu.File)
			info = fu.Info
		}
		for im := range info.Imported {
			imports = append(imports, im)
		}
		m, err := air.Lower(air.LowerInput{Pkg: name, Files: files, Info: info, Imports: imports, Mono: true})
		if err != nil {
			return nil, err
		}
		if vs := air.Verify(m); len(vs) > 0 {
			return nil, fmt.Errorf("IR verification failed: %s", vs[0].String())
		}
		if vs := air.VerifyMono(m, info); len(vs) > 0 {
			return nil, fmt.Errorf("mair verification failed: %s", vs[0])
		}
		cp, err := cir.Build(m)
		if err != nil {
			return nil, err
		}
		for _, f := range cp.Funcs {
			if err := ir.Add(f.Sym, f); err != nil {
				return nil, err
			}
		}
	}
	return emit.EmitIRUnit(prog.Emit, ir)
}
