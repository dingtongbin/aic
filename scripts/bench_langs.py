#!/usr/bin/env python3
"""多语言性能对照：AIC vs C -O2 vs Go vs Rust -O vs Java（同算法、同输出）。

用途：回答「AIC 大致相当于主流语言的什么水平」。判据与 §九 一致 —— **同算法**、
**同输出**（数值等价），交替配对测量取 min（稳健估计）。

- C     : gcc -std=c11 -O2
- Go    : go build（默认优化；GOGC 默认）
- Rust  : rustc -O
- Java  : javac（JDK8）+ java；JIT 需预热 → 取多次运行的最小值（预热后的稳态）
- AIC   : bin/aic.exe build（默认 -O2 走 gcc）

用法：python scripts/bench_langs.py [--quick]
"""

from __future__ import annotations

import os
import shutil
import statistics
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AIC = ROOT / "bin" / "aic.exe"
WORK = ROOT / "build" / "_langs"  # `_` 前缀 = Go 工具链忽略
WORK.mkdir(parents=True, exist_ok=True)

# --- 各语言的同算法实现（与 benches/ 下的 AIC 版逐行同构） --------------------

GO_SIEVE = '''package main

import ("fmt"; "os"; "strconv")

func main() {
	limit := int64(100000000)
	if len(os.Args) >= 2 { if v, err := strconv.ParseInt(os.Args[1], 10, 64); err == nil { limit = v } }
	composite := make([]bool, limit+1)
	for p := int64(2); p*p <= limit; p++ {
		if !composite[p] { for m := p * p; m <= limit; m += p { composite[m] = true } }
	}
	var count int64
	for i := int64(2); i <= limit; i++ { if !composite[i] { count++ } }
	fmt.Println(count)
}
'''

GO_MATMUL = '''package main

import ("fmt"; "os"; "strconv")

func main() {
	n := int64(512)
	if len(os.Args) >= 2 { if v, err := strconv.ParseInt(os.Args[1], 10, 64); err == nil { n = v } }
	size := n * n
	a := make([]float64, size)
	b := make([]float64, size)
	c := make([]float64, size)
	for i := int64(0); i < size; i++ { a[i] = 1.5; b[i] = 2.5; c[i] = 0.0 }
	for i := int64(0); i < n; i++ {
		for j := int64(0); j < n; j++ {
			sum := 0.0
			for k := int64(0); k < n; k++ { sum += a[i*n+k] * b[k*n+j] }
			c[i*n+j] = sum
		}
	}
	checksum := 0.0
	for i := int64(0); i < size; i++ { checksum += c[i] }
	fmt.Printf("%.6f\\n", checksum)
}
'''

GO_GRAPH = '''package main

import ("fmt"; "os"; "strconv")

type Node struct { value int64; next *Node }

func main() {
	n := int64(1000000)
	if len(os.Args) >= 2 { if v, err := strconv.ParseInt(os.Args[1], 10, 64); err == nil { n = v } }
	var head *Node
	for i := int64(0); i < n; i++ { head = &Node{value: i, next: head} }
	var sum int64
	for cur := head; cur != nil; cur = cur.next { sum += cur.value }
	fmt.Println(sum)
}
'''

RUST_SIEVE = '''use std::env;
fn main() {
    let mut limit: i64 = 100_000_000;
    if let Some(a) = env::args().nth(1) { if let Ok(v) = a.parse() { limit = v; } }
    let mut composite = vec![false; (limit + 1) as usize];
    let mut p: i64 = 2;
    while p * p <= limit {
        if !composite[p as usize] {
            let mut m = p * p;
            while m <= limit { composite[m as usize] = true; m += p; }
        }
        p += 1;
    }
    let mut count: i64 = 0;
    for i in 2..=limit { if !composite[i as usize] { count += 1; } }
    println!("{}", count);
}
'''

RUST_MATMUL = '''use std::env;
fn main() {
    let mut n: i64 = 512;
    if let Some(a) = env::args().nth(1) { if let Ok(v) = a.parse() { n = v; } }
    let size = (n * n) as usize;
    let mut a = vec![0.0f64; size];
    let mut b = vec![0.0f64; size];
    let mut c = vec![0.0f64; size];
    for i in 0..size { a[i] = 1.5; b[i] = 2.5; c[i] = 0.0; }
    for i in 0..n {
        for j in 0..n {
            let mut sum = 0.0f64;
            for k in 0..n { sum += a[(i * n + k) as usize] * b[(k * n + j) as usize]; }
            c[(i * n + j) as usize] = sum;
        }
    }
    let mut checksum = 0.0f64;
    for i in 0..size { checksum += c[i]; }
    println!("{:.6}", checksum);
}
'''

RUST_GRAPH = '''use std::env;
struct Node { value: i64, next: Option<Box<Node>> }
fn main() {
    let mut n: i64 = 1_000_000;
    if let Some(a) = env::args().nth(1) { if let Ok(v) = a.parse() { n = v; } }
    let mut head: Option<Box<Node>> = None;
    for i in 0..n { head = Some(Box::new(Node { value: i, next: head })); }
    let mut sum: i64 = 0;
    let mut cur: Option<&Node> = head.as_deref();
    while let Some(node) = cur { sum += node.value; cur = node.next.as_deref(); }
    println!("{}", sum);
}
'''

JAVA_SIEVE = '''public class B1Sieve {
    public static void main(String[] args) {
        long limit = 100000000L;
        if (args.length >= 1) limit = Long.parseLong(args[0]);
        boolean[] composite = new boolean[(int)(limit + 1)];
        for (long p = 2; p * p <= limit; p++) {
            if (!composite[(int)p]) { for (long m = p * p; m <= limit; m += p) composite[(int)m] = true; }
        }
        long count = 0;
        for (long i = 2; i <= limit; i++) if (!composite[(int)i]) count++;
        System.out.println(count);
    }
}
'''

JAVA_MATMUL = '''public class B1Matmul {
    public static void main(String[] args) {
        int n = 512;
        if (args.length >= 1) n = Integer.parseInt(args[0]);
        int size = n * n;
        double[] a = new double[size], b = new double[size], c = new double[size];
        for (int i = 0; i < size; i++) { a[i] = 1.5; b[i] = 2.5; c[i] = 0.0; }
        for (int i = 0; i < n; i++) {
            for (int j = 0; j < n; j++) {
                double sum = 0.0;
                for (int k = 0; k < n; k++) sum += a[i * n + k] * b[k * n + j];
                c[i * n + j] = sum;
            }
        }
        double checksum = 0.0;
        for (int i = 0; i < size; i++) checksum += c[i];
        System.out.printf("%.6f%n", checksum);
    }
}
'''

# case → (AIC 源, C 源, 默认参数, quick 参数, {语言: 源文本})
CASES = {
    "B1-sieve": ("b1_sieve.aic", "c/b1_sieve.c", "100000000", "1000000",
                 {"go": ("b1_sieve.go", GO_SIEVE), "rust": ("b1_sieve.rs", RUST_SIEVE),
                  "java": ("B1Sieve.java", JAVA_SIEVE)}),
    "B1-matmul": ("b1_matmul.aic", "c/b1_matmul.c", "512", "64",
                  {"go": ("b1_matmul.go", GO_MATMUL), "rust": ("b1_matmul.rs", RUST_MATMUL),
                   "java": ("B1Matmul.java", JAVA_MATMUL)}),
    "B4-graph": ("b4_graph.aic", "c/b4_graph.c", "1000000", "100000",
                 {"go": ("b4_graph.go", GO_GRAPH), "rust": ("b4_graph.rs", RUST_GRAPH)}),
}


def run(cmd, cwd=ROOT, timeout=600):
    p = subprocess.run([str(c) for c in cmd], cwd=str(cwd), capture_output=True, timeout=timeout)
    return p.returncode, p.stdout, p.stderr


def find(name: str) -> str | None:
    return shutil.which(name)


def time_runs(exe_cmd: list, reps: int = 5) -> tuple[float, str]:
    """交替无关：同一命令跑 reps 次取 min（稳健估计）+ 最后一次输出。"""
    best, out = float("inf"), ""
    for _ in range(reps):
        t0 = time.perf_counter()
        rc, o, _ = run(exe_cmd)
        dt = time.perf_counter() - t0
        if rc != 0:
            return float("nan"), ""
        best = min(best, dt)
        out = o.decode("utf-8", "replace").strip()
    return best, out


def main() -> int:
    quick = "--quick" in sys.argv
    cc = find("gcc") or find("clang")
    langs = {"AIC": bool(AIC.is_file()), "C": bool(cc), "Go": bool(find("go")),
             "Rust": bool(find("rustc")), "Java": bool(find("java") and find("javac"))}
    print("多语言对照（同算法、同输出；C -O2 = 1.00×）：",
          ", ".join(f"{k}={'有' if v else '无'}" for k, v in langs.items()))
    header = f"{'bench':<11}" + "".join(f"{k:>10}" for k in langs) + f"{'AIC/C':>8}  {'输出一致':>8}"
    print(header)

    only = None
    if "--case" in sys.argv:
        only = sys.argv[sys.argv.index("--case") + 1]
    for name, (aic_src, c_src, dflt, qk, others) in CASES.items():
        if only and name != only:
            continue
        arg = qk if quick else dflt
        times: dict[str, float] = {}
        outs: dict[str, str] = {}

        if langs["AIC"]:
            exe = WORK / f"{name}-aic.exe"
            rc, _, err = run([AIC, "build", str((ROOT / "benches" / aic_src)), "-o", exe])
            if rc == 0:
                times["AIC"], outs["AIC"] = time_runs([exe, arg])
        if langs["C"]:
            exe = WORK / f"{name}-c.exe"
            rc, _, _ = run([cc, "-std=c11", "-O2", str(ROOT / "benches" / c_src), "-o", exe])
            if rc == 0:
                times["C"], outs["C"] = time_runs([exe, arg])
        if langs["Go"] and "go" in others:
            fname, src = others["go"]
            (WORK / fname).write_text(src, encoding="utf-8")
            exe = WORK / f"{name}-go.exe"
            rc, _, err = run(["go", "build", "-o", exe, fname], cwd=WORK)
            if rc == 0:
                times["Go"], outs["Go"] = time_runs([exe, arg])
        if langs["Rust"] and "rust" in others:
            fname, src = others["rust"]
            (WORK / fname).write_text(src, encoding="utf-8")
            exe = WORK / f"{name}-rust.exe"
            rc, _, err = run(["rustc", "-O", "-o", exe, fname], cwd=WORK)
            if rc == 0:
                times["Rust"], outs["Rust"] = time_runs([exe, arg])
        if langs["Java"] and "java" in others:
            fname, src = others["java"]
            (WORK / fname).write_text(src, encoding="utf-8")
            rc, _, err = run(["javac", fname], cwd=WORK)
            if rc == 0:
                cls = fname[:-5]
                times["Java"], outs["Java"] = time_runs(["java", "-cp", str(WORK), cls, arg])

        ref = times.get("C", float("nan"))
        cells = []
        for k in langs:
            t = times.get(k)
            cells.append(f"{t * 1000:>10.1f}" if t and t == t else f"{'-':>10}")
        same = "yes"
        ref_out = outs.get("C", "")
        for k, o in outs.items():
            if k == "C" or k not in times or ref_out == "":
                continue
            if not num_equal(o, ref_out):
                same = f"NO({k})"
        ratio = f"{times['AIC'] / ref:>8.2f}" if "AIC" in times and ref == ref else f"{'-':>8}"
        print(f"{name:<11}" + "".join(cells) + ratio + f"  {same:>8}")

    print("（单位 ms；交替配对取 min；Java 需 JIT 预热故取多次最小值）")
    return 0


def num_equal(a: str, b: str) -> bool:
    la, lb = a.strip().splitlines(), b.strip().splitlines()
    if len(la) != len(lb):
        return False
    for x, y in zip(la, lb):
        if x == y:
            continue
        try:
            fx, fy = float(x), float(y)
        except ValueError:
            return False
        if abs(fx - fy) > 1e-6 * max(1.0, abs(fx), abs(fy)):
            return False
    return True


if __name__ == "__main__":
    sys.exit(main())
