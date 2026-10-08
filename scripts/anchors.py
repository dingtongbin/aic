#!/usr/bin/env python3
"""显式锚点清单（黄金执行规划 §七.2 T3「红线逐条转 ok + 显式锚点清单 scripts/anchors.py」）。

单一事实来源 = 规划文档 §五 的红线表（本脚本**运行时解析它**，不抄第二份），
本文件只维护「哪条红线/哪个语言特性由哪个语料锚点钉住」。

判据（`--strict` 下全部为硬失败；默认只报告）：
  ① 每条红线 1–29 至少有一个锚点，且锚点文件真实存在；
  ② 每个关键字（23）与注解（5）至少有一个 ok 锚点；
  ③ 每个「特性」条目有 ok（正常路径）；错误类特性还须有 err 锚点；
     运行时 trap 类特性还须有 trap 锚点（§四.1 的「三锚点」要求）。

用法：
  python scripts/anchors.py            # 报告（缺失只打印）
  python scripts/anchors.py --strict   # 有缺口即退出 1（ci.py --strict 用）
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PLAN = ROOT.parent / "黄金执行规划.md"
OK = ROOT / "testdata" / "ok"
ERR = ROOT / "testdata" / "err"
TRAP = ROOT / "testdata" / "trap"

# --- 红线 → 锚点（键 = 红线号；值 = 语料文件名，不含扩展名） -------------------
RED_LINE_ANCHORS: dict[int, tuple[str, ...]] = {
    1: ("020_explicit_conversions", "227_conv_assignment_mismatch"),
    2: ("017_multi_return", "101_err_swallowed_call", "304_err_nil_into_i32"),
    3: ("018_shadows_none", "121_option_bare_some"),
    4: ("026_match_enum", "705_enum_match_values", "114_match_missing_arm"),
    5: ("742_scope_spawn", "016_spawn_outside_scope"),
    6: ("710_check_and_handle", "734_nil_interface_call", "734_interface_dispatch"),
    7: ("715_region_block", "748_guard_same_region", "748_guard_cross_region"),
    8: ("712_defer_runs_lifo", "713_errdefer_only_on_error", "714_defer_captures_arguments"),
    9: ("001_hello", "748_guard_same_region"),  # #line 全覆盖由 H2/H8 + trap 行号锚点钉住
    10: ("740_generic_functions", "747_export_zero_mangle", "741_generic_methods"),
    11: ("740_generic_functions", "694_list_element_types_are_separate"),
    12: ("001_hello",),  # emit 确定性由 H2 钉住（两次发射逐字节）
    13: ("730_print_containers", "024_print_typed"),
    14: ("012_while_loop", "013_for_three_part", "014_for_break_continue"),
    15: ("007_operators", "720_bool_logic", "009_compare"),
    16: ("742_scope_spawn", "743_chan", "744_mutex"),
    17: ("742_scope_spawn",),  # netpoller 循环归圆 3；T1/T2 以 scope/chan 锚点占位
    18: ("001_hello",),  # 禁 cgo 由 #20 门禁钉住
    19: ("746_c_boundary_annotations",),
    20: ("730_print_containers", "024_print_typed"),  # 打印走 L0/L1，不经 libc printf
    21: ("021_globals_and_locals", "041_top_level_var"),
    22: ("748_guard_same_region",),  # -O2 + 跨 TU 内联（同次编译）由 H10③ 钉住
    23: ("742_scope_spawn", "740_generic_functions"),  # 多包/多文件由 pkg/ 语料钉住
    24: ("748_guard_same_region",),  # IR 校验常开由 H8 钉住
    25: ("748_guard_same_region",),  # restrict 的 NoAlias 前提由 H10② 钉住
    26: ("748_guard_same_region", "003_string_index_oob"),  # 内联检查 + cold 慢路径由 H10①
    27: ("745_live_annotation", "716_composite_literal"),
    28: ("733_derive_compare_hash", "741_generic_methods"),
    29: ("003_string_index_oob", "734_nil_interface_call"),
}

# --- 关键字 / 注解 → ok 锚点（H7 的字面量表是单一来源，这里给锚点） ------------
KEYWORD_ANCHORS: dict[str, tuple[str, ...]] = {
    "class": ("716_composite_literal",),
    "interface": ("734_interface_dispatch",),
    "enum": ("026_match_enum",),
    "var": ("001_hello",),
    "const": ("021_globals_and_locals",),
    "func": ("015_functions",),
    "if": ("011_if_chain",),
    "else": ("701_if_else_chain",),
    "for": ("717_for_forms",),
    "in": ("025_for_range",),
    "return": ("015_functions",),
    "break": ("702_while_and_break",),
    "continue": ("703_continue_skips_rest",),
    "match": ("026_match_enum",),
    "defer": ("034_defer",),
    "check": ("028_check_handle",),
    "spawn": ("742_scope_spawn",),
    "scope": ("742_scope_spawn",),
    "region": ("715_region_block",),
    "this": ("716_composite_literal",),
    "import": ("036_std_str",),
    "export": ("747_export_zero_mangle",),
    "extern": ("746_c_boundary_annotations",),
}

ANNOTATION_ANCHORS: dict[str, tuple[str, ...]] = {
    "@packed": ("005_annotations", "741_generic_methods"),
    "@live": ("745_live_annotation",),
    "@derive": ("733_derive_compare_hash",),
    "@noblock": ("746_c_boundary_annotations",),
    "@blocking": ("746_c_boundary_annotations",),
}

# --- 特性 → (ok 锚点, err 锚点, trap 锚点) -----------------------------------
FEATURE_ANCHORS: dict[str, tuple[tuple[str, ...], tuple[str, ...], tuple[str, ...]]] = {
    "容器 list": (("690_list_basics",), ("230_list_append_wrong_type",), ("111_list_index_out_of_bounds_read",)),
    "容器 set": (("696_set_dedup_and_order",), ("233_set_unhashable_element",), ()),
    "容器 map": (("698_map_insertion_order_and_zero",), ("231_map_put_key_wrong",), ()),
    "定长数组": (("715_fixed_array_basics",), ("235_array_literal_too_few",), ("004_fixed_array_oob",)),
    "str 方法": (("683_str_concat_replace_trim",), ("279_str_slice_argument_type",), ("114_str_slice_out_of_range",)),
    "Err/check": (("028_check_handle",), ("101_err_swallowed_call",), ()),
    "Option": (("707_option_ok_none_helpers",), ("121_option_bare_some",), ()),
    "接口分发": (("734_interface_dispatch",), ("250_interface_method_missing",), ("734_nil_interface_call",)),
    "泛型函数": (("740_generic_functions",), ("368_generic_cannot_infer",), ()),
    "泛型类": (("741_generic_methods",), ("366_generic_arg_count_mismatch",), ()),
    "region 守卫": (("748_guard_same_region",), (), ("748_guard_cross_region",)),
    "@live": (("745_live_annotation",), ("273_live_without_variable",), ()),
    "defer": (("712_defer_runs_lifo",), ("324_defer_non_call",), ()),
    "spawn/scope": (("742_scope_spawn",), ("016_spawn_outside_scope",), ()),
    "chan": (("743_chan",), (), ()),
    "sync.Mutex": (("744_mutex",), (), ()),
    "导出/绑定": (("747_export_zero_mangle",), ("262_call_unknown_function",), ()),
    "打印": (("730_print_containers",), ("345_print_class_without_tostring",), ()),
    "@derive": (("733_derive_compare_hash",), ("347_derive_unknown_capability",), ()),
    "隐式转换禁令": (("020_explicit_conversions",), ("226_conv_operand_mismatch",), ()),
    "match": (("026_match_enum",), ("114_match_missing_arm",), ()),
    "lambda": (("718_lambda",), ("042_lambda_typed_param",), ()),
    "文件 IO": (("036_std_str",), ("281_os_readfile_argument",), ()),
}


def red_lines() -> list[int]:
    """从规划文档 §五 的红线表解析出红线号（单一事实来源）。"""
    if not PLAN.is_file():
        return list(range(1, 30))
    text = PLAN.read_text(encoding="utf-8", errors="replace")
    nums = []
    for m in re.finditer(r"^\|\s*(\d+)\s*\|[^|]*\|[^|]*\|\s*([^|]*)\|", text, re.M):
        n = int(m.group(1))
        if 1 <= n <= 29 and n not in nums:
            nums.append(n)
    return sorted(nums) or list(range(1, 30))


def exists(kind: str, name: str) -> bool:
    d = {"ok": OK, "err": ERR, "trap": TRAP}[kind]
    return (d / (name + ".aic")).is_file()


def main() -> int:
    strict = "--strict" in sys.argv
    problems: list[str] = []
    checked = 0

    lines = red_lines()
    for n in lines:
        anchors = RED_LINE_ANCHORS.get(n)
        if not anchors:
            problems.append(f"红线 {n} 没有锚点条目")
            continue
        for a in anchors:
            for kind in ("ok", "err", "trap"):
                if exists(kind, a):
                    checked += 1
                    break
            else:
                problems.append(f"红线 {n} 的锚点 {a} 不存在")

    for kw, anchors in KEYWORD_ANCHORS.items():
        if not any(exists("ok", a) for a in anchors):
            problems.append(f"关键字 {kw} 缺 ok 锚点")
        else:
            checked += 1
    for ann, anchors in ANNOTATION_ANCHORS.items():
        if not any(exists("ok", a) for a in anchors):
            problems.append(f"注解 {ann} 缺 ok 锚点")
        else:
            checked += 1

    for feat, (oks, errs, traps) in FEATURE_ANCHORS.items():
        if not any(exists("ok", a) for a in oks):
            problems.append(f"特性「{feat}」缺 ok 锚点")
        else:
            checked += 1
        for a in errs:
            if not exists("err", a):
                problems.append(f"特性「{feat}」的 err 锚点 {a} 不存在")
        for a in traps:
            if not exists("trap", a):
                problems.append(f"特性「{feat}」的 trap 锚点 {a} 不存在")

    print(f"锚点清单：红线 {len(lines)} 条、关键字 {len(KEYWORD_ANCHORS)} 个、"
          f"注解 {len(ANNOTATION_ANCHORS)} 个、特性 {len(FEATURE_ANCHORS)} 项；校验通过 {checked} 项")
    if problems:
        print(f"缺口 {len(problems)} 处：")
        for p in problems:
            print("  -", p)
    else:
        print("无缺口：每条红线/关键字/注解/特性都有真实存在的锚点")
    if strict and problems:
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
