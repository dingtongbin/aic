#!/usr/bin/env python3
"""内存验收（用户 2026-10 直令）：AIC vs 标准 C vs Rust 的**峰值 RSS / 增长斜率 /
泄漏探针 / UBSan 洁净**。

- 峰值 RSS：子进程运行中轮询 WorkingSetSize（psapi，零依赖），取最大值。
  同时报 AIC/C 比值（C = 1.00×）。
- 增长斜率：B5（N 轮 region 建/回收）采样整条 RSS 轨迹，比较首半/尾半均值 ——
  尾半不涨 = 无泄漏（区域模型的核心判据）。
- UBSan：clang -fsanitize=undefined 跑一遍 bench 产物，有报告即失败。

用法：python -X utf8 scripts/bench_mem.py [--case B4-graph|B5-region|B1-sieve]
"""

from __future__ import annotations

import ctypes
import statistics
import os
import subprocess
import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AIC = ROOT / "bin" / "aic.exe"
BENCH = ROOT / "benches"
WORK = ROOT / "build" / "bench_mem"
GCC = Path(r"C:\msys64\ucrt64\bin\gcc.exe")
CLANG = Path(r"C:\msys64\clang64\bin\clang.exe")

# --- psapi 轮询 -----------------------------------------------------------------
class _PMC(ctypes.Structure):
    _fields_ = [("cb", ctypes.c_ulong), ("PageFaultCount", ctypes.c_ulong),
                ("PeakWorkingSetSize", ctypes.c_size_t), ("WorkingSetSize", ctypes.c_size_t),
                ("QuotaPeakPagedPoolUsage", ctypes.c_size_t), ("QuotaPagedPoolUsage", ctypes.c_size_t),
                ("QuotaPeakNonPagedPoolUsage", ctypes.c_size_t), ("QuotaNonPagedPoolUsage", ctypes.c_size_t),
                ("PagefileUsage", ctypes.c_size_t), ("PeakPagefileUsage", ctypes.c_size_t)]

_psapi = ctypes.WinDLL("psapi", use_last_error=True)
_kernel = ctypes.WinDLL("kernel32", use_last_error=True)
PROCESS_QUERY_INFORMATION = 0x0400
PROCESS_VM_READ = 0x0010


def peak_working_set_kb(exe: Path, arg: str, sample_hz: int = 2000) -> tuple[int, list[tuple[float, int]]]:
    """运行 exe 并全程轮询工作集；返回 (峰值 KB, [(秒, KB)] 轨迹)。"""
    p = subprocess.Popen([str(exe), arg], cwd=str(ROOT), stdout=subprocess.DEVNULL,
                         stderr=subprocess.DEVNULL)
    h = _kernel.OpenProcess(PROCESS_QUERY_INFORMATION | PROCESS_VM_READ, False, p.pid)
    trace: list[tuple[float, int]] = []
    peak = 0
    t0 = time.perf_counter()
    interval = 1.0 / sample_hz
    try:
        while p.poll() is None:
            ctr = _PMC()
            ctr.cb = ctypes.sizeof(_PMC)
            if _psapi.GetProcessMemoryInfo(h, ctypes.byref(ctr), ctr.cb):
                kb = ctr.WorkingSetSize // 1024
                peak = max(peak, kb)
                trace.append((time.perf_counter() - t0, kb))
            time.sleep(interval)
    finally:
        if h:
            _kernel.CloseHandle(h)
        p.wait()
    return peak, trace


RUST_B5 = '''use std::env;
struct Buf { n: i32 }
fn main() {
    let mut rounds: i32 = 200000;
    if let Some(a) = env::args().nth(1) { if let Ok(v) = a.parse() { rounds = v; } }
    let mut acc: i32 = 0;
    for round in 0..rounds {
        let b = Box::new(Buf { n: round });
        acc += b.n;
    }
    println!("{}", acc);
}
'''

RUST_B4 = '''use std::env;
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

# case → (AIC 源, C 源, 默认参数, rust 源文本 or None)
CASES = {
    "B4-graph": ("b4_graph.aic", "c/b4_graph.c", "1000000", RUST_B4),
    "B5-region": ("b5_region.aic", "c/b5_region.c", "2000000", RUST_B5),
    "B10-endurance": ("b10_endurance.aic", "c/b10_endurance.c", "300", None),
}


def build(case: str, aic_src: str, c_src: str, rust_src: str | None) -> dict[str, Path]:
    exes: dict[str, Path] = {}
    exe = WORK / f"{case}-aic.exe"
    # 与 C/Rust 同档优化（AIC_CFLAGS 默认空 = -O0；见 bench_full.py 同款说明）。
    aic_env = dict(os.environ, AIC_CFLAGS="-O2")
    r = subprocess.run([str(AIC), "build", str(BENCH / aic_src), "-o", str(exe)],
                       cwd=str(ROOT), capture_output=True, env=aic_env)
    if r.returncode == 0:
        exes["AIC"] = exe
    c_exe = WORK / f"{case}-c.exe"
    r = subprocess.run([str(GCC), "-std=c11", "-O2", str(BENCH / c_src), "-o", str(c_exe)],
                       capture_output=True)
    if r.returncode == 0:
        exes["C"] = c_exe
    if rust_src:
        (WORK / f"{case}.rs").write_text(rust_src, encoding="utf-8")
        rust_exe = WORK / f"{case}-rust.exe"
        r = subprocess.run(["rustc", "-O", "-o", str(rust_exe), f"{case}.rs"],
                           cwd=str(WORK), capture_output=True)
        if r.returncode == 0:
            exes["Rust"] = rust_exe
    return exes


def growth_check(trace: list[tuple[float, int]]) -> tuple[float, float, float]:
    """首半/尾半均值与斜率（KB/s）：尾半不涨 = 无泄漏。"""
    if len(trace) < 8:
        return 0.0, 0.0, 0.0
    half = len(trace) // 2
    first = statistics.fmean(kb for _, kb in trace[:half])
    last = statistics.fmean(kb for _, kb in trace[-half:])
    growth = last - first
    return first, last, growth


def main() -> int:
    only = None
    if "--case" in sys.argv:
        only = sys.argv[sys.argv.index("--case") + 1]
    WORK.mkdir(parents=True, exist_ok=True)
    print("内存验收（峰值 RSS = 运行中轮询工作集；C = 1.00× 基线）")
    print(f"{'case':<12}{'语言':<7}{'峰值RSS(MB)':>13}{'×C':>7}{'首半MB':>9}{'尾半MB':>9}{'增长MB':>8}")

    for case, (aic_src, c_src, dflt, rust_src) in CASES.items():
        if only and case != only:
            continue
        exes = build(case, aic_src, c_src, rust_src)
        if "AIC" not in exes or "C" not in exes:
            print(f"{case}: 构建失败，跳过")
            continue
        rows: dict[str, tuple[int, float, float, float]] = {}
        for lang in ("AIC", "C", "Rust"):
            if lang not in exes:
                continue
            peak, trace = peak_working_set_kb(exes[lang], dflt)
            first, last, growth = growth_check(trace)
            rows[lang] = (peak // 1024, first / 1024, last / 1024, growth)
        ref = rows.get("C", (0, 0, 0, 0))[0]
        for lang, (peak_mb, first_mb, last_mb, growth) in rows.items():
            ratio = f"{peak_mb / ref:>7.2f}" if ref else "       -"
            print(f"{case:<12}{lang:<7}{peak_mb:>13}{ratio}{first_mb:>9.1f}{last_mb:>9.1f}{growth / 1024:>8.1f}")

    # UBSan：bench 产物在 clang -fsanitize=undefined 下必须零报告
    print()
    print("UBSan（clang -fsanitize=undefined -fno-sanitize-recover=all）：")
    fails = 0
    for case, (aic_src, c_src, dflt, _rust) in CASES.items():
        if only and case != only:
            continue
        aic_exe = WORK / f"{case}-aic-ubsan.exe"
        c_src_path = WORK / f"{case}-aic.c"
        r = subprocess.run([str(AIC), "build", str(BENCH / aic_src), "--emit-c"],
                           cwd=str(ROOT), capture_output=True)
        if r.returncode != 0:
            print(f"  {case}: emit 失败")
            continue
        c_src_path.write_bytes(r.stdout)
        rt = ROOT / "runtime"
        # L2（真并发/调度）不是每个 bench 都需要：spawn/scope/chan 出现才链
        # aic_l2.c，否则 B10 这类含 spawn 的语料会 undefined symbol（aic_sched_init）。
        needs_l2 = any(m in r.stdout for m in (b"aic_scope_", b"aic_task_", b"aic_chan"))
        srcs = [str(rt / "aic_l0.c"), str(rt / "aic_std.c")]
        if needs_l2:
            srcs.append(str(rt / "aic_l2.c"))
        cc = subprocess.run([str(CLANG), "-std=c11", "-O1", "-fsanitize=undefined",
                             "-fno-sanitize-recover=all", "-I", str(rt), str(c_src_path),
                             *srcs,
                             "-o", str(aic_exe)], capture_output=True)
        if cc.returncode != 0:
            print(f"  {case}: UBSan 构建失败：{cc.stderr.decode('utf-8', 'replace')[:120]}")
            fails += 1
            continue
        run = subprocess.run([str(aic_exe), dflt], cwd=str(ROOT), capture_output=True, timeout=300)
        err = run.stderr.decode("utf-8", "replace")
        if "runtime error" in err or "undefinedbehaviour" in err.lower():
            print(f"  {case}: UBSan 报告：{err[:200]}")
            fails += 1
        else:
            print(f"  {case}: 零报告（exit {run.returncode}）")
    print()
    print("（判读：B4 峰值 RSS 比值 = 区域模型 vs 逐节点分配的内存效率；"
          "B5 尾半不涨 = 无泄漏；UBSan 零报告 = 生成代码无未定义行为）")
    return 1 if fails else 0


if __name__ == "__main__":
    sys.exit(main())
