package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmdDumpSyms 实现 `aic dump syms <file.aic> [-o aic_syms.h]`。
//
// 内部符号是 `aic_` + sha256(包身份路径::recv::name) 前 8 字节 —— **不可逆**。
// 崩溃现场、性能剖析、C 侧断点看到的都是 `aic_9f3a...`；没有这张对照表就回不到
// 源码里的限定名。所以哈希符号与对照表是**一体**的（核心设计 §10.4 的可调试性对价）。
//
// 产物形态是 C 头：一行一条注释，外加 `AIC_SYMS_COUNT`。注释而不是宏，
// 是为了让它能被 `#include` 进任何 TU 而不产生符号。
func cmdDumpSyms(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "aic dump syms: no input file")
		return 2
	}
	prog, diag := loadProgram(args[0])
	if prog == nil {
		fmt.Fprintf(os.Stderr, "aic dump syms: %s\n", diag)
		return 1
	}
	res, err := emitProgram(prog, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "aic dump syms: emit failed: %v\n", err)
		return 1
	}
	var b strings.Builder
	b.WriteString("/* aic_syms.h — 内部符号 ↔ AIC 限定名对照表。\n")
	b.WriteString(" * 内部符号 = aic_ + sha256(包身份路径::recv::name) 前 8 字节（不可逆），\n")
	b.WriteString(" * 故本表是「从 aic_xxxxxxxx 回到源码限定名」的唯一途径。\n")
	b.WriteString(" * export/extern 符号原名直出，不在此表（它们本来就是可读的）。\n")
	b.WriteString(" */\n")
	b.WriteString("#define AIC_SYMS_COUNT " + strconv.Itoa(len(res.Syms)) + "\n")
	for _, e := range res.Syms {
		b.WriteString("/* " + e.C + "  <=  " + e.AIR + "  [" + e.Kind + "] */\n")
	}
	out := b.String()
	if o, ok := flagValue(args, "-o"); ok {
		if err := os.WriteFile(o, []byte(out), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "aic dump syms: cannot write %s: %v\n", o, err)
			return 2
		}
		return 0
	}
	fmt.Print(out)
	return 0
}

// firstOr 取切片首元素，空切片返回默认值（CLI 诊断用）。
func firstOr(xs []string, def string) string {
	if len(xs) == 0 {
		return def
	}
	return xs[0]
}
