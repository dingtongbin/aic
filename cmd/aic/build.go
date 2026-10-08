package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ---------------------------------------------------------------------------
// 外部 C 编译器封装（核心设计 §十一：开发路径 tcc，生产路径 gcc/clang -O2）。
// 全仓禁 cgo（红线 18）：这里只有外部进程，没有任何 C 链接进 Go 二进制。
// 运行时源码按需链接：无 spawn 的程序根本不拉 L2（红线 16）。
// ---------------------------------------------------------------------------

func isWindows() bool { return runtime.GOOS == "windows" }

// repoRuntimeDir 定位 runtime/（随源码包分发，与 aic 可执行文件无关）。
func repoRuntimeDir() string {
	if env := os.Getenv("AIC_RUNTIME_DIR"); env != "" {
		return env
	}
	// 以可执行文件位置向上找 runtime/（bin/ 或 build/ 下都能命中）
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for i := 0; i < 4; i++ {
			cand := filepath.Join(dir, "runtime")
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				return cand
			}
			dir = filepath.Dir(dir)
		}
	}
	if wd, err := os.Getwd(); err == nil {
		dir := wd
		for i := 0; i < 4; i++ {
			cand := filepath.Join(dir, "runtime")
			if st, err := os.Stat(cand); err == nil && st.IsDir() {
				return cand
			}
			dir = filepath.Dir(dir)
		}
	}
	return "runtime"
}

// findCC 选取 C 编译器：AIC_CC 优先，其次 cc/gcc/clang（PATH），最后 tcc。
// 开发路径可用 tcc（快），生产路径要 gcc/clang -O2（核心设计 §十一）。
func findCC() string {
	if cc := os.Getenv("AIC_CC"); cc != "" {
		return cc
	}
	for _, name := range []string{"cc", "gcc", "clang"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	for _, cand := range []string{
		filepath.Join(os.Getenv("SystemDrive")+`\`, "msys64", "ucrt64", "bin", "gcc.exe"),
		`D:\tools\tcc\tcc\tcc.exe`,
	} {
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

// runProgram 跑一个已构建的产物，返回 (stdout, 失败说明)。
// 失败说明 = 退出码非 0（含 trap）：trap 消息已在 stderr（继承给调用方）。
func runProgram(exe string) (string, error) {
	cmd := exec.Command(exe)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("non-zero exit code (%v)", err)
	}
	return out.String(), nil
}

// buildArgs 按 C 编译器家族拼参数（tcc 不认 -Wl,--no-insert-timestamp 与
// -finput-charset，硬塞会让四配置里的 tcc 直接失败）。
func buildArgs(cc, cPath, rt, out string, needL2 bool) []string {
	base := strings.ToLower(filepath.Base(cc))
	base = strings.TrimSuffix(base, ".exe")
	isTCC := strings.Contains(base, "tcc")
	args := []string{"-std=c11"}
	if !isTCC {
		args = append(args, "-finput-charset=UTF-8")
	}
	args = append(args, "-I", rt, cPath,
		filepath.Join(rt, "aic_l0.c"), filepath.Join(rt, "aic_std.c"))
	if needL2 {
		args = append(args, filepath.Join(rt, "aic_l2.c")) // 红线 16：按需链接
	}
	args = append(args, "-o", out)
	if !isTCC {
		args = append(args, "-lm")
	}
	// H2 构建确定性：PE 头的 COFF TimeDateStamp 默认写当前时间，同一输入两次构建
	// 产物必然不同。MinGW ld 用 --no-insert-timestamp 关掉（GNU ld/tcc 无此选项）。
	if runtime.GOOS == "windows" && !isTCC {
		args = append(args, "-Wl,--no-insert-timestamp")
	}
	return args
}

// buildExecutable 把 C 文本编译成可执行产物（外部进程 + 运行时按需链接）。
func buildExecutable(cText, out string, needL2 bool) error {
	cc := findCC()
	if cc == "" {
		return fmt.Errorf("no C compiler found (set AIC_CC, or put gcc/clang on PATH)")
	}
	rt := repoRuntimeDir()
	tmpDir, err := os.MkdirTemp("", "aic-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	cPath := filepath.Join(tmpDir, "tu.c")
	if err := os.WriteFile(cPath, []byte(cText), 0o644); err != nil {
		return err
	}
	args := buildArgs(cc, cPath, rt, out, needL2)
	cmd := exec.Command(cc, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("C compiler failed (%s %s): %v", cc, strings.Join(args, " "), err)
	}
	return nil
}
