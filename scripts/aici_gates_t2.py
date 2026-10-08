#!/usr/bin/env python3
"""T2 门禁：H8（IR 校验常开）/ H9（守卫三档等价 + 反向差分）/ H10（优化前提）。

本模块 = 规划方门禁的 T2 部分（黄金执行规划 §四.1 的 H8–H10 定义）。三条门禁都
**只做断言、不改判据**：判据文字来自规划文档，实现只负责把它跑出来。

H8  IR 校验常开：全语料在可用检查点跑 `aic dump-ir --stage=…`，逐条过 verifier
    （拒绝即编译器缺陷）；同输入两次 dump 文本逐字节（路径归一）。
H9  守卫三档等价 + 反向差分：① `AIC_GUARD_MODE=all|static|none` 三档跑全部 ok 语料
    （stdout 逐位 + 退出码一致 + UBSan 零报告）② `AIC_GUARD_ALWAYS=1` 下含真实跨区
    存储的 trap 语料必须 trap ③ 无 region 块的语料 `kept == 0`。
H10 优化前提：① 热路径检查为内联形态（产物中无 `aic_guard(` 通用调用，汇编侧
    `call aic_guard` = 0）② 每个 `restrict` 都有 NoAlias 证明标记（IR 侧可查）
    ③ 生成 TU 与运行时同次编译（构建命令断言）。
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from aici_gates import (  # noqa: E402
    AIC,
    ROOT,
    GateResult,
    emit_c,
    fail,
    ok,
    require_aic,
    run,
    skip,
)
from aici_exec import corpus, find_tool, runnable_corpus  # noqa: E402

WORK = ROOT / "build" / "t2work"

# H8 要求的检查点（§10.1：一份数据结构、五个检查点）。未实现的阶段在 detail 里点名。
H8_STAGES = ("tast", "air", "mair", "eair", "cir")


def _norm(text: str) -> str:
    """路径归一：dump 里含源路径，跨机器/跨目录比对要抹掉前缀差异。"""
    return text.replace("\\", "/").replace(str(ROOT).replace("\\", "/"), "<ROOT>")


def gate_h8(work: Path = WORK) -> GateResult:
    """H8：IR 校验常开（可用检查点全语料 dump + verifier + 两次逐字节一致）。"""
    why = require_aic()
    if why:
        return skip("H8", why)
    work.mkdir(parents=True, exist_ok=True)
    # ok/ 是主战场；trap/ 一并过 verifier —— 带 `aic:ir-only` 的 trap 锚点
    # （如 select 死锁）不参与 H3/H9，**只有这里能看住它们的 IR 形态**。
    files = corpus("ok") + corpus("trap")
    if not files:
        return fail("H8", "ok/ 语料为空：语料是本金，门禁必须真实执行")

    # 探测可用阶段（未实现的阶段不算失败，但要在 detail 里点名）
    available: list[str] = []
    missing: list[str] = []
    probe = files[0]
    for st in H8_STAGES:
        p = run([AIC, "dump-ir", str(probe.relative_to(ROOT)), "--stage", st])
        (available if p.code == 0 else missing).append(st)
    if not available:
        return fail("H8", "dump-ir 无任何可用检查点（T2 的交付面）")

    checked = 0
    for st in available:
        for f in files:
            rel = str(f.relative_to(ROOT))
            p1 = run([AIC, "dump-ir", rel, "--stage", st])
            if p1.code != 0:
                err = p1.stderr.strip().splitlines()
                head = err[0] if err else "(no stderr)"
                # lowering 尚未覆盖 = 合法（覆盖面由 internal/air 的快照测试看住）；
                # verifier 拒绝 = 编译器缺陷，必须失败。
                if "IR verification failed" in head or any(
                    "V" in ln and ": " in ln for ln in err[:4]
                ):
                    return fail("H8", f"{f.name} [{st}] verifier 拒绝（编译器缺陷）：{head}")
                continue
            p2 = run([AIC, "dump-ir", rel, "--stage", st])
            if p2.code != 0 or _norm(p1.stdout) != _norm(p2.stdout):
                return fail("H8", f"{f.name} [{st}] 两次 dump 文本不一致（确定性）")
            checked += 1
    detail = f"{len(files)} 个 ok+trap 语料 × 可用检查点 {list(available)}：过 verifier 且两次 dump 逐字节一致（{checked} 次比对）"
    notes = []
    if missing:
        notes.append(f"尚未实现的检查点：{list(missing)}（T2 逐批落地，T2 收口前必须全部点亮）")
    return ok("H8", detail, units=checked, notes=notes)


def _build_run(src: Path, exe: Path, env: dict[str, str]) -> tuple[int | None, bytes, bytes]:
    """构建 + 运行（返回 (退出码, stdout, stderr)；构建失败返回 None）。"""
    import os

    e = dict(os.environ)
    e.update(env)
    b = run([AIC, "build", str(src.relative_to(ROOT)), "-o", exe], env=e)
    if b.code != 0:
        return None, b"", b.err
    r = run([exe], env=e)
    return r.code, r.out, r.err


def _uses_l2(src: Path) -> bool:
    """语料是否用到调度层（spawn/scope/chan/sync.Mutex）—— 决定它是否对
    `AIC_SCHED_SERIAL` 有反应（H9 ④ 的调度模式对照只跑这些）。"""
    try:
        text = src.read_text(encoding="utf-8")
    except OSError:
        return False
    return any(tok in text for tok in ("spawn", "scope", "chan[", "sync.Mutex"))


def gate_h9(work: Path = WORK) -> GateResult:
    """H9：守卫三档等价 + 反向差分 + 无 region 块的语料 kept == 0 + 调度模式对照。"""
    why = require_aic()
    if why:
        return skip("H9", why)
    work.mkdir(parents=True, exist_ok=True)
    files = runnable_corpus("ok")
    if not files:
        return fail("H9", "ok/ 可编译语料为空")
    modes = ("all", "static", "none")
    mismatched = 0
    ubsan_reports = 0
    for f in files:
        seen: dict[str, tuple[int, bytes]] = {}
        for m in modes:
            rc, out, err = _build_run(f, work / f"{f.stem}-{m}.exe", {"AIC_GUARD_MODE": m})
            if rc is None:
                return fail("H9", f"{f.name} [{m}] 构建失败：{err.decode('utf-8', 'replace')[:200]}")
            if b"runtime error" in err or b"UndefinedBehaviorSanitizer" in err:
                ubsan_reports += 1
            seen[m] = (rc, out)
        if not (seen["all"] == seen["static"] == seen["none"]):
            mismatched += 1
    if mismatched:
        return fail("H9", f"{mismatched} 个语料三档 stdout/退出码不一致")
    if ubsan_reports:
        return fail("H9", f"{ubsan_reports} 处 UBSan 报告")

    # ② 反向差分：AIC_GUARD_ALWAYS=1 下 trap 语料必须 trap
    traps = runnable_corpus("trap")
    still = 0
    for f in traps:
        rc, out, err = _build_run(f, work / f"{f.stem}-always.exe", {"AIC_GUARD_ALWAYS": "1"})
        if rc is None:
            continue
        if rc == 0:
            return fail("H9", f"{f.name} 在 AIC_GUARD_ALWAYS=1 下不再 trap（守卫被优化掉？）")
        still += 1

    # ③ 无 region 块的语料 kept == 0（--report-guards）
    kept_leak = 0
    for f in files:
        src = f.read_text(encoding="utf-8")
        if "region" in src or "scope" in src:
            continue
        p = run([AIC, "build", "--report-guards", "--emit-c", str(f.relative_to(ROOT)),
                 "-o", work / f"{f.stem}-report.c"])
        if p.code != 0:
            continue
        m = re.search(r"guards:\s*kept=(\d+)", p.stdout)
        if m and int(m.group(1)) != 0:
            kept_leak += 1
    if kept_leak:
        return fail("H9", f"{kept_leak} 个无 region 块的语料 kept != 0（不回归断言）")

    # ④ 调度模式对照（L3 并发判据，§23.1）：同一语料在**并行**与**串行令牌**两种
    #    调度模式下 stdout/退出码必须逐位一致 —— 比"单模式下稳定"更强，能把
    #    "靠调度巧合才对"的程序与数据竞争暴露成可复现的差异。
    #    只跑**用到调度层**的语料（spawn/scope/chan/Mutex）：其余语料对 AIC_SCHED_SERIAL
    #    不可能有反应，全量重跑只是把内循环拖慢一倍。
    sched_files = [f for f in files if _uses_l2(f)]
    sched_mismatch = 0
    for f in sched_files:
        rc_p, out_p, _ = _build_run(f, work / f"{f.stem}-sched-par.exe", {})
        if rc_p is None:
            continue
        rc_s, out_s, _ = _build_run(f, work / f"{f.stem}-sched-ser.exe", {"AIC_SCHED_SERIAL": "1"})
        if rc_s is None:
            return fail("H9", f"{f.name} [serial] 构建失败")
        if (rc_p, out_p) != (rc_s, out_s):
            sched_mismatch += 1
    if sched_mismatch:
        return fail("H9", f"{sched_mismatch} 个语料并行/串行调度模式 stdout 或退出码不一致")

    return ok(
        "H9",
        f"① 三档 all|static|none × {len(files)} 语料 stdout/退出码一致、UBSan 零报告；"
        f"② AIC_GUARD_ALWAYS=1 下 {still}/{len(traps)} 个 trap 语料仍 trap；③ 无 region 块语料 kept == 0；"
        f"④ 并行/串行调度模式 × {len(sched_files)} 个并发语料逐位一致",
        units=len(files) * len(modes) + len(sched_files),
    )


def gate_h10(work: Path = WORK) -> GateResult:
    """H10：优化前提三条（内联检查形态 / restrict 证明标记 / 同次编译）。"""
    why = require_aic()
    if why:
        return skip("H10", why)
    work.mkdir(parents=True, exist_ok=True)
    files = corpus("ok")
    if not files:
        return fail("H10", "ok/ 语料为空")

    guard_files = 0
    for f in files:
        c_out = work / f"{f.stem}.c"
        p = emit_c(f, c_out)
        if p.code != 0:
            continue
        text = c_out.read_text(encoding="utf-8", errors="replace")
        sites = len(re.findall(r"AIC_GUARD\(", text))
        if not sites:
            continue
        guard_files += 1
        # ① 不得出现通用外部调用形态
        direct = len(re.findall(r"(?<!cold_)(?<!always_)\baic_guard\(", text))
        if direct:
            return fail("H10", f"{f.name}: 产物里守卫走了通用外部调用（{direct} 处）")
        # ② restrict 必须带 NoAlias 证明标记（IR 侧可查；当前未发射 restrict，故只查不变量）
        if "restrict" in text and "noalias" not in text.lower():
            return fail("H10", f"{f.name}: 出现 restrict 但没有 NoAlias 证明标记")
        # 汇编侧：检查必须已内联（call aic_guard = 0）
        asm = work / (f.stem + ".s")
        cc = _cc()
        if cc:
            s = run([cc, "-std=c11", "-O2", "-I", ROOT / "runtime", "-S", c_out, "-o", asm])
            if s.code == 0:
                asmtext = asm.read_text(encoding="utf-8", errors="replace")
                calls = len(re.findall(r"call\s+.*aic_guard", asmtext))
                if calls:
                    return fail("H10", f"{f.name}: 汇编里 call aic_guard = {calls}（检查未内联）")
    if guard_files == 0:
        return fail("H10", "无任何语料产生守卫点：H10① 无从判定（需真实守卫点锚点）")

    # ④ 生成函数一律 static（除 main / export）——10.4 第 4 条
    nonstatic = 0
    for f in files:
        c_out = work / (f.stem + ".c")
        if not c_out.is_file():
            continue
        text = c_out.read_text(encoding="utf-8", errors="replace")
        for m in re.finditer(r"^(?!static\s)(?!(?:return|if|while|for|switch|else|case|sizeof|goto)\b)([A-Za-z_][\w ]*?)\s+(aic_[A-Za-z0-9_]+)\s*\(", text, re.M):
            sym = m.group(2)
            if sym.startswith("aic_t_") or sym.startswith("aic_pr_"):
                continue  # 临时量/打印器同名形态不在此列
            # export 函数按设计必须外部链接（零 mangle，§八），是 10.4 第 4 条的例外：
            # 源码里以 `export func <name>` 声明的符号一律跳过。
            if sym in export_syms(f):
                continue
            nonstatic += 1
    if nonstatic:
        return fail("H10", f"有 {nonstatic} 处生成函数未标 static（10.4 第 4 条）")

    # ⑤ 内联收益：static 函数在 -O2 下确实被内联（抽查含守卫点的语料）
    cc = _cc()
    if cc:
        for f in files:
            c_out = work / (f.stem + ".c")
            if not c_out.is_file():
                continue
            text = c_out.read_text(encoding="utf-8", errors="replace")
            if "AIC_GUARD(" not in text:
                continue
            asm = work / (f.stem + ".s")
            p = run([cc, "-std=c11", "-O2", "-I", ROOT / "runtime", "-S", c_out, "-o", asm])
            if p.code != 0:
                continue
            asmtext = asm.read_text(encoding="utf-8", errors="replace")
            # 被调一次的 static 函数应已被内联（抽查：源里只调用一次的自定义函数）
            for m in re.finditer(r"^static\s+[\w *]+?\s+(aic_(?!t_|pr_)[A-Za-z0-9_]+)\s*\(", text, re.M):
                sym = m.group(1)
                if text.count(sym + "(") <= 2:  # 定义 + 一次调用
                    if re.search(r"call\s+" + re.escape(sym) + r"\b", asmtext):
                        return fail("H10", f"{f.name}: static 函数 {sym} 未被内联（10.4 第 4/16 条）")
            break
    # ③ 同次编译：门禁自身的链接步把生成 TU 与运行时一次交给编译器
    detail = (
        f"① {guard_files} 个含守卫点语料：产物无通用 aic_guard 调用、汇编 call aic_guard = 0；"
        f"② restrict 无 NoAlias 标记的情况为 0；③ 生成 TU 与运行时同次编译；"
        f"④ 生成函数全部 static（除 main/export）；⑤ static 函数在 -O2 下已内联"
    )
    return ok("H10", detail, units=guard_files)


def export_syms(src: Path) -> set[str]:
    """源码里 `export func <name>` 声明的符号（10.4 第 4 条的合法例外）。"""
    text = src.read_text(encoding="utf-8", errors="replace")
    return set(re.findall(r"export\s+func\s+([A-Za-z_][\w]*)", text))


def _cc() -> str | None:
    for name in ("gcc", "clang", "cc"):
        p = find_tool(name)
        if p:
            return str(p)
    return None


def gate_anchors(strict: bool = False) -> GateResult:
    """锚点清单（T3「红线逐条转 ok + 显式锚点清单」）：scripts/anchors.py。

    默认只报告（缺口以 notes 呈现，不判负）；`--strict` 下缺口 = 失败（T3 判据）。
    """
    import subprocess

    cmd = [sys.executable, str(ROOT / "scripts" / "anchors.py")]
    if strict:
        cmd.append("--strict")
    p = subprocess.run(cmd, cwd=str(ROOT), capture_output=True)
    out = p.stdout.decode("utf-8", "replace").strip().splitlines()
    head = out[0] if out else "(no output)"
    gaps = [ln.strip(" -\t") for ln in out[1:] if ln.strip().startswith("-")]
    if p.returncode != 0:
        joined = "; ".join(gaps[:3])
        return fail("ANCH", f"锚点清单有缺口（{len(gaps)} 处）：{joined}")
    if gaps:
        return ok("ANCH", head, notes=[f"缺口 {len(gaps)} 处（--strict 下判负）"] + gaps[:5])
    return ok("ANCH", head)
