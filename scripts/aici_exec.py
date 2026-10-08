#!/usr/bin/env python3
"""AIC CI 公共设施（scripts/aici_exec.py）—— 规划方资产。

本模块只做「环境与进程」这一件事，不含任何门禁语义；门禁在 aici_gates.py，
唯一入口是 ci.py（黄金执行规划 §四：ci 是门禁权威入口，执行者只读）。

为什么整体改 Python：Windows PowerShell 5.1 读无 BOM 的 .ps1 时按 ANSI(GBK)
解码，中文注释与输出必然乱码；`[Console]::OutputEncoding` 又会把子进程输出
重新编码一次。Python 全程显式 UTF-8（源码无 BOM、读写显式 encoding、子进程
捕获原始字节后自行解码），与「含中文的 .ps1 须 UTF-8 with BOM」这条纪律解耦。
"""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TESTDATA = ROOT / "testdata"
RUNTIME = ROOT / "runtime"
BIN = ROOT / "bin"

# MSYS2 / LLVM / tcc 兜底探测目录（黄金执行规划 §十一 D5：clang 不在 PATH 时的落点；
# tcc 单文件安装目录 = D:\tools\tcc\tcc，装了它四配置才是真的四配置）
TOOL_DIRS = (
    Path(r"C:\msys64\clang64\bin"),
    Path(r"C:\msys64\ucrt64\bin"),
    Path(r"C:\msys64\mingw64\bin"),
    Path(r"C:\Program Files\LLVM\bin"),
    Path(r"D:\tools\tcc\tcc"),
)


def utf8_stdio() -> None:
    """stdout/stderr 按 UTF-8 直通，避免 Windows 控制台代码页乱码。"""
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8", errors="replace")
        except (AttributeError, ValueError):  # pragma: no cover - 非文本流
            pass


def decode(data: bytes) -> str:
    """子进程输出按 UTF-8 解码，坏字节不抛（门禁只比对文本）。"""
    return data.decode("utf-8", errors="replace")


def find_tool(name: str) -> Path | None:
    """PATH 优先，其次 MSYS2/LLVM 兜底目录（与 ci.ps1 的 Find-Tool 同序）。"""
    found = shutil.which(name)
    if found:
        return Path(found)
    for base in TOOL_DIRS:
        cand = base / (name + ".exe")
        if cand.is_file():
            return cand
        cand = base / name
        if cand.is_file():
            return cand
    return None


def tool_env(exe: Path) -> dict[str, str]:
    """把工具自身所在目录置于 PATH 首位。

    MSYS2 的 clang 需要同目录的 libclang_rt.ubsan_*.dll 才能加载 UBSan 运行时；
    只按绝对路径调用 exe 时，Windows 的 DLL 搜索不保证命中该目录（黄金执行规划
    §十一 D7：UBSan 是 H4 的唯一落点，不能因 DLL 找不到而静默跳过）。
    """
    env = dict(os.environ)
    env["PATH"] = str(exe.parent) + os.pathsep + env.get("PATH", "")
    return env


@dataclass
class RunResult:
    code: int
    out: bytes = b""
    err: bytes = b""

    @property
    def ok(self) -> bool:
        return self.code == 0

    @property
    def stdout(self) -> str:
        return decode(self.out)

    @property
    def stderr(self) -> str:
        return decode(self.err)


def run(
    args: list[str | Path],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    stdin: bytes | None = None,
) -> RunResult:
    """跑一个外部进程，捕获原始字节（全仓禁 cgo：外部进程是唯一 C 边界）。"""
    argv = [str(a) for a in args]
    proc = subprocess.run(
        argv,
        cwd=str(cwd or ROOT),
        env=env,
        input=stdin,
        capture_output=True,
        shell=False,
    )
    return RunResult(proc.returncode, proc.stdout, proc.stderr)


@dataclass
class CBackend:
    """一个 C 配置（黄金执行规划 §十一：tcc / gcc -O0 / gcc -O2 / clang -O2）。"""

    name: str
    exe: Path
    flags: list[str] = field(default_factory=list)
    ubsan: bool = False
    # tcc 不认 -lm（Windows 版没有独立 libm，数学函数走 CRT）与
    # -Wl,--no-insert-timestamp：硬塞会让整个配置直接失败。
    is_tcc: bool = False

    @property
    def env(self) -> dict[str, str]:
        return tool_env(self.exe)

    def compile_cmd(self, sources: list[Path], out: Path) -> list[str | Path]:
        args: list[str | Path] = [self.exe, *self.flags, "-std=c11"]
        if not self.is_tcc:
            args.append("-finput-charset=UTF-8")
        args += ["-I", RUNTIME, *(Path(s) for s in sources), "-o", out]
        if not self.is_tcc:
            args.append("-lm")
        return args


UBSAN_FLAGS = ["-fsanitize=undefined", "-fno-sanitize-recover=all"]


def backends() -> tuple[list[CBackend], list[str]]:
    """探测可用 C 配置；返回 (可用配置, 缺失说明)。

    tcc 未安装时降级为三配置并显式记录（黄金执行规划 §十一：缺失降级记录，
    不阻塞）；gcc 是环境基线，缺失由调用方判为环境故障。
    """
    found: list[CBackend] = []
    notes: list[str] = []

    gcc = find_tool("gcc")
    if gcc:
        found.append(CBackend("gcc-O0", gcc, ["-O0"]))
        found.append(CBackend("gcc-O2", gcc, ["-O2"]))
    else:
        notes.append("gcc 缺失（环境基线，黄金执行规划 §十一）")

    tcc = find_tool("tcc")
    if tcc:
        found.append(CBackend("tcc", tcc, is_tcc=True))
    else:
        notes.append("tcc 未安装 → 四配置降级为三配置（补齐即自动回四）")

    clang = find_tool("clang")
    if clang:
        found.append(CBackend("clang-O2", clang, ["-O2"]))
        found.append(CBackend("clang-UBSan", clang, ["-O2", *UBSAN_FLAGS], ubsan=True))
    else:
        notes.append("clang 缺失 → H4（UBSan 唯一落点）无法执行")
    return found, notes


# IR-ONLY 标记（与 triage.py 同源）：语料首行注释 `// aic:ir-only` 表示它用的是
# **只在 IR 路径实现**的新语法（核心设计 §十七：直译路径冻结）。这类语料在 T-A
# 阶段只要求 `check` + `dump-ir --stage air`（H8 覆盖），不参与 C 编译/运行门禁
# （H1–H4/H9）；T-B 的 IR→C 发射器接上后逐个删标记，**T-B 收口时该计数必须为 0**。
IR_ONLY_MARK = "aic:ir-only"


def is_ir_only(path: Path) -> bool:
    """报告语料是否带 IR-ONLY 标记（读首 400 字符，与 triage.py 同一判据）。"""
    try:
        head = path.read_text(encoding="utf-8", errors="replace")[:400]
    except OSError:
        return False
    return IR_ONLY_MARK in head


def corpus(kind: str) -> list[Path]:
    """列出一个语料目录下的 .aic，按文件名排序（确定性）。"""
    d = TESTDATA / kind
    if not d.is_dir():
        return []
    return sorted(p for p in d.glob("*.aic") if p.is_file())


def runnable_corpus(kind: str) -> list[Path]:
    """可编译/可运行的语料 = corpus(kind) 去掉 IR-ONLY 标记的那些。

    门禁**必须显式报告**被排除的数量（不能静默少测）：调用方用
    `ir_only_count()` 打出计数，T-B 收口时该数必须为 0。
    """
    return [p for p in corpus(kind) if not is_ir_only(p)]


def ir_only_count(kind: str = "ok") -> int:
    """带 IR-ONLY 标记的语料数（路径切换进度条：T-B 收口必须为 0）。"""
    return len([p for p in corpus(kind) if is_ir_only(p)])


def pkg_corpus() -> list[Path]:
    """多包工程语料（红线 23）：testdata/pkg/<prog>/main.aic 是入口。

    只收「有 main.out 快照」的工程——main.err 在册的是编译错工程（循环 import
    等），归 pkg_err_corpus()，不能混进 H1–H4 的可运行集合。
    """
    d = TESTDATA / "pkg"
    if not d.is_dir():
        return []
    return sorted(
        p
        for p in d.glob("*/main.aic")
        if p.is_file() and not p.with_suffix(".err").is_file()
    )


def pkg_err_corpus() -> list[Path]:
    """多包工程的编译错语料：入口 + 同名 .err 快照（三段式）。"""
    d = TESTDATA / "pkg"
    if not d.is_dir():
        return []
    return sorted(
        p
        for p in d.glob("*/main.aic")
        if p.is_file() and p.with_suffix(".err").is_file()
    )


def rel(path: Path) -> str:
    """仓库根相对路径，正斜杠（R1c：诊断路径一律相对仓库根）。"""
    try:
        return path.resolve().relative_to(ROOT.resolve()).as_posix()
    except ValueError:
        return path.as_posix()


def read_text(path: Path) -> str:
    """读文本快照：UTF-8，去掉 BOM，换行归一（.out/.err/.trap 都是冻结文本）。"""
    raw = path.read_bytes()
    if raw.startswith(b"\xef\xbb\xbf"):
        raw = raw[3:]
    return decode(raw).replace("\r\n", "\n").replace("\r", "\n")
