"""Second batch of check-stage err/ corpus rules.

Kept apart from gen_err_corpus.py only because the file is already long; the
expectations are validated the same way — the program states the rule it breaks, and a
case whose diagnostic does not match is reported instead of written, so a snapshot can
never be written for the wrong reason.

Run from aic-src:  python scripts/gen_err_corpus2.py
"""

import io
import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ERR_DIR = os.path.join(ROOT, "testdata", "err")
AIC = os.path.join(ROOT, "bin", "aic.exe")

CASES = [
    # --- H6 无数值隐式转换（红线#1） ----------------------------------------
    ("conv_bool_to_numeric", 'func f() -> i32 {\n    var b bool = true\n    return b\n}\n', "禁止隐式"),
    ("conv_numeric_to_bool", 'func f() -> bool {\n    var x i32 = 1\n    return x\n}\n', "禁止隐式"),
    ("conv_str_to_number", 'func f() -> i32 {\n    var s str = "1"\n    return s\n}\n', "禁止隐式"),
    ("conv_number_to_str", 'func f() -> str {\n    var x i32 = 1\n    return x\n}\n', "禁止隐式"),
    ("conv_i64_to_u8_narrow", 'func f(a i64) -> u8 {\n    return u8(a)\n}\n', "u8"),
    ("conv_u32_to_i32", 'func f(a u32) -> i32 {\n    return i32(a)\n}\n', "i32"),
    ("conv_usize_to_i32", 'func f(a usize) -> i32 {\n    return i32(a)\n}\n', "i32"),
    ("conv_operand_mismatch", 'func f(a i32, b i64) -> i32 {\n    return a + b\n}\n', "禁止隐式"),
    ("conv_assignment_mismatch", 'func f() {\n    var a i32 = 1\n    var b i64 = 2\n    a = b\n}\n', "禁止隐式"),
    ("conv_compound_assign", 'func f() {\n    var a i32 = 1\n    var b i64 = 2\n    a += b\n}\n', "禁止隐式"),

    # --- 容器元素/键类型（红线#6, D15） -------------------------------------
    ("list_element_mismatch", 'func f() {\n    var xs i32[] = [1, "two"]\n}\n', "元素"),
    ("list_append_wrong_type", 'func f() {\n    var xs i32[] = [1]\n    xs.append("two")\n}\n', "元素类型"),
    ("map_put_key_wrong", 'func f() {\n    var m map[str]i32 = map.new()\n    m.put(1, 2)\n}\n', "键类型"),
    ("map_put_value_wrong", 'func f() {\n    var m map[str]i32 = map.new()\n    m.put("k", "v")\n}\n', "值类型"),
    ("set_unhashable_element", 'func f() {\n    var s set[i32[]] = set.new()\n}\n', "不可哈希"),
    ("map_unhashable_key", 'func f() {\n    var m map[i32[]]str = map.new()\n}\n', "不可哈希"),
    ("array_literal_too_few", 'func f() {\n    var a [i32; 3] = [1, 2]\n}\n', "个数"),
    ("array_literal_too_many", 'func f() {\n    var a [i32; 2] = [1, 2, 3]\n}\n', "个数"),
    ("array_literal_element_mismatch", 'func f() {\n    var a [i32; 2] = [1, "x"]\n}\n', "元素"),
    ("array_length_zero", 'func f() {\n    var a [i32; 0]\n}\n', "[T;N]"),
    ("map_unsupported_pair", 'func f() {\n    var m map[f32]i32 = map.new()\n}\n', "运行时容器"),
    ("container_new_with_arg", 'func f() {\n    var m map[str]i32 = map.new(1)\n}\n', "不接受实参"),

    # --- Option / enum ------------------------------------------------------
    ("option_some_two_args", 'func f() {\n    var o = Some(1, 2)\n}\n', "实参"),
    ("option_some_wrong_payload", 'func f() -> Option[str] {\n    return Some(1)\n}\n', "禁止隐式"),
    ("none_not_option", 'func f() -> i32 {\n    return None\n}\n', "禁止隐式"),
    ("enum_duplicate_variant", 'enum E {\n    A,\n    A\n}\n', "重复"),
    ("enum_recursive", 'enum E {\n    A(E)\n}\n', "enum"),
    ("enum_unknown_variant", 'enum E {\n    A\n}\n\nfunc f() -> E {\n    return E.B\n}\n', "没有"),
    ("enum_int_underlying", 'enum E {\n    A\n}\n\nfunc f() -> i32 {\n    return E.A\n}\n', "禁止隐式"),
    ("match_wildcard_rejected", 'enum E {\n    A,\n    B\n}\n\nfunc f(e E) -> i32 {\n    match e {\n        A => { return 1 }\n        _ => { return 2 }\n    }\n}\n', "match"),
    ("match_arm_block_missing", 'enum E {\n    A\n}\n\nfunc f(e E) -> i32 {\n    match e {\n        A => 1\n    }\n}\n', "match"),

    # --- class / interface --------------------------------------------------
    ("interface_method_missing", 'interface Shape {\n    func area() -> i32\n}\n\nclass Square implements Shape {\n    var side i32 = 0\n}\n\nfunc main() {\n}\n', "area"),
    ("interface_unknown_method", 'interface Shape {\n    func area() -> i32\n}\n\nclass Square implements Shape {\n    func area() -> i32 {\n        return 0\n    }\n\n    func bogus() -> i32 {\n        return 0\n    }\n}\n\nfunc main() {\n}\n', "bogus"),
    ("class_duplicate_field", 'class Box {\n    var v i32 = 0\n    var v i32 = 1\n}\n\nfunc main() {\n}\n', "重复"),
    ("class_duplicate_method", 'class Box {\n    func f() -> i32 {\n        return 1\n    }\n\n    func f() -> i32 {\n        return 2\n    }\n}\n\nfunc main() {\n}\n', "重复"),
    ("class_duplicate_init", 'class Box {\n    func init() {\n    }\n\n    func init() {\n    }\n}\n\nfunc main() {\n}\n', "init"),
    ("method_outside_class", 'func speak() {\n}\n\nfunc main() {\n}\n', "class"),
    ("this_outside_method", 'func f() -> i32 {\n    return this.v\n}\n\nfunc main() {\n}\n', "this"),
    ("class_method_unknown", 'class Box {\n    var v i32 = 0\n}\n\nfunc main() {\n    var b Box = Box.init()\n    b.missing()\n}\n', "没有方法"),
    ("init_as_expression", 'class Box {\n    func init() {\n    }\n}\n\nfunc main() {\n    var b Box = Box.init\n}\n', "init"),
    ("init_as_statement_missing", 'class Base {\n    func init() {\n    }\n}\n\nclass Derived {\n    var v i32 = 0\n\n    func init() {\n        this.v = 1\n    }\n}\n\nfunc main() {\n    var d Derived = Derived.init()\n}\n', "init"),
    ("super_init_arity", 'class Base {\n    func init(n i32) {\n    }\n}\n\nclass Derived {\n    var v i32 = 0\n\n    func init() {\n        Base.init(this)\n    }\n}\n\nfunc main() {\n    var d Derived = Derived.init()\n}\n', "实参"),

    # --- 函数与调用 --------------------------------------------------------
    ("func_duplicate", 'func f() -> i32 {\n    return 1\n}\n\nfunc f() -> i32 {\n    return 2\n}\n\nfunc main() {\n}\n', "重复"),
    ("call_unknown_function", 'func main() {\n    missing()\n}\n', "未声明"),
    ("call_too_few_args", 'func g(a i32, b i32) -> i32 {\n    return a\n}\n\nfunc main() {\n    var x = g(1)\n}\n', "实参"),
    ("call_too_many_args", 'func g(a i32) -> i32 {\n    return a\n}\n\nfunc main() {\n    var x = g(1, 2)\n}\n', "实参"),
    ("return_type_mismatch", 'func f() -> i32 {\n    return "text"\n}\n\nfunc main() {\n}\n', "禁止隐式"),
    ("return_missing_value", 'func f() -> i32 {\n    return\n}\n\nfunc main() {\n}\n', "返回值"),
    ("missing_return", 'func f() -> i32 {\n    var x = 1\n}\n\nfunc main() {\n}\n', "返回"),
    ("param_type_unknown", 'func f(a unknown) -> i32 {\n    return 1\n}\n\nfunc main() {\n}\n', "未知"),
    ("result_type_unknown", 'func f() -> unknown {\n    return 1\n}\n\nfunc main() {\n}\n', "未知"),

    # --- 错误出口的前置条件 --------------------------------------------------
    ("handle_nested_in_if", 'func f(flag bool) -> (i32, Err) {\n    if flag {\n        handle err {\n            return 0, err\n        }\n    }\n    return 1, nil\n}\n\nfunc main() {\n}\n', "直接语句"),
    ("check_without_handle", 'func g() -> (i32, Err) {\n    return 1, nil\n}\n\nfunc f() -> (i32, Err) {\n    var x = check g()\n    return x, nil\n}\n\nfunc main() {\n}\n', "handle err"),
    ("defer_complex_argument", 'import print\n\nfunc f() {\n    defer print.println(1 + 2)\n}\n\nfunc main() {\n}\n', "实参"),

    # --- @live / scope / del ------------------------------------------------
    ("live_without_variable", 'func main() {\n    @live missing\n}\n', "@live"),
    ("del_without_live", 'func main() {\n    var x i32 = 1\n    del x\n}\n', "del"),
    ("await_outside_scope", 'func main() {\n    await missing\n}\n', "await"),

    # --- std 冻结表面 -------------------------------------------------------
    ("std_arity_too_few", 'import str\n\nfunc main() {\n    var b = str.contains("abc")\n}\n', "实参"),
    ("std_arity_too_many", 'import str\n\nfunc main() {\n    var b = str.contains("a", "b", "c")\n}\n', "实参"),
    ("std_argument_type", 'import str\n\nfunc main() {\n    var b = str.contains(1, 2)\n}\n', "禁止隐式"),
    ("str_slice_argument_type", 'import str\n\nfunc main() {\n    var s = str.slice("abc", i32(0), i32(1))\n}\n', "usize"),
    ("print_unsupported_type", 'import print\n\nclass Box {\n    var v i32 = 0\n}\n\nfunc main() {\n    var b Box = Box.init()\n    print.println(b)\n}\n', "print"),
    ("os_readfile_argument", 'import os\n\nfunc main() {\n    var s, err = os.readFile(1)\n}\n', "禁止隐式"),
    ("option_unwrap_wrong_type", 'import option\n\nfunc main() {\n    var v = option.unwrap(Some("text"))\n}\n', "i64"),
    ("math_abs_wrong_type", 'import math\n\nfunc main() {\n    var v = math.abs(1.5)\n}\n', "i64"),

    # --- 声明形态 -----------------------------------------------------------
    ("import_duplicate", 'import str\nimport str\n\nfunc main() {\n}\n', "import"),
    ("annotation_unknown", '@nonsense\nimport str\n\nfunc main() {\n}\n', "注解"),
    ("annotation_arg_count", '@derive(Equals, Hash, ToString, Compare, Clone, Bogus)\nclass Box {\n    var v i32 = 0\n}\n\nfunc main() {\n}\n', "derive"),
    ("generic_missing_argument", 'func f() {\n    var b Box[int]\n}\n\nfunc main() {\n}\n', "Box"),
    ("generic_unknown_parameter", 'func f() {\n    var b Box[T]\n}\n\nfunc main() {\n}\n', "T"),
    ("interface_with_body_method", 'interface Shape {\n    func area() -> i32 {\n        return 0\n    }\n}\n\nfunc main() {\n}\n', "interface"),
    ("class_field_needs_this", 'class Box {\n    func get() -> i32 {\n        return v\n    }\n}\n\nfunc main() {\n}\n', "this"),
    ("packed_field_not_scalar", '@packed class P {\n    var xs i32[]\n}\n\nfunc main() {\n}\n', "@packed"),
    ("byte_vs_u8_mismatch", 'func f() -> byte {\n    return u8(1)\n}\n\nfunc main() {\n}\n', "禁止隐式"),
    ("shadow_import_as_var", 'import str\n\nfunc main() {\n    var str i32 = 1\n}\n', "重复"),
]


def next_index() -> int:
    used = []
    for name in os.listdir(ERR_DIR):
        if name.endswith(".aic"):
            stem = name.split("_", 1)[0]
            if stem.isdigit():
                used.append(int(stem))
    return max(used) + 1 if used else 1


def main() -> int:
    if not os.path.exists(AIC):
        print("bin/aic.exe 不存在：先 go build -o bin/aic.exe ./cmd/aic", file=sys.stderr)
        return 1
    os.makedirs(ERR_DIR, exist_ok=True)
    idx = next_index()
    written = 0
    rejected = []
    for topic, src, want in CASES:
        stem = "%03d_%s" % (idx, topic)
        idx += 1
        aic_path = os.path.join(ERR_DIR, stem + ".aic")
        io.open(aic_path, "w", encoding="utf-8", newline="\n").write(src)
        proc = subprocess.run([AIC, "check", os.path.join("testdata", "err", stem + ".aic")],
                              cwd=ROOT, capture_output=True)
        text = (proc.stderr or proc.stdout).decode("utf-8", "replace")
        if proc.returncode == 0:
            rejected.append((topic, "竟然通过了检查"))
            os.remove(aic_path)
            continue
        if want not in text:
            first = text.strip().splitlines()[0] if text.strip() else ""
            rejected.append((topic, "诊断与预期不符，期望包含 %r，实际 %s" % (want, first)))
            os.remove(aic_path)
            continue
        written += 1
    print("写入 %d 条检查期 .aic 语料" % written)
    if rejected:
        print("未写入（请修正用例或修编译器）：")
        for topic, why in rejected:
            print("  %s: %s" % (topic, why))
    print("接着运行：$env:AIC_UPDATE_GOLDEN=1; go test ./internal/types -run TestUpdateCheckGoldens")
    return 0


if __name__ == "__main__":
    sys.exit(main())