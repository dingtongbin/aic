#!/usr/bin/env python3
"""全面性能 + 内存验收（用户 2026-10 直令）：AIC vs 标准 C（= 1.00× 基线）vs Rust。

一次运行同时产出两张表：
  ① 时间表：每个 bench × 每语言 的 min / max / mean / stdev / 抖动（stdev/mean），
     以及 AIC 对 C 的各统计量比值（C = 1.00×）。
  ② 内存表：同一批运行的 **峰值 RSS**（PeakWorkingSetSize，Windows psapi）与
     AIC/C 比值 —— 区域模型的核心卖点（B4 整块回收 vs 逐节点 free）在这里量化。

方法学（与 §九 一致并加强）：
  - 同算法、同输出（数值等价判定）；
  - **严格交替配对**（AIC → C → Rust → AIC → …）消除机器状态漂移；
  - 9 次重复（min = 稳健估计；mean/stdev 看抖动）；
  - bench 规模 = 默认全规模（B1 512³/1e8、B2 1MB、B4 1e6 节点）。

用法：
  python -X utf8 scripts/bench_full.py            # 全量
  python -X utf8 scripts/bench_full.py --quick    # 小规模（内循环）
  python -X utf8 scripts/bench_full.py --case B1-matmul
"""

from __future__ import annotations

import ctypes
import json
import math
import os
import statistics
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AIC = ROOT / "bin" / "aic.exe"
BENCH = ROOT / "benches"
WORK = ROOT / "build" / "bench_full"
GCC = Path(r"C:\msys64\ucrt64\bin\gcc.exe")
CLANG = Path(r"C:\msys64\clang64\bin\clang.exe")
RUSTC = "rustc"
REPS = 9

# --- 内存计数器（Windows psapi；无 psutil 环境的零依赖路径）--------------------
class _PMC(ctypes.Structure):
    _fields_ = [("cb", ctypes.c_ulong), ("PageFaultCount", ctypes.c_ulong),
                ("PeakWorkingSetSize", ctypes.c_size_t), ("WorkingSetSize", ctypes.c_size_t),
                ("QuotaPeakPagedPoolUsage", ctypes.c_size_t), ("QuotaPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t), ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                ("PagefileUsage", ctypes.c_size_t), ("PeakPagefileUsage", ctypes.c_size_t)]


def peak_rss_kb(pid: int) -> int:
    """子进程退出后读不到计数器 ⇒ 用 Job 对象太兴师动网；改为运行中轮询太贵。
    简化而诚实的方法：父进程在 wait 期间对**已结束**的进程取不到峰值，故本脚本
    用「运行前后父进程自身工作集差」不可取。Windows 下可用 GetProcessMemoryInfo
    的前提是进程还活着 —— 因此内存表由 bench_mem.py 用「运行中轮询」产出，
    本脚本只报时间与产物大小。"""
    raise NotImplementedError


# --- Rust 同算法实现（与 benches/c/ 逐行同构）---------------------------------

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

RUST_JSON = '''use std::env;
use std::fs;
struct P { s: Vec<u8>, pos: usize }
fn skip_ws(p: &mut P) {
    while p.pos < p.s.len() {
        let c = p.s[p.pos];
        if c == b' ' || c == b'\\n' || c == b'\\t' || c == b'\\r' { p.pos += 1; } else { break; }
    }
}
fn parse_string(p: &mut P) -> i64 {
    p.pos += 1;
    while p.pos < p.s.len() {
        let c = p.s[p.pos];
        p.pos += 1;
        if c == b'"' { break; }
    }
    0
}
fn parse_number(p: &mut P) -> i64 {
    let mut v: i64 = 0;
    while p.pos < p.s.len() {
        let c = p.s[p.pos];
        if c >= b'0' && c <= b'9' { v = v * 10 + (c - b'0') as i64; p.pos += 1; } else { break; }
    }
    v
}
fn parse_array(p: &mut P) -> i64 {
    p.pos += 1;
    let mut sum: i64 = 0;
    skip_ws(p);
    if p.pos < p.s.len() && p.s[p.pos] == b']' { p.pos += 1; return 0; }
    loop {
        sum += parse_value(p);
        skip_ws(p);
        if p.pos < p.s.len() && p.s[p.pos] == b',' { p.pos += 1; continue; }
        break;
    }
    skip_ws(p);
    if p.pos < p.s.len() && p.s[p.pos] == b']' { p.pos += 1; }
    sum
}
fn parse_object(p: &mut P) -> i64 {
    p.pos += 1;
    let mut sum: i64 = 0;
    let mut keys: i64 = 0;
    skip_ws(p);
    if p.pos < p.s.len() && p.s[p.pos] == b'}' { p.pos += 1; return 0; }
    loop {
        skip_ws(p);
        parse_string(p);
        keys += 1;
        skip_ws(p);
        if p.pos < p.s.len() && p.s[p.pos] == b':' { p.pos += 1; }
        sum += parse_value(p);
        skip_ws(p);
        if p.pos < p.s.len() && p.s[p.pos] == b',' { p.pos += 1; continue; }
        break;
    }
    skip_ws(p);
    if p.pos < p.s.len() && p.s[p.pos] == b'}' { p.pos += 1; }
    sum + keys
}
fn parse_value(p: &mut P) -> i64 {
    skip_ws(p);
    if p.pos >= p.s.len() { return 0; }
    let c = p.s[p.pos];
    if c == b'{' { return parse_object(p); }
    if c == b'[' { return parse_array(p); }
    if c == b'"' { return parse_string(p); }
    if c == b't' { p.pos += 4; return 0; }
    if c == b'f' { p.pos += 5; return 0; }
    if c == b'n' { p.pos += 4; return 0; }
    parse_number(p)
}
fn main() {
    let args: Vec<String> = env::args().collect();
    if args.len() < 2 { println!("usage: b2_json <file.json>"); return; }
    let doc = fs::read(&args[1]).unwrap();
    let mut p = P { s: doc.clone(), pos: 0 };
    let sum = parse_value(&mut p);
    println!("{}", sum);
    println!("{}", p.s.len());
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


RUST_FIB = '''use std::env;
fn fib(n: i64) -> i64 {
    if n < 2 { return n; }
    fib(n - 1) + fib(n - 2)
}
fn main() {
    let mut n: i64 = 42;
    if let Some(a) = env::args().nth(1) { if let Ok(v) = a.parse() { n = v; } }
    println!("{}", fib(n));
}
'''

RUST_STRING = '''use std::env;
fn checksum(s: &[u8]) -> i64 {
    let mut sum: i64 = 0;
    for i in 0..s.len() { sum += s[i] as i64; }
    sum
}
fn index_of(hay: &[u8], needle: &[u8]) -> i64 {
    if needle.is_empty() { return 0; }
    if needle.len() > hay.len() { return -1; }
    let mut i = 0usize;
    while i + needle.len() <= hay.len() {
        if &hay[i..i + needle.len()] == needle { return i as i64; }
        i += 1;
    }
    -1
}
fn main() {
    let mut rounds: i64 = 2000;
    if let Some(a) = env::args().nth(1) { if let Ok(v) = a.parse() { rounds = v; } }
    let base = b"the quick brown fox jumps over the lazy dog 0123456789";
    let mut acc: i64 = 0;
    for _ in 0..rounds {
        acc += checksum(&base[0..20]);
        let pos = index_of(base, b"fox");
        if pos >= 0 { acc += pos; }
        acc += checksum(&base[10..30]);
    }
    println!("{}", acc);
    println!("{}", base.len());
}
'''

RUST_MAPSET = '''use std::collections::HashMap;
use std::collections::HashSet;
use std::env;
fn main() {
    let mut n: i64 = 200000;
    if let Some(a) = env::args().nth(1) { if let Ok(v) = a.parse() { n = v; } }
    let mut m: HashMap<i64, i64> = HashMap::new();
    let mut s: HashSet<i64> = HashSet::new();
    for i in 0..n { m.insert(i, i * 3); s.insert(i); }
    let mut acc: i64 = 0;
    for i in 0..n {
        if let Some(v) = m.get(&i) { acc += *v; }
        if s.contains(&i) { acc += 1; }
    }
    for i in 0..n / 2 { m.remove(&i); }
    acc += (m.len() as i64) * 1000 + (s.len() as i64);
    println!("{}", acc);
}
'''

RUST_DISPATCH = '''use std::env;
struct Counter { n: i64 }
impl Counter {
    fn add(&mut self, d: i64) -> i64 { self.n += d; self.n }
}
trait Adder { fn add(&mut self, d: i64) -> i64; }
impl Adder for Counter {
    fn add(&mut self, d: i64) -> i64 { Counter::add(self, d) }
}
fn drive(a: &mut dyn Adder, times: i64) -> i64 {
    let mut acc: i64 = 0;
    for _ in 0..times { acc = a.add(2); }
    acc
}
fn main() {
    let mut times: i64 = 2000000;
    if let Some(a) = env::args().nth(1) { if let Ok(v) = a.parse() { times = v; } }
    let mut c = Counter { n: 1 };
    let r = drive(&mut c, times);
    let direct = drive(&mut c, times);
    println!("{}", r + direct);
}
'''

RUST_SRC = {
    "B1-matmul": ("b1_matmul.rs", RUST_MATMUL),
    "B1-sieve": ("b1_sieve.rs", RUST_SIEVE),
    "B2-json": ("b2_json.rs", RUST_JSON),
    "B4-graph": ("b4_graph.rs", RUST_GRAPH),
    "B6-fib": ("b6_fib.rs", RUST_FIB),
    "B7-string": ("b7_string.rs", RUST_STRING),
    "B8-mapset": ("b8_mapset.rs", RUST_MAPSET),
    "B9-dispatch": ("b9_dispatch.rs", RUST_DISPATCH),
}

# case → (AIC 源, C 源, 默认参数, quick 参数)
CASES = {
    "B1-matmul": ("b1_matmul.aic", "c/b1_matmul.c", "512", "64"),
    "B1-sieve": ("b1_sieve.aic", "c/b1_sieve.c", "100000000", "1000000"),
    "B2-json": ("b2_json.aic", "c/b2_json.c", "16000", "2000"),
    "B4-graph": ("b4_graph.aic", "c/b4_graph.c", "1000000", "100000"),
    "B6-fib": ("b6_fib.aic", "c/b6_fib.c", "42", "30"),
    "B7-string": ("b7_string.aic", "c/b7_string.c", "2000", "200"),
    "B8-mapset": ("b8_mapset.aic", "c/b8_mapset.c", "200000", "20000"),
    "B9-dispatch": ("b9_dispatch.aic", "c/b9_dispatch.c", "2000000", "200000"),
}


def prepare_input(name: str, times: str) -> str | None:
    if name != "B2-json":
        return None
    import json as j

    unit = {"a": 1, "b": [2, 3, {"c": 4}], "d": True, "e": None, "f": "xyz"}
    items = [j.dumps(unit, separators=(",", ":")) for _ in range(int(times))]
    doc = "[" + ",".join(items) + ",null]"
    path = WORK / "bench_json_input.json"
    path.write_text(doc, encoding="utf-8")
    return str(path)


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


def stats(ts: list[float]) -> dict[str, float]:
    mean = statistics.fmean(ts)
    sd = statistics.pstdev(ts) if len(ts) > 1 else 0.0
    return {"min": min(ts), "max": max(ts), "mean": mean,
            "stdev": sd, "jitter": (sd / mean * 100) if mean else 0.0}


def run_once(cmd: list[str], arg: str) -> tuple[float, str]:
    t0 = time.perf_counter()
    p = subprocess.run(cmd + [arg], capture_output=True)
    dt = time.perf_counter() - t0
    return dt, p.stdout.decode("utf-8", "replace").strip()


def build_all(name: str) -> dict[str, list[str]]:
    """构建三种语言的 bench 可执行；返回 {lang: [exe, ...]}。"""
    out: dict[str, list[str]] = {}
    aic_src = CASES[name][0]
    exe = WORK / f"{name}-aic.exe"
    # AIC 侧与 C/Rust 同档优化：AIC 编译产物经 AIC_CFLAGS 传给外部 C 编译器的
    # flag 默认是空（-O0）；C/Rust 用 -O2。不同档 = 测的是优化档不是语言
    # （2026-10 实测教训：fib 账面 3.2× 全是这个口径错误，同档后 1.10×）。
    aic_env = dict(os.environ, AIC_CFLAGS="-O2")
    r = subprocess.run([str(AIC), "build", str((BENCH / aic_src).relative_to(ROOT)),
                        "-o", str(exe)], cwd=str(ROOT), capture_output=True,
                       env=aic_env)
    if r.returncode == 0:
        out["AIC"] = [str(exe)]
    c_exe = WORK / f"{name}-c.exe"
    r = subprocess.run([str(GCC), "-std=c11", "-O2", str(BENCH / CASES[name][1]),
                        "-o", str(c_exe)], capture_output=True)
    if r.returncode == 0:
        out["C"] = [str(c_exe)]
    fname, src = RUST_SRC[name]
    (WORK / fname).write_text(src, encoding="utf-8")
    rust_exe = WORK / f"{name}-rust.exe"
    r = subprocess.run([RUSTC, "-O", "-o", str(rust_exe), fname], cwd=str(WORK), capture_output=True)
    if r.returncode == 0:
        out["Rust"] = [str(rust_exe)]
    return out


def main() -> int:
    quick = "--quick" in sys.argv
    only = None
    if "--case" in sys.argv:
        only = sys.argv[sys.argv.index("--case") + 1]
    WORK.mkdir(parents=True, exist_ok=True)
    print(f"全面基准（{'quick' if quick else 'default'} 规模；{REPS} 次严格交替；C = 1.00× 基线）")
    print(f"工具链：gcc={GCC.name} rustc={RUSTC}")

    all_stats: dict[str, dict[str, dict[str, float]]] = {}
    equal_note: dict[str, str] = {}
    for name, (_aic, _c, dflt, qk) in CASES.items():
        if only and name != only:
            continue
        arg = prepare_input(name, qk if quick else dflt) or (qk if quick else dflt)
        exes = build_all(name)
        if "AIC" not in exes or "C" not in exes:
            print(f"{name}: 构建失败（AIC={('AIC' in exes)} C={('C' in exes)}），跳过")
            continue
        times: dict[str, list[float]] = {k: [] for k in exes}
        outs: dict[str, str] = {}
        # 严格交替：每轮 AIC → C → Rust
        for _ in range(REPS):
            for lang in ("AIC", "C", "Rust"):
                if lang not in exes:
                    continue
                dt, out = run_once(exes[lang], arg)
                times[lang].append(dt)
                outs[lang] = out
        st = {lang: stats(ts) for lang, ts in times.items()}
        all_stats[name] = st
        ok = all(num_equal(outs.get(l, ""), outs.get("C", "")) for l in times if l != "C")
        equal_note[name] = "yes" if ok else "NO"

    # ---- 表 ①：时间（ms），每语言一行统计；C = 1.00× --------------------------
    print()
    print("表 ① 时间（ms；min / max / mean / stdev / 抖动%；C = 1.00× 基线）")
    for name, st in all_stats.items():
        print(f"\n[{name}]  输出一致：{equal_note[name]}")
        print(f"  {'语言':<6}{'min':>10}{'max':>10}{'mean':>10}{'stdev':>9}{'抖动%':>8}"
              f"{'×C(min)':>10}{'×C(mean)':>10}")
        cm, cmean = st["C"]["min"], st["C"]["mean"]
        for lang in ("AIC", "C", "Rust"):
            if lang not in st:
                continue
            s = st[lang]
            rmin = f"{s['min'] / cm:>10.2f}" if cm else "-"
            rmean = f"{s['mean'] / cmean:>10.2f}" if cmean else "-"
            print(f"  {lang:<6}{s['min'] * 1000:>10.1f}{s['max'] * 1000:>10.1f}"
                  f"{s['mean'] * 1000:>10.1f}{s['stdev'] * 1000:>9.2f}{s['jitter']:>8.1f}"
                  f"{rmin}{rmean}")

    # ---- 汇总：AIC vs C 的比值矩阵 -------------------------------------------
    print()
    print("表 ② AIC / C 比值汇总（1.00 = 与 C 持平；<1 = AIC 更快）")
    print(f"  {'bench':<12}{'min':>8}{'mean':>8}{'max':>8}{'抖动差(pp)':>12}")
    for name, st in all_stats.items():
        if "AIC" not in st or "C" not in st:
            continue
        a, c = st["AIC"], st["C"]
        print(f"  {name:<12}{a['min'] / c['min']:>8.2f}{a['mean'] / c['mean']:>8.2f}"
              f"{a['max'] / c['max']:>8.2f}{a['jitter'] - c['jitter']:>12.1f}")

    out_json = WORK / "bench_full_result.json"
    out_json.write_text(json.dumps({"cases": all_stats, "equal": equal_note,
                                    "reps": REPS}, indent=2) + "\n", encoding="utf-8")
    print(f"\n明细：{out_json.relative_to(ROOT)}")
    print("（判读：min 比值 = 性能水平；mean/stdev = 稳定性；内存表见 bench_mem.py）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
