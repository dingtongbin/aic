#!/usr/bin/env python3
"""AIC 快速开发回路（scripts/t.py）—— 规划方资产（原 scripts/t.ps1 的 Python 等价物）。

为什么存在：go build / go vet / go test 各自都要做一次包加载与依赖分析，
三条串起来约 2.5~3.7s；而 go test 本身已经把所有包编译一遍，单独再跑
go build 是纯冗余。本脚本把内循环压到一次进程启动 + 一次 go 调用。

为什么改 Python：Windows PowerShell 5.1 读无 BOM 的 .ps1 时按 ANSI(GBK) 解码，
中文注释与输出必然乱码，且 `[Console]::OutputEncoding` 会把子进程输出重新编码
一次（中文双份乱码 + 计时噪声）。Python 全程显式 UTF-8，无 BOM 依赖。

用法：
  python scripts/t.py                       # 默认：gofmt 检查 + go test ./...（内循环用这个）
  python scripts/t.py --full                # 里程碑：gofmt -w + go vet + go test ./...（未缓存重跑）
  python scripts/t.py -run TestFoo          # 定点迭代：只跑匹配的测试
  python scripts/t.py --pkg ./internal/lex
  python scripts/t.py --pkg ./internal/lex --pkg ./internal/parse
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent


def _utf8_stdio() -> None:
    """子进程输出按 UTF-8 直通；避免 Windows 控制台默认代码页造成的乱码。"""
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, ValueError):  # pragma: no cover - 非文本流
            pass


def run(args: list[str]) -> int:
    proc = subprocess.run(args, cwd=ROOT)
    return proc.returncode


def gofmt_files() -> list[str]:
    """列出 gofmt 认为需要改动的文件（无则返回空表）。"""
    proc = subprocess.run(
        ["gofmt", "-l", "internal", "cmd"], cwd=ROOT, capture_output=True, text=True
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stderr)
        return []
    return [line for line in proc.stdout.splitlines() if line.strip()]


class Steps:
    def __init__(self) -> None:
        self.started = time.perf_counter()

    def step(self, name: str, fn) -> None:
        mark = time.perf_counter()
        code = fn()
        elapsed = int((time.perf_counter() - mark) * 1000)
        if code != 0:
            total = int((time.perf_counter() - self.started) * 1000)
            print(f"[t] FAIL in {name} (total {total}ms)", file=sys.stderr)
            raise SystemExit(1)
        print(f"[t] {name:<10} {elapsed:>6} ms")


def main() -> int:
    _utf8_stdio()
    ap = argparse.ArgumentParser(add_help=True, description="AIC 内循环（gofmt / vet / test）")
    ap.add_argument("--full", action="store_true", help="里程碑模式：gofmt -w + go vet + 未缓存 go test")
    ap.add_argument("-run", "--run", default="", help="go test -run 过滤表达式")
    ap.add_argument("--pkg", action="append", default=[], help="包路径（可重复；默认 ./...）")
    ns = ap.parse_args()

    if shutil.which("go") is None:
        print("[t] FAIL: 找不到 go（黄金执行规划 §十一 环境故障：立即暂停）", file=sys.stderr)
        return 1

    pkgs = ns.pkg or ["./..."]
    steps = Steps()

    if ns.full:
        def do_fmt() -> int:
            dirty = gofmt_files()
            if dirty:
                print("[t] gofmt -w 修正: " + ", ".join(dirty))
            return run(["gofmt", "-w", "internal", "cmd"])

        steps.step("gofmt -w", do_fmt)
        steps.step("go vet", lambda: run(["go", "vet", "./..."]))
        test_args = ["go", "test", *pkgs, "-count=1"]
    else:
        def do_fmt_check() -> int:
            dirty = gofmt_files()
            if dirty:
                print("[t] gofmt 未格式化: " + ", ".join(dirty), file=sys.stderr)
                return 1
            return 0

        steps.step("gofmt -l", do_fmt_check)
        test_args = ["go", "test", *pkgs]

    if ns.run:
        test_args += ["-run", ns.run]

    steps.step("test", lambda: run(test_args))
    total = int((time.perf_counter() - steps.started) * 1000)
    print(f"[t] PASS total {total}ms")
    return 0


if __name__ == "__main__":
    if sys.version_info < (3, 10):
        sys.exit("scripts/t.py 需要 Python 3.10+")
    sys.exit(main())
