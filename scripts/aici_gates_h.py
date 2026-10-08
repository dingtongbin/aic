#!/usr/bin/env python3
"""AIC 硬约束门禁 H1-H7 与 #20（scripts/aici_gates_h.py）—— 规划方资产。

一个门禁一个函数，返回 GateResult（真实跑 / 合法 SKIP / 失败 三判定之一，
黄金执行规划 §八 验收机）。本模块不打印、不退出，便于 scripts/gate_selftest.py
用合成输入把每条门禁的判负能力钉住（门禁自身也要被验证：§二 W2「验而不真」）。

  H1  单文件 check <=100ms  gate_h1
  H2  构建确定性            gate_h2（emit 文本两次逐字节 + 产物 sha256 两次）
  H3  配置输出逐位一致       gate_h3（tcc/gcc-O0/gcc-O2/clang-O2 + trap 用例）
  H4  UBSan 常开            gate_h4（clang 配置固定开启，UBSan 报告即失败）
  H5  报错三段式            gate_h5（.err 快照逐位 + 形状校验）
  H6  无隐式转换            gate_h6（索引位例外锚点必须真跑）
  H7  表面冻结              gate_h7（23 关键字 + 5 注解字面量表）
  #20 禁 cgo                gate_no_cgo
"""

from __future__ import annotations

import hashlib
import shutil
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

from aici_exec import (
    ROOT,
    CBackend,
    backends,
    corpus,
    ir_only_count,
    is_ir_only,
    runnable_corpus,
    pkg_corpus,
    pkg_err_corpus,
    find_tool,
    read_text,
    rel,
    run,
)
from aici_gates import (
    AIC,
    H1_BUDGET_MS,
    GateResult,
    aic_check,
    emit_c,
    emit_ready,
    fail,
    link_program,
    ok,
    require_aic,
    runtime_objects,
    skip,
)
from aici_shape import normalize_emit, three_part_shape


# ------------------------------------------------------------------- H1 计时

def gate_h1(budget_ms: int = H1_BUDGET_MS) -> GateResult:
    why = require_aic()
    if why:
        return skip("H1", why)
    files = runnable_corpus("ok") + runnable_corpus("trap") + pkg_corpus()
    if not files:
        return skip("H1", "ok/trap/pkg 语料为空")
    # 预热一次：首个文件承载进程/磁盘冷启动开销，不代表 check 热路径（R1e）
    warm = aic_check(files[0])
    if not warm.ok:
        return fail("H1", f"预热：{rel(files[0])} check 失败")
    notes: list[str] = []
    worst = (0, "")
    for f in files:
        t0 = time.perf_counter()
        r = aic_check(f)
        ms = (time.perf_counter() - t0) * 1000
        if not r.ok:
            return fail("H1", f"{rel(f)} check 失败：{r.stderr.strip()[:400]}")
        if ms > worst[0]:
            worst = (ms, f.name)
        if ms > budget_ms:
            return fail("H1", f"{rel(f)} 耗时 {ms:.1f}ms > {budget_ms}ms", notes)
        notes.append(f"H1: {f.name} {ms:.0f}ms")
    return ok(
        "H1",
        f"{len(files)} 个可编译语料全部 ≤{budget_ms}ms（最慢 {worst[1]} {worst[0]:.0f}ms）",
        len(files),
        notes,
    )


# --------------------------------------------------------------- H2 确定性

def sha256_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def gate_h2(work: Path | None = None) -> GateResult:
    why = emit_ready()
    if why:
        return skip("H2", why)
    files = runnable_corpus("ok") + runnable_corpus("trap") + pkg_corpus()
    if not files:
        return skip("H2", "ok/trap/pkg 语料为空")

    tmp = Path(work) if work else Path(tempfile.mkdtemp(prefix="aic-h2-"))
    made_tmp = work is None
    try:
        notes: list[str] = []
        # 第一层：同一输入两次 emit，文本逐字节一致（emit 纯函数，红线 12）
        for f in files:
            a_dir = tmp / f"{f.stem}-a"
            b_dir = tmp / f"{f.stem}-b"
            a_dir.mkdir(parents=True, exist_ok=True)
            b_dir.mkdir(parents=True, exist_ok=True)
            a_c, b_c = a_dir / "out.c", b_dir / "out.c"
            ra = emit_c(f, a_c)
            rb = emit_c(f, b_c)
            if not ra.ok:
                return fail("H2", f"{rel(f)} 首次 emit 失败：{ra.stderr.strip()[:400]}", notes)
            if not rb.ok:
                return fail("H2", f"{rel(f)} 二次 emit 失败：{rb.stderr.strip()[:400]}", notes)
            ta, tb = normalize_emit(read_text(a_c)), normalize_emit(read_text(b_c))
            if ta != tb:
                first = next(
                    (i for i, (x, y) in enumerate(zip(ta.split("\n"), tb.split("\n"))) if x != y),
                    0,
                )
                return fail(
                    "H2",
                    f"{rel(f)} 两次 emit 文本不一致（emit 非纯函数？红线 12），首处差异行 {first + 1}",
                    notes,
                )
            notes.append(f"H2: {f.name} emit 文本 sha256={hashlib.sha256(ta.encode()).hexdigest()[:16]}")
        # 第二层：同一配置两次构建 → 产物 sha256 比对（黄金执行规划 §四.1）
        be_list, _ = backends()
        gcc = next((b for b in be_list if b.name == "gcc-O2"), None)
        if gcc is None:
            notes.append("H2: 产物层跳过——gcc 不可用")
            return ok("H2", f"{len(files)} 个语料 emit 文本两次逐字节一致（产物层：gcc 缺失）", len(files), notes)
        probe = runnable_corpus("ok")[0] if runnable_corpus("ok") else files[0]
        hashes: list[str] = []
        for tag in ("a", "b"):
            d = tmp / f"bin-{tag}"
            d.mkdir(parents=True, exist_ok=True)
            c_out = d / "out.c"
            r = emit_c(probe, c_out)
            if not r.ok:
                return fail("H2", f"{rel(probe)} emit 失败：{r.stderr.strip()[:400]}", notes)
            built, objs, err = runtime_objects(gcc, d)
            if not built:
                return fail("H2", err, notes)
            exe = d / "prog.exe"
            lr = link_program(gcc, c_out, objs, exe)
            if not lr.ok:
                return fail("H2", f"{rel(probe)} 链接失败：{lr.stderr[:1000]}", notes)
            hashes.append(sha256_file(exe))
        if hashes[0] != hashes[1]:
            return fail(
                "H2",
                f"{rel(probe)} 两次构建 sha256 不同（{hashes[0][:16]} vs {hashes[1][:16]}）",
                notes,
            )
        notes.append(f"H2: 产物 sha256={hashes[0][:16]}（两次一致）")
        return ok(
            "H2",
            f"{len(files)} 个语料 emit 文本两次逐字节一致 + 产物 sha256 两次一致",
            len(files),
            notes,
        )
    finally:
        if made_tmp:
            shutil.rmtree(tmp, ignore_errors=True)


# ----------------------------------------------------------- H3 四配置一致

@dataclass
class H3Case:
    src: Path
    kind: str  # ok | trap
    expect_exit_zero: bool
    snapshot: str  # .out 文本（ok）或 .trap 前缀（trap）


def h3_cases() -> tuple[list[H3Case], list[str]]:
    cases: list[H3Case] = []
    problems: list[str] = []
    for f in runnable_corpus("ok"):
        snap = f.with_suffix(".out")
        if not snap.is_file():
            problems.append(f"{rel(f)} 缺 .out 快照（§六：golden 同名 .out）")
            continue
        cases.append(H3Case(f, "ok", True, read_text(snap)))
    # trap 语料同样可能带 `aic:ir-only`（如 select/闭包等只在 IR 路径实现的语法）：
    # 它们由 H8 覆盖（IR verifier + 两次 dump 一致），不参与 C 编译/运行门禁。
    for f in runnable_corpus("trap"):
        snap = f.with_suffix(".trap")
        if not snap.is_file():
            problems.append(f"{rel(f)} 缺 .trap 期望（§六）")
            continue
        cases.append(H3Case(f, "trap", False, read_text(snap).strip()))
    # 多包工程（红线 23）：入口 + main.out 快照，四配置逐位一致照跑。
    for f in pkg_corpus():
        snap = f.with_suffix(".out")
        if not snap.is_file():
            problems.append(f"{rel(f)} 缺 main.out 快照（§六：golden 同名 .out）")
            continue
        cases.append(H3Case(f, "ok", True, read_text(snap)))
    return cases, problems


def _run_case(
    be: CBackend,
    case: H3Case,
    work: Path,
    objs: list[Path],
    emit=emit_c,
) -> tuple[bool, str]:
    """一个配置跑一个用例；返回 (是否与期望相符, 说明)。

    emit 可注入（默认走 aic CLI）：门禁自测用预置 C 源替换 emit，从而在
    没有 emit 的情况下也能钉住 H3/H4 的比对与判负路径（§二 W2）。
    """
    name = f"{case.src.stem}-{be.name}"
    c_out = work / f"{name}.c"
    exe = work / f"{name}.exe"
    r = emit(case.src, c_out)
    if not r.ok:
        return False, f"{rel(case.src)} [{be.name}] emit 失败：{r.stderr.strip()[:300]}"
    lr = link_program(be, c_out, objs, exe)
    if not lr.ok:
        return False, f"{rel(case.src)} [{be.name}] 编译链接失败：{lr.stderr[:600]}"
    rr = run([exe], cwd=work, env=be.env)
    out = rr.stdout.replace("\r\n", "\n")
    err_text = rr.stderr  # RunResult 已按 UTF-8 解码（坏字节不抛）
    if case.expect_exit_zero:
        if rr.code != 0:
            return False, f"{rel(case.src)} [{be.name}] 退出码 {rr.code}（期望 0）\n{err_text[:600]}"
        if be.ubsan and err_text.strip():
            return False, f"{rel(case.src)} [{be.name}] UBSan 报告：\n{err_text[:600]}"
        # .out 快照是「程序应打印的内容」，末行换行不承载语义（§六 逐位比对
        # 的粒度 = 行），故两侧仅在末尾换行上归一，中间任何差异都算不符。
        if out.rstrip("\n") != case.snapshot.rstrip("\n"):
            return False, (
                f"{rel(case.src)} [{be.name}] stdout 与 .out 不符\n"
                f"--- 实际 ---\n{out}\n--- 期望 ---\n{case.snapshot}"
            )
        return True, out
    # trap 用例：退出码非 0 + 消息前缀 + 无 UBSan 报告（黄金执行规划 §六）
    if rr.code == 0:
        return False, f"{rel(case.src)} [{be.name}] 未 trap（退出码 0）"
    if case.snapshot and case.snapshot not in err_text:
        return False, (
            f"{rel(case.src)} [{be.name}] trap 消息前缀未命中\n"
            f"期望前缀: {case.snapshot}\n实际: {err_text[:400]!r}"
        )
    low = err_text.lower()
    if "runtime error" in low or "undefinedbehaviour" in low or "ubsan" in low:
        return False, f"{rel(case.src)} [{be.name}] UBSan 报告：\n{err_text[:600]}"
    return True, err_text


def gate_h3(work: Path, *, kinds: tuple[str, ...] = ("ok", "trap")) -> GateResult:
    why = emit_ready()
    if why:
        return skip("H3", why)
    be_list, notes = backends()
    if not be_list:
        return fail("H3", "无任何可用 C 编译器（黄金执行规划 §十一 D5–D7）")
    cases, problems = h3_cases()
    cases = [c for c in cases if c.kind in kinds]
    if problems:
        return fail("H3", "；".join(problems))
    if not cases:
        return skip("H3", "无语料可运行")
    if len(be_list) == 1:
        notes.append("只有一个配置可用：无法做「逐位一致」比对，只做期望比对")

    obj_cache: dict[str, tuple[bool, list[Path], str]] = {}
    for be in be_list:
        good, objs, err = runtime_objects(be, work)
        if not good:
            return fail("H3", err, notes)
        obj_cache[be.name] = (good, objs, err)

    checked = 0
    for case in cases:
        outs: dict[str, str] = {}
        for be in be_list:
            good, objs, err = obj_cache[be.name]
            if not good:  # pragma: no cover - 上面已判
                return fail("H3", err, notes)
            passed, detail = _run_case(be, case, work, objs)
            if not passed:
                return fail("H3", detail, notes)
            if case.expect_exit_zero:
                outs[be.name] = detail
            else:
                notes.append(f"H3: {case.src.name} [{be.name}] trap 退出码非 0，前缀命中")
            checked += 1
        if len(outs) > 1:
            first_name = next(iter(outs))
            base = outs[first_name]
            for name, text in outs.items():
                if text != base:
                    return fail(
                        "H3",
                        f"{rel(case.src)} stdout 跨配置不一致：{first_name} vs {name}",
                        notes,
                    )
        if case.expect_exit_zero:
            notes.append(f"H3: {case.src.name} {len(be_list)} 配置 stdout 逐位一致")

    ubsan_note = "（含 clang-UBSan 配置）" if any(b.ubsan for b in be_list) else ""
    return ok(
        "H3",
        f"{len(cases)} 个语料 × {len(be_list)} 配置{ubsan_note} 运行输出逐位一致",
        checked,
        notes,
    )


# ------------------------------------------------------------------- H4 UBSan

def gate_h4(work: Path, *, h3_result: GateResult | None = None) -> GateResult:
    clang = find_tool("clang")
    if not clang:
        return skip("H4", "clang 缺失（UBSan 唯一落点）；环境故障不静默放宽")
    why = emit_ready()
    if why:
        return skip("H4", why)
    be_list, _ = backends()
    ubsan = next((b for b in be_list if b.ubsan), None)
    if ubsan is None:  # pragma: no cover - clang 在场即会构造出来
        return fail("H4", "clang 在场但 UBSan 配置未构造")
    if "-fsanitize=undefined" not in ubsan.flags or "-fno-sanitize-recover=all" not in ubsan.flags:
        return fail("H4", f"UBSan 配置缺固定 flag：{ubsan.flags}")
    good, objs, err = runtime_objects(ubsan, work)
    if not good:
        return fail("H4", err)
    cases, problems = h3_cases()
    if problems:
        return fail("H4", "；".join(problems))
    notes = ["H4: clang 配置固定 -fsanitize=undefined -fno-sanitize-recover=all"]
    passes = 0
    for case in cases:
        passed, detail = _run_case(ubsan, case, work, objs)
        if not passed:
            return fail("H4", detail, notes)
        passes += 1
    notes.append(f"H4: {passes} 个语料在 UBSan 下零报告")
    return ok("H4", f"UBSan 常开，{passes} 个语料零报告（trap 用例亦无 UB）", passes, notes)


# ------------------------------------------------------------- H5 报错三段式

def gate_h5() -> GateResult:
    why = require_aic()
    if why:
        return skip("H5", why)
    files = corpus("err") + pkg_err_corpus()
    if not files:
        return skip("H5", "err/pkg 语料为空")
    notes: list[str] = []
    matched = 0
    compile_clean = 0
    for f in files:
        snap = f.with_suffix(".err")
        if not snap.is_file():
            return fail("H5", f"{rel(f)} 缺 .err 快照（红线 15：报错必须进快照）", notes)
        want = read_text(snap)
        r = aic_check(f)
        if r.ok:
            # 检查器层语料（解析干净）：R2 起应为 0，否则是语料或检查器回退
            compile_clean += 1
            if not want.strip():
                return fail("H5", f"{rel(f)} 解析干净且 .err 快照为空", notes)
            notes.append(f"H5: {f.name} 编译通过但 .err 在册（检查器层遗留）")
            continue
        actual = r.stderr.replace("\r\n", "\n")
        if actual.strip() != want.strip():
            return fail(
                "H5",
                f"{rel(f)} 报错快照不符\n--- 实际 ---\n{actual}\n--- 期望 ---\n{want}",
                notes,
            )
        shape = three_part_shape(actual)
        if shape:
            return fail("H5", f"{rel(f)} 报错不是三段式：{shape}", notes)
        matched += 1
    if compile_clean:
        return fail(
            "H5",
            f"{compile_clean} 条 err/ 语料编译通过（R2 起 req 全部命中，检查器层遗留=0）",
            notes,
        )
    return ok(
        "H5",
        f"{matched} 条 err/ 语料逐位命中 .err 快照且全部三段式；检查器层遗留 0",
        matched,
        notes,
    )

# --------------------------------------------------------------- H6 无隐式转换

def gate_h6() -> GateResult:
    why = require_aic()
    if why:
        return skip("H6", why)
    files = corpus("err")
    if not files:
        return skip("H6", "err/ 语料为空")
    # 语料锚点：非法隐式转换必须被拒（.err 的第一段描述点名规则）
    conv = [f for f in files if "conv" in f.name]
    if not conv:
        return fail("H6", "err/ 无可判定的隐式转换锚点（红线 1 无语料=证明而非验证）")
    notes: list[str] = []
    hit = 0
    for f in conv:
        r = aic_check(f)
        if r.ok:
            return fail("H6", f"{rel(f)} 隐式转换未被拒（红线 1 破坏）", notes)
        hit += 1
    # 例外位（红线 1 唯一例外：索引位与 for-in 计数位任意整型）——例外也要有锚点，
    # 否则「宁保守」会退化成把合法程序也拒掉，属于另一种门禁失效。
    exceptions = [f for f in runnable_corpus("ok") if "index" in f.name]
    if not exceptions:
        return fail("H6", "ok/ 无「索引位任意整型」例外锚点（红线 1 例外位缺证）")
    for f in exceptions:
        r = aic_check(f)
        if not r.ok:
            return fail(
                "H6",
                f"{rel(f)} 例外位（索引/for-in 计数位）被误拒：{r.stderr.strip()[:300]}",
                notes,
            )
        hit += 1
    notes.append(f"H6: {len(conv)} 条非法隐式转换锚点全部被拒")
    notes.append(
        "H6: 索引位/计数位例外锚点全部通过：" + ", ".join(rel(f) for f in exceptions)
    )
    return ok("H6", f"无隐式数值转换：{len(conv)} 拒 / {len(exceptions)} 例外通过", hit, notes)


# ------------------------------------------------------------- H7 表面冻结

def gate_h7() -> GateResult:
    if not (ROOT / "internal" / "lex" / "lex.go").is_file():
        return skip("H7", "词法器未交付")
    r = run(["go", "test", "./internal/lex", "-run", "TestFrozenSurface", "-count=1"], cwd=ROOT)
    if not r.ok:
        return fail("H7", "表面冻结测试失败：\n" + (r.stdout + r.stderr)[-1500:])
    return ok("H7", "23 关键字 + 5 注解字面量表通过", 1)


# ------------------------------------------------------------- #20 禁 cgo

def gate_no_cgo() -> GateResult:
    hits: list[str] = []
    for sub in ("internal", "cmd", "runtime", "std"):
        d = ROOT / sub
        if not d.is_dir():
            continue
        for p in d.rglob("*"):
            if not p.is_file():
                continue
            if p.suffix not in (".go", ".c", ".h"):
                continue
            text = read_text(p)
            for i, line in enumerate(text.split("\n"), 1):
                s = line.strip()
                if s.startswith('import "C"') or s.startswith("// #cgo") or s.startswith("#cgo"):
                    hits.append(f"{rel(p)}:{i}")
    if hits:
        return fail("#20", "发现 cgo 依赖（红线 18，全仓禁）：" + ", ".join(hits))
    return ok("#20", "禁 cgo：零命中（internal/cmd/runtime/std）", 0)
