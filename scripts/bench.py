#!/usr/bin/env python3
"""性能验收（黄金执行规划 §九）：B1 数值核（AIC vs 同算法手写 C -O2）。

度量 = 墙钟（5 次取中位）+ 峰值 RSS（可选，psutil 缺失时跳过）+ 产物大小。
基线入 `benches/baseline.json`（`--record` 写，默认比对；回归 = 报告不判负，
因为 T2 的优化工作尚未落地 —— 判负留给 `ci.py --perf` 强制档，§九）。

用法：
  python scripts/bench.py                 # 跑默认规模（B1：512³ + 1e8）
  python scripts/bench.py --quick         # 小规模（64³ + 1e6），内循环用
  python scripts/bench.py --record        # 记录基线
"""

from __future__ import annotations

import json
import os
import statistics
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
AIC = ROOT / "bin" / "aic.exe"
BENCH = ROOT / "benches"
WORK = ROOT / "build" / "bench"
BASELINE = BENCH / "baseline.json"

# bench 定义：名字 → (AIC 源, C 基线源, 默认参数, quick 参数)
CASES = {
    "B1-matmul": ("b1_matmul.aic", "c/b1_matmul.c", "512", "64"),
    "B1-sieve": ("b1_sieve.aic", "c/b1_sieve.c", "100000000", "1000000"),
    # B2 解析：JSON 子集递归下降（输入 = harness 生成的约 1MB 文件；两侧都读文件）
    "B2-json": ("b2_json.aic", "c/b2_json.c", "16000", "2000"),
    # B4 分配：百万节点对象图（构建 + 遍历 + 整块回收 vs 逐节点 free）
    "B4-graph": ("b4_graph.aic", "c/b4_graph.c", "1000000", "100000"),
}


def prepare_input(name: str, times: str) -> str | None:
    """带输入的 bench：生成确定性输入文件并返回其路径（两侧读同一文件，保证公平）。"""
    if name != "B2-json":
        return None
    import json

    unit = {"a": 1, "b": [2, 3, {"c": 4}], "d": True, "e": None, "f": "xyz"}
    items = [json.dumps(unit, separators=(",", ":")) for _ in range(int(times))]
    doc = "[" + ",".join(items) + ",null]"
    path = WORK / "bench_json_input.json"
    path.write_text(doc, encoding="utf-8")
    return str(path)


def find_cc() -> str:
    for name in ("gcc", "clang"):
        for d in os.environ.get("PATH", "").split(os.pathsep):
            p = Path(d) / (name + ".exe")
            if p.is_file():
                return str(p)
            p2 = Path(d) / name
            if p2.is_file():
                return str(p2)
    return "gcc"


def outputs_equal(a: str, b: str) -> bool:
    """数值等价判定：AIC 浮点打印走最短往返、C 基线用 %.6f，字符串必然不同；
    逐行按数值比较（整数直接比，浮点允许 1e-6 相对误差）。"""
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


def run_once(exe: Path, arg: str) -> tuple[float, str]:
    t0 = time.perf_counter()
    p = subprocess.run([str(exe), arg], capture_output=True)
    dt = time.perf_counter() - t0
    return dt, p.stdout.decode("utf-8", "replace").strip()


def main() -> int:
    quick = "--quick" in sys.argv
    record = "--record" in sys.argv
    WORK.mkdir(parents=True, exist_ok=True)
    cc = find_cc()
    results: dict[str, dict[str, float]] = {}
    print(f"性能验收（{'quick' if quick else 'default'} 规模，C 基线 = {Path(cc).name} -O2）")
    print(f"{'bench':<12} {'AIC(ms)':>10} {'C -O2(ms)':>10} {'AIC/C':>7}  {'输出一致':>8}")
    for name, (aic_src, c_src, dflt, qk) in CASES.items():
        arg = qk if quick else dflt
        prepared = prepare_input(name, arg)
        if prepared:
            arg = prepared
        aic_exe = WORK / f"{name}-aic.exe"
        c_exe = WORK / f"{name}-c.exe"
        b = subprocess.run([str(AIC), "build", str((BENCH / aic_src).relative_to(ROOT)),
                            "-o", str(aic_exe)], cwd=str(ROOT), capture_output=True)
        if b.returncode != 0:
            print(f"{name:<12} AIC 构建失败：{b.stderr.decode('utf-8', 'replace')[:120]}")
            continue
        b2 = subprocess.run([cc, "-std=c11", "-O2", str(BENCH / c_src), "-o", str(c_exe)],
                            capture_output=True)
        if b2.returncode != 0:
            print(f"{name:<12} C 基线构建失败：{b2.stderr.decode('utf-8', 'replace')[:120]}")
            continue
        aic_times, c_times = [], []
        aic_out = c_out = ""
        # 交替测量（A/C 配对，消除机器状态漂移）；各 5 次
        for _ in range(7):
            dt, out = run_once(aic_exe, arg)
            aic_times.append(dt)
            aic_out = out
            dt2, out2 = run_once(c_exe, arg)
            c_times.append(dt2)
            c_out = out2
        # min = 基准测试的稳健估计（噪声只会让时间变长）；median 一并报出
        am, cm = min(aic_times), min(c_times)
        am_med, cm_med = statistics.median(aic_times), statistics.median(c_times)
        same = "yes" if outputs_equal(aic_out, c_out) else f"NO ({aic_out[:12]} vs {c_out[:12]})"
        ratio = (am / cm) if cm > 0 else float("inf")
        ratio_med = (am_med / cm_med) if cm_med > 0 else float("inf")
        print(f"{name:<12} {am * 1000:>10.1f} {cm * 1000:>10.1f} {ratio:>7.2f}  {same:>8}"
              f"   (median AIC {am_med * 1000:.1f} / C {cm_med * 1000:.1f} → ratio_med {ratio_med:.2f})")
        results[name] = {"aic_ms": round(am * 1000, 3), "c_ms": round(cm * 1000, 3),
                         "aic_median_ms": round(am_med * 1000, 3),
                         "c_median_ms": round(cm_med * 1000, 3),
                         "ratio": round(ratio, 4), "ratio_med": round(ratio_med, 4), "arg": arg,
                         "aic_size": aic_exe.stat().st_size, "c_size": c_exe.stat().st_size}
    if not results:
        return 1
    if record:
        BASELINE.write_text(json.dumps({"cases": results}, indent=2) + "\n", encoding="utf-8")
        print(f"基线已记录：{BASELINE.relative_to(ROOT)}")
    elif BASELINE.is_file():
        base = json.loads(BASELINE.read_text(encoding="utf-8")).get("cases", {})
        for name, r in results.items():
            b0 = base.get(name)
            if not b0:
                continue
            delta = (r["ratio"] - b0["ratio"]) / b0["ratio"] * 100 if b0["ratio"] else 0.0
            flag = "regress" if delta > 15 else "ok"
            print(f"  vs 基线 {name}: ratio {b0['ratio']:.2f} → {r['ratio']:.2f} ({delta:+.1f}% {flag})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
