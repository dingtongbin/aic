#!/usr/bin/env python3
"""T1 执行工具：ok/ 语料端到端分诊。

用途：把「全部语法可编可跑」的差距按**可修复的类别**列出来，而不是只看一个总数。
类别（互斥，按流水线顺序短路）：
  NO-SNAPSHOT  缺 .out 快照（T1 必须补齐，且不得用编译器输出回填）
  CHECK-FAIL   `aic check` 失败（检查器/解析层）
  BUILD-FAIL   发射或 C 编译失败（emit 未覆盖该语法形态）
  RUN-EXIT     跑起来但退出码非 0
  STDOUT-DIFF  退出码 0 但 stdout 与 .out 不一致（语义错）
  PASS         全绿

用法（在 aic-src 根运行）：
  python scripts/triage.py                     汇总各类别计数 + 失败文件清单
  python scripts/triage.py --only 715          只看名字含 715 的
  python scripts/triage.py --cat BUILD-FAIL    只列该类别的明细（含 stderr 首行）
  python scripts/triage.py --all               每类都打明细
"""
from __future__ import annotations

import argparse
import os
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
OK_DIR = ROOT / "testdata" / "ok"
AIC = ROOT / "bin" / "aic.exe"
if not AIC.exists():
    AIC = ROOT / "bin" / "aic"

CATS = ["NO-SNAPSHOT", "IR-ONLY", "CHECK-FAIL", "BUILD-FAIL", "RUN-EXIT", "STDOUT-DIFF", "PASS"]
ORDER = {c: i for i, c in enumerate(CATS)}

# IR-ONLY 标记（语料首行注释 `// aic:ir-only`）：该语料用的是**只在 IR 路径实现**
# 的新语法（核心设计 §十七：直译路径冻结、不再加新特性）。T-A 期间它只要求
# `check` + `dump-ir --stage air` 通过；C 发射由 T-B 补，补完就删标记。
# 计数是路径切换的进度条：T-D 收口时这个数必须是 0。
IR_ONLY_MARK = "aic:ir-only"


def is_ir_only(aic_file: Path) -> bool:
    try:
        head = aic_file.read_text(encoding="utf-8", errors="replace")[:400]
    except OSError:
        return False
    return IR_ONLY_MARK in head


def run(cmd: list[str], cwd: Path | None = None, timeout: int = 60):
    return subprocess.run(
        cmd,
        cwd=str(cwd or ROOT),
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=timeout,
    )


def first_line(s: str) -> str:
    for line in s.splitlines():
        if line.strip():
            return line.strip()[:200]
    return ""


def check_one(aic_file: Path, tmp: Path) -> tuple[str, str]:
    """返回 (类别, 明细)。"""
    out_file = aic_file.with_suffix(".out")

    r = run([str(AIC), "check", str(aic_file)])
    if r.returncode != 0:
        return "CHECK-FAIL", first_line(r.stderr)

    # IR-ONLY 语料：check 必须过（上一步已经判了），C 发射留给 T-B
    # （`dump-ir --stage air` 的覆盖由 internal/air 的语料测试承担）。
    if is_ir_only(aic_file):
        return "IR-ONLY", "IR 路径新语法（直译路径已冻结）"

    if not out_file.exists():
        return "NO-SNAPSHOT", "缺 .out 快照"

    exe = tmp / (aic_file.stem + (".exe" if os.name == "nt" else ""))
    r = run([str(AIC), "build", str(aic_file), "-o", str(exe)])
    if r.returncode != 0:
        return "BUILD-FAIL", first_line(r.stderr) or first_line(r.stdout)

    try:
        r = run([str(exe)], timeout=30)
    except subprocess.TimeoutExpired:
        return "RUN-EXIT", "超时（可能死循环）"
    if r.returncode != 0:
        return "RUN-EXIT", f"退出码 {r.returncode}；stderr: {first_line(r.stderr)}"

    want = out_file.read_text(encoding="utf-8")
    got = r.stdout
    # 归一：只吃结尾换行差异（与门禁一致）
    if want.rstrip("\n") != got.rstrip("\n"):
        return "STDOUT-DIFF", f"want={want!r} got={got!r}"
    return "PASS", ""


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--only", default="", help="只跑文件名含该子串的语料")
    ap.add_argument("--cat", default="", help="只打印该类别的明细")
    ap.add_argument("--all", action="store_true", help="打印全部类别的明细")
    ap.add_argument("--quiet", action="store_true", help="只打印汇总")
    ap.add_argument("--out", default="", help="把报告写成 UTF-8 文件（避开 Windows 控制台 GBK 转码）")
    args = ap.parse_args()

    if not AIC.exists():
        print(f"缺少 CLI：{AIC}（先 go build -o bin/aic.exe ./cmd/aic）", file=sys.stderr)
        return 2

    files = sorted(OK_DIR.glob("*.aic"))
    if args.only:
        files = [f for f in files if args.only in f.name]
    if not files:
        print("无语料", file=sys.stderr)
        return 2

    results: dict[str, list[tuple[str, str]]] = {c: [] for c in CATS}
    with tempfile.TemporaryDirectory(prefix="aic-triage-") as td:
        tmp = Path(td)
        for f in files:
            cat, detail = check_one(f, tmp)
            results[cat].append((f.name, detail))

    total = len(files)
    lines: list[str] = []

    def emit(s: str = "") -> None:
        print(s)
        lines.append(s)

    emit(f"== ok/ 分诊：{total} 个语料 ==")
    for c in CATS:
        n = len(results[c])
        if n or c == "PASS":
            emit(f"{c:<12} {n:>3}  ({n * 100 // total}%)")
    emit(f"{'合计':<12} {total:>3}")

    show = set()
    if args.all:
        show = set(CATS)
    elif args.cat:
        show = {args.cat}
    if not args.quiet:
        for c in CATS:
            if c not in show or not results[c]:
                continue
            emit(f"\n--- {c} ---")
            for name, detail in results[c]:
                emit(f"  {name:<40} {detail}")
        if not show:
            emit("\n（加 --cat <类别> 或 --all 看明细）")

    if args.out:
        Path(args.out).write_text("\n".join(lines) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
