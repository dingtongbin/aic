#!/usr/bin/env python3
"""AIC 门禁骨架（scripts/aici_gates.py）—— 规划方资产（黄金执行规划 §四）。

本模块 = 门禁的公共语言：GateResult 三判定、进程与编译助手、emit 就位探测。
具体门禁（H1–H7 与 #20）在 aici_gates_h.py；本模块不打印、不退出。
"""

from __future__ import annotations

import sys
from dataclasses import dataclass, field
from enum import Enum
from pathlib import Path

from aici_exec import (
    BIN,
    ROOT,
    RUNTIME,
    CBackend,
    rel,
    run,
)

AIC = BIN / "aic.exe"
CHECK_BUDGET_MS = 100
H1_BUDGET_MS = CHECK_BUDGET_MS


class Verdict(Enum):
    PASS = "PASS"
    FAIL = "FAIL"
    SKIP = "SKIP"  # 合法 SKIP 仅限 §四.3 白名单：功能尚未就位


@dataclass
class GateResult:
    gate: str
    verdict: Verdict
    detail: str = ""
    notes: list[str] = field(default_factory=list)
    units: int = 0


def skip(gate: str, why: str) -> GateResult:
    return GateResult(gate, Verdict.SKIP, why)


def fail(gate: str, why: str, notes: list[str] | None = None) -> GateResult:
    return GateResult(gate, Verdict.FAIL, why, notes or [])


def ok(gate: str, detail: str, units: int = 0, notes: list[str] | None = None) -> GateResult:
    return GateResult(gate, Verdict.PASS, detail, notes or [], units)


# ---------------------------------------------------------------- 进程与编译

def aic_check(src: Path) -> RunResult:
    """`aic check <file>`（cwd = 仓库根，诊断路径 = 仓库根相对，R1c）。"""
    return run([AIC, "check", rel(src)], cwd=ROOT)


def require_aic() -> str | None:
    if not AIC.is_file():
        return "aic CLI 未构建（R3 前占位：CLI 交付即真实执行）"
    return None


def runtime_objects(be: CBackend, work: Path) -> tuple[bool, list[Path], str]:
    """把 L0/L1 的 C 落点编译成目标文件，供各产物链接。

    L1 是头文件内联实现，编译单元只有 aic_l0.c 与 aic_std.c；按需链接靠
    链接器丢弃未引用段，L2（调度层）根本不参与（红线 16）。
    """
    # aic_l2.c 一并编成目标文件：不引用 L2 的程序由链接器整段丢弃（红线 16 判产物），
    # 用到 spawn/scope 的程序则必须能链接上（emit 会按需 #include "aic_l2.h"）。
    sources = [RUNTIME / "aic_l0.c", RUNTIME / "aic_std.c", RUNTIME / "aic_l2.c"]
    objs: list[Path] = []
    for src in sources:
        obj = work / f"{be.name}-{src.stem}.o"
        cmd: list[str | Path] = [be.exe, *be.flags, "-std=c11"]
        if not be.is_tcc:
            cmd.append("-finput-charset=UTF-8")
        cmd += ["-I", RUNTIME, "-c", src, "-o", obj]
        r = run(cmd, cwd=ROOT, env=be.env)
        if not r.ok:
            return False, [], f"{be.name} 编译 {src.name} 失败:\n{r.stderr[:2000]}"
        objs.append(obj)
    return True, objs, ""


def link_program(be: CBackend, c_src: Path, objs: list[Path], out: Path) -> RunResult:
    cmd: list[str | Path] = [be.exe, *be.flags, "-std=c11"]
    if not be.is_tcc:
        cmd.append("-finput-charset=UTF-8")
    cmd += ["-I", RUNTIME, c_src, *objs, "-o", out]
    if not be.is_tcc:
        cmd.append("-lm")  # tcc 的 Windows 版没有独立 libm（数学函数走 CRT）
        if sys.platform == "win32":
            # H2 产物层确定性：PE 头的 COFF TimeDateStamp 默认写当前时间，
            # 同一输入两次构建必然不同。MinGW ld 用该开关关掉（GNU ld/tcc 无此选项）。
            cmd.append("-Wl,--no-insert-timestamp")
    return run(cmd, cwd=ROOT, env=be.env)


def emit_c(src: Path, out_c: Path, extra: list[str] | None = None) -> RunResult:
    """`aic build --emit-c <src> -o <out.c>`：只出 C，不调 C 编译器。

    门禁自己掌握外部 C 编译器（红线 18：外部进程是唯一 C 边界），这样 H3 的
    「配置」才是脚本真正控制的四配置，而不是 CLI 内部某个不可见开关。
    """
    args: list[str | Path] = [AIC, "build", "--emit-c", rel(src), "-o", out_c]
    if extra:
        args.extend(extra)
    return run(args, cwd=ROOT)


def emit_ready() -> str | None:
    """emit/CLI 是否已交付；未交付时返回 SKIP 理由（§四.2 就位/缺席规则）。

    区分「功能尚未就位」（合法 SKIP）与「功能就位但坏了」（FAIL）：
    CLI 连 build 子命令都不认 = 未就位；一旦认了，任何失败都必须判 FAIL，
    否则门禁可以靠坏掉的 emit 一直保持绿色。
    """
    why = require_aic()
    if why:
        return why
    usage = run([AIC, "--help"], cwd=ROOT)
    text = usage.stdout + usage.stderr
    if "build" not in text:
        return "aic CLI 尚无 build 子命令（emit 未交付；R3 主体交付即真实执行）"
    return None


# 门禁本体在 aici_gates_h.py（H1–H7 与 #20）；此处不留转发，避免两处各写一份。
