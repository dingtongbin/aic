#!/usr/bin/env python3
"""门禁自测（scripts/gate_selftest.py）—— 规划方资产。

为什么存在（黄金执行规划 §二 W2「验而不真」）：门禁本身也会坏——阈值写反、
比对松了、缺 `.out` 时静默跳过。本文件用合成输入把每条门禁的**判负能力**钉死：
好输入必须 PASS，坏输入必须 FAIL。门禁改坏了，这里先红。

H3/H4 的比对路径用预置 C 源替换 aic emit（`_run_case(emit=…)` 可注入），
所以门禁的判负能力在 emit 交付前就已可验证。

用法：python scripts/gate_selftest.py
"""

from __future__ import annotations

import shutil
import sys
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from aici_exec import RunResult, backends, utf8_stdio  # noqa: E402
from aici_gates import Verdict  # noqa: E402
from aici_gates_h import (  # noqa: E402
    H3Case,
    _run_case,
    gate_h1,
    gate_h2,
    gate_h5,
    runtime_objects,
)
from aici_shape import normalize_emit, three_part_shape  # noqa: E402

FAILURES: list[str] = []


def check(name: str, cond: bool, detail: str = "") -> None:
    if cond:
        print(f"[selftest] ok   {name}")
    else:
        print(f"[selftest] FAIL {name} {detail}")
        FAILURES.append(name)


# --------------------------------------------------------------- 纯函数断言

def test_three_part_shape() -> None:
    good = (
        "ok.aic:3:5: 错误：隐式数值转换\n"
        "    实际类型 i64，目标类型 i32\n"
        "    修复：写成 i32(x)\n"
    )
    bad_head = (
        "ok.aic:3:5 错误：少一个冒号\n"
        "    实际类型 i64\n"
        "    修复：写成 i32(x)\n"
    )
    bad_ctx = (
        "ok.aic:3:5: 错误：上下文没缩进\n"
        "实际类型 i64\n"
        "    修复：写成 i32(x)\n"
    )
    bad_fix = (
        "ok.aic:3:5: 错误：修复行不对\n"
        "    实际类型 i64\n"
        "  修复：缩进不足\n"
    )
    missing = "ok.aic:3:5: 错误：只有两段\n    实际类型 i64\n"
    two_errors = good + "\n" + good
    check("三段式：合法输入 PASS", three_part_shape(good) is None, str(three_part_shape(good)))
    check("三段式：两个错误仍合法（3 的倍数）", three_part_shape(two_errors) is None)
    check("三段式：缺位置判定为坏", three_part_shape(bad_head) is not None)
    check("三段式：上下文无缩进判定为坏", three_part_shape(bad_ctx) is not None)
    check("三段式：修复行判定为坏", three_part_shape(bad_fix) is not None)
    check("三段式：段数不足判定为坏", three_part_shape(missing) is not None)


def test_normalize_emit() -> None:
    a = '#line 3 "D:\\githubproject\\aic\\aic-src\\testdata\\ok\\001_hello.aic"\nint x = 1;\n'
    b = '#line 3 "/tmp/xyz/001_hello.aic"\nint x = 1;\n'
    c = '#line 3 "D:\\githubproject\\aic\\aic-src\\testdata\\ok\\001_hello.aic"\nint x = 2;\n'
    check("emit 归一：同一语义不同临时根归一后相等", normalize_emit(a) == normalize_emit(b))
    check("emit 归一：代码差异不被抹平", normalize_emit(a) != normalize_emit(c))


# ------------------------------------------------------------ H3 判负能力

GOOD_C = """#include "aic_l0.h"
int main(void) {
    aic_task_init("selftest.c", 1);
    aic_println_i32(7);
    return 0;
}
"""

SKEW_C = """#include "aic_l0.h"
int main(void) {
    aic_task_init("selftest.c", 1);
    aic_println_i32(8);   /* 故意与期望不同 */
    return 0;
}
"""

TRAP_C = """#include "aic_l0.h"
int main(void) {
    aic_task_init("selftest.c", 1);
    aic_trap(AIC_TRAP_DIVIDE_BY_ZERO, "selftest.c", 5);
}
"""

NO_TRAP_C = """#include "aic_l0.h"
int main(void) {
    aic_task_init("selftest.c", 1);
    return 0;   /* 期望 trap 却没 trap */
}
"""


def fixture(work: Path, tag: str, c_src: str) -> tuple[H3Case, Path]:
    """造一个 (用例, C 源) 对；case.aic 只是路径占位，C 源才是被编译的东西。"""
    d = work / tag
    d.mkdir(parents=True, exist_ok=True)
    (d / "case.aic").write_text("func main() {\n}\n", encoding="utf-8")
    c = d / "case.c"
    c.write_text(c_src, encoding="utf-8")
    return d, c


def stub_emit(c_src: Path):
    def _emit(_src: Path, out_c: Path):
        shutil.copyfile(c_src, out_c)
        return RunResult(0, b"", b"")

    return _emit


def test_h3_gate() -> None:
    be_list, _ = backends()
    gcc = next((b for b in be_list if b.name == "gcc-O0"), None)
    if gcc is None:
        print("[selftest] SKIP H3 判负：gcc 不可用（环境故障，非门禁结论）")
        return
    tmp = Path(tempfile.mkdtemp(prefix="aic-selftest-"))
    try:
        good, objs, err = runtime_objects(gcc, tmp)
        if not good:
            raise RuntimeError(err)

        def case_of(kind: str, snapshot: str) -> H3Case:
            return H3Case(tmp / "case.aic", kind, expect_exit_zero=(kind == "ok"), snapshot=snapshot)

        d_good, c_good = fixture(tmp, "good", GOOD_C)
        passed, detail = _run_case(gcc, case_of("ok", "7"), d_good, objs, stub_emit(c_good))
        check("H3：期望相符的产物判定 PASS", passed, detail)

        d_skew, c_skew = fixture(tmp, "skew", SKEW_C)
        passed, detail = _run_case(gcc, case_of("ok", "7"), d_skew, objs, stub_emit(c_skew))
        check("H3：stdout 不符判定 FAIL", not passed, detail)

        d_trap, c_trap = fixture(tmp, "trap", TRAP_C)
        passed, detail = _run_case(
            gcc, case_of("trap", "aic trap: 除以零 (division by zero)"), d_trap, objs, stub_emit(c_trap)
        )
        check("H3：trap 退出码非 0 + 前缀命中判定 PASS", passed, detail)

        d_notrap, c_notrap = fixture(tmp, "notrap", NO_TRAP_C)
        passed, detail = _run_case(
            gcc, case_of("trap", "aic trap: 除以零 (division by zero)"), d_notrap, objs, stub_emit(c_notrap)
        )
        check("H3：该 trap 却没 trap 判定 FAIL", not passed, detail)

        passed, detail = _run_case(gcc, case_of("trap", "完全不同的前缀"), d_trap, objs, stub_emit(c_trap))
        check("H3：trap 前缀不符判定 FAIL", not passed, detail)
    finally:
        shutil.rmtree(tmp, ignore_errors=True)


# ------------------------------------------------------------ H2 判负能力

def test_h2_normalization_is_not_a_blanket_amnesty() -> None:
    """归一化必须只吃路径，不能把 6 秒后才出现的差异也吃掉。"""
    same_semantics = '#line 1 "D:\\a\\b.aic"\nx;\n' == '#line 1 "/tmp/b.aic"\nx;\n'
    check("H2：不同路径文本本身不同（归一化确实在起作用）", not same_semantics)
    check(
        "H2：归一化后同一语义相等",
        normalize_emit('#line 1 "D:\\a\\b.aic"\nx;\n') == normalize_emit('#line 1 "/tmp/b.aic"\nx;\n'),
    )
    check(
        "H2：归一化不抹平实例顺序差异",
        normalize_emit("aic_f_1();\naic_f_2();\n") != normalize_emit("aic_f_2();\naic_f_1();\n"),
    )


def test_cli_absent_gates_skip() -> None:
    """无 CLI 时 H1/H2/H5 必须是 SKIP（合法占位）而不是 PASS。

    这条最容易悄悄失效：bin/aic.exe 一旦留在工作区，「缺 CLI」就永远测不到，
    于是「门禁在干净检出上会不会静默通过」这个问题再也不会被回答。故本用例
    显式把 AIC 指到不存在的路径（只在本用例内生效，随即还原）。
    """
    import aici_gates

    saved = aici_gates.AIC
    aici_gates.AIC = saved.parent / "definitely-not-built.exe"
    try:
        for name, fn in (("H1", gate_h1), ("H2", gate_h2), ("H5", gate_h5)):
            res = fn()
            check(f"{name}：无 aic CLI 时判 SKIP 而非 PASS", res.verdict is Verdict.SKIP, res.detail)
    finally:
        aici_gates.AIC = saved


def main(verbose: bool = True) -> int:
    utf8_stdio()
    FAILURES.clear()
    test_three_part_shape()
    test_normalize_emit()
    test_h2_normalization_is_not_a_blanket_amnesty()
    test_h3_gate()
    test_cli_absent_gates_skip()
    if FAILURES:
        print(f"[selftest] FAIL：{len(FAILURES)} 项未通过：{', '.join(FAILURES)}")
        return 1
    print("[selftest] PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
