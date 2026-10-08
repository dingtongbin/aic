"""Generate err/ corpus entries: one file per distinct diagnostic the front end
must produce. Every .err snapshot is written from what `aic check` actually
prints, so a snapshot can never drift from the compiler; the program text states
which rule it is meant to break.

Run from aic-src:  python scripts/gen_err_corpus.py
"""

import io
import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ERR_DIR = os.path.join(ROOT, "testdata", "err")
AIC = os.path.join(ROOT, "bin", "aic.exe")

# (topic, source, expected substring that must appear in the diagnostic)
CASES = [
    # --- H6 无数值隐式转换（红线#1） ---------------------------------------
    ("conv_i32_to_i64", 'func f() -> i64 {\n    var x i32 = 1\n    return x\n}\n', "禁止隐式数值转换"),
    ("conv_i64_to_i32", 'func f() -> i32 {\n    var x i64 = 1\n    return x\n}\n', "禁止隐式数值转换"),
    ("conv_arg_widen", 'func g(a i64) -> i64 {\n    return a\n}\n\nfunc f() -> i64 {\n    var x i32 = 1\n    return g(x)\n}\n', "禁止隐式数值转换"),
    ("conv_return_narrow", 'func f() -> i8 {\n    var x i32 = 300\n    return x\n}\n', "禁止隐式数值转换"),
    ("conv_literal_to_u8_range", 'func f() -> u8 {\n    return u8(300)\n}\n', "范围"),
    ("conv_signed_unsigned", 'func f() -> u32 {\n    var x i32 = 1\n    return x\n}\n', "禁止隐式数值转换"),
    ("conv_f32_f64", 'func f() -> f32 {\n    var x f64 = 1.5\n    return x\n}\n', "禁止隐式数值转换"),
    ("conv_bool_to_i32", 'func f() -> i32 {\n    var b bool = true\n    return b\n}\n', "类型不符且无隐式转换"),
    # --- 红线#2 Err 末位与吞 Err -------------------------------------------
    ("err_swallowed_call", 'func g() -> (i32, Err) {\n    return 1, nil\n}\n\nfunc f() {\n    var x = g()\n}\n', "Err"),
    ("err_not_last_slot", 'func f() -> (Err, i32) {\n    return nil, 1\n}\n', "末位"),
    ("err_arity_short", 'func g() -> (i32, i32, Err) {\n    return 1, 2, nil\n}\n\nfunc f() -> i32 {\n    var a, b = g()\n    return a\n}\n', "个数不匹配"),
    ("err_nil_into_i32", 'func f() -> i32 {\n    var x i32 = nil\n}\n', "nil"),
    ("err_check_on_plain", 'func g() -> i32 {\n    return 1\n}\n\nfunc f() {\n    var x = check g()\n}\n', "check"),
    ("err_return_without_slot", 'func f() -> (i32, Err) {\n    return 1\n}\n', "返回值个数"),
    # handle / errdefer 已退役（§一 关键字表）：这两条改判退役诊断。
    ("err_missing_handle_slot", 'func f() {\n    handle err {\n        return\n    }\n}\n', "已退役"),

    # --- 红线#3 裸名撞字段 ----------------------------------------------------
    ("this_bare_field", 'class Box {\n    var v i32 = 0\n\n    func get() -> i32 {\n        return v\n    }\n}\n', "this"),
    ("this_missing_prefix", 'class Box {\n    var v i32 = 0\n}\n\nfunc f(b Box) -> i32 {\n    return b.v\n}\n', "this"),

    # --- 索引与类型（红线: 下标固定 usize） -----------------------------------
    # 索引位接受任意整型（红线 1 的例外位，H6 锚点），i32 下标不是错误形态：
    # 该用例改由 ok/ 语料承担（index_i32_ok），此处不再作为 err 语料。
    ("index_on_scalar", 'func f(x i32) -> i32 {\n    return x[0]\n}\n', "下标"),
    ("index_wrong_key", 'func f(m map[str]i32) -> i32 {\n    return m[0]\n}\n', "键类型"),

    # --- match 穷尽（红线#6） -----------------------------------------------
    ("match_missing_arm", 'enum Color {\n    Red,\n    Green,\n    Blue\n}\n\nfunc f(c Color) -> i32 {\n    match c {\n        Red => { return 1 }\n        Green => { return 2 }\n    }\n}\n', "未穷尽"),
    ("match_duplicate_arm", 'enum Color {\n    Red,\n    Green\n}\n\nfunc f(c Color) -> i32 {\n    match c {\n        Red => { return 1 }\n        Red => { return 2 }\n        Green => { return 3 }\n    }\n}\n', "重复覆盖"),
    ("match_unknown_variant", 'enum Color {\n    Red,\n    Green\n}\n\nfunc f(c Color) -> i32 {\n    match c {\n        Red => { return 1 }\n        Purple => { return 2 }\n        Green => { return 3 }\n    }\n}\n', "不是该 enum 的变体"),
    ("match_on_int", 'func f(x i32) -> i32 {\n    match x {\n        return 1\n    }\n}\n', "match"),
    ("match_some_on_enum", 'enum Color {\n    Red,\n    Green\n}\n\nfunc f(c Color) -> i32 {\n    match c {\n        Some(x) => { return 1 }\n        None => { return 2 }\n    }\n}\n', "Some/None"),
    ("match_option_missing_none", 'func f(o Option[i32]) -> i32 {\n    match o {\n        Some(x) => { return x }\n    }\n}\n', "未穷尽"),

    # --- enum / Option ------------------------------------------------------
    ("enum_missing_comma", 'enum Color {\n    Red\n    Green\n}\n', "缺少"),
    ("option_bare_some", 'func f() {\n    var x = Some\n}\n', "Some"),
    ("err_variant_needs_some", 'enum Shape {\n    Circle(i32),\n    Square\n}\n\nfunc f() {\n    var s Shape = Shape.Circle\n}\n', "Some"),

    # --- defer / 错误出口前置条件（emit 门禁） --------------------------------
    ("errdefer_without_err_slot", 'import print\n\nfunc f() {\n    errdefer print.println("x")\n}\n', "已退役"),
    ("defer_non_call", 'import print\n\nfunc f() {\n    defer 1\n}\n', "defer"),
    ("handle_without_err_slot", 'func f() -> i32 {\n    handle err {\n        return 0\n    }\n    return 1\n}\n', "已退役"),
    ("handle_twice", 'func f() -> Err {\n    handle err {\n        return err\n    }\n    handle err {\n        return err\n    }\n    return nil\n}\n', "已退役"),
    ("check_without_error_exit", 'func g() -> (i32, Err) {\n    return 1, nil\n}\n\nfunc f() -> i32 {\n    var x i32 = check g()\n    return x\n}\n', "Err 返回位"),

    # --- 零容器 / 容器 ------------------------------------------------------
    ("set_needs_type", 'func f() {\n    var s = set.new()\n}\n', "元素类型"),
    ("map_needs_type", 'func f() {\n    var m = map.new()\n}\n', "元素类型"),
    ("set_wrong_element", 'import print\n\nfunc f() {\n    var s set[i32] = set.new()\n    s.add("text")\n}\n', "范围"),
    ("map_wrong_key", 'import print\n\nfunc f() {\n    var m map[str]i32 = map.new()\n    m.put(1, 2)\n}\n', "范围"),
    ("set_unknown_method", 'func f() {\n    var s set[i32] = set.new()\n    s.bogus(1)\n}\n', "没有方法"),

    # --- std 表面冻结（D18） -------------------------------------------------
    ("std_codepointlen_forbidden", 'import str\n\nfunc f() -> i32 {\n    return str.codepointLen("abc")\n}\n', "包内无此函数"),
    ("std_unknown_package_member", 'import str\n\nfunc f() -> bool {\n    return str.containsUnknown("a", "b")\n}\n', "包内无此函数"),
    # str + str = 连接（核心设计 §一 运算符全表第 3 级）：不是错误形态，
    # 故不再作为 err 语料；连接语义的正锚点在 ok/ 语料（str_plus_concat）。

    # --- 导入 / 声明形态 -----------------------------------------------------
    ("import_missing_name", 'import\n\nfunc main() {\n}\n', "缺少"),
    ("func_missing_name", 'func () -> i32 {\n    return 1\n}\n', "缺少"),
    ("var_missing_name", 'func main() {\n    var = 1\n}\n', "缺失"),
    ("var_missing_type", 'func main() {\n    var x =\n}\n', "缺少"),
    ("duplicate_declaration", 'func main() {\n    var x i32 = 1\n    var x i32 = 2\n}\n', "重复"),
    ("unknown_name", 'func main() {\n    print.println(nothing)\n}\n', "未声明的名字"),
    ("break_outside_loop", 'func main() {\n    break\n}\n', "break"),
    ("continue_outside_loop", 'func main() {\n    continue\n}\n', "continue"),
    ("assign_to_literal", 'func main() {\n    1 = 2\n}\n', "赋值"),

    # --- 打印规则（§十四：class 须 @derive(ToString)） -----------------------
    ("print_class_without_tostring", 'import print\n\nclass P {\n    var x i32\n}\n\nfunc main() {\n    var p = P{x: 1}\n    print.println(p)\n}\n', "@derive(ToString)"),
    ("print_list_of_class_without_tostring", 'import print\n\nclass P {\n    var x i32\n}\n\nfunc main() {\n    var ps P[] = []\n    print.println(ps)\n}\n', "@derive(ToString)"),
    ("derive_unknown_capability", 'import print\n\n@derive(Debug)\nclass P {\n    var x i32\n}\n\nfunc main() {\n    print.println(1)\n}\n', "三件套"),
    ("derive_compare_conflict", 'import print\n\n@derive(Compare)\nclass P {\n    var x i32\n\n    func compare(o P) -> i32 {\n        return 0\n    }\n}\n\nfunc main() {\n    print.println(1)\n}\n', "重名"),
    ("derive_hash_container_field", 'import print\n\n@derive(Hash)\nclass P {\n    var xs i32[]\n}\n\nfunc main() {\n    print.println(1)\n}\n', "不可哈希"),
    ("derive_compare_data_enum_field", 'import print\n\nenum E {\n    A(i32),\n    B\n}\n\n@derive(Compare)\nclass P {\n    var e E\n}\n\nfunc main() {\n    print.println(1)\n}\n', "不可比较"),
    ("derive_compare_missing_nested", 'import print\n\nclass Inner {\n    var x i32\n}\n\n@derive(Compare)\nclass Outer {\n    var i Inner\n}\n\nfunc main() {\n    print.println(1)\n}\n', "没有 @derive(Compare)"),
    ("iface_unsatisfied_missing_method", 'import print\n\ninterface Speaker {\n    func speak() -> str\n    func loud() -> str\n}\n\nclass Dog {\n    func speak() -> str {\n        return "woof"\n    }\n}\n\nfunc main() {\n    var d = Dog{}\n    var s Speaker = d\n    print.println(s.speak())\n}\n', "类不满足接口"),
    ("iface_unsatisfied_wrong_sig", 'import print\n\ninterface Counter {\n    func count() -> i32\n}\n\nclass Tally {\n    func count() -> i64 {\n        return 1\n    }\n}\n\nfunc main() {\n    var t = Tally{}\n    var c Counter = t\n    print.println(c.count())\n}\n', "类不满足接口"),
    ("iface_method_missing", 'import print\n\ninterface Speaker {\n    func speak() -> str\n}\n\nclass Dog {\n    func speak() -> str {\n        return "woof"\n    }\n}\n\nfunc main() {\n    var s Speaker = Dog{}\n    print.println(s.bark())\n}\n', "has no such method"),
    # --- 泛型（T1 单态化） ---------------------------------------------------
    ("generic_arg_count_mismatch", 'class Box[T] {\n    var v T\n}\n\nfunc main() {\n    var b = Box[i32, str]{v: 1}\n}\n', "wrong number of type arguments"),
    ("generic_args_on_plain_type", 'class P {\n    var x i32\n}\n\nfunc main() {\n    var p = P[i32]{x: 1}\n}\n', "takes no type arguments"),
    ("generic_cannot_infer", 'func make[T]() -> T {\n    var z T\n    return z\n}\n\nfunc main() {\n    var x = make()\n}\n', "cannot infer the type arguments"),
    ("generic_packed_bad_field", 'class Ref {\n    var x i32\n}\n\n@packed\nclass Bad[T] {\n    var r Ref\n}\n\nfunc main() {\n    var b = Bad[i32]{r: Ref{x: 1}}\n}\n', "@packed field type is not on the whitelist")
]


def next_index() -> int:
    used = []
    for name in os.listdir(ERR_DIR):
        if name.endswith(".aic"):
            stem = name.split("_", 1)[0]
            if stem.isdigit():
                used.append(int(stem))
    return max(used) + 1 if used else 1


def existing_topics() -> set:
    """语料里已存在的主题名（`<编号>_<主题>.aic` 的主题段）。

    幂等判据：主题已在册 = 不再写新文件。少了这一步，重跑一次就多一份同主题
    语料（编号不同、内容相同），既撑大语料又让门禁重复劳动。
    """
    topics = set()
    for name in os.listdir(ERR_DIR):
        if not name.endswith(".aic"):
            continue
        parts = name.split("_", 1)
        if len(parts) == 2 and parts[0].isdigit():
            topics.add(parts[1][:-4])
    return topics


def main() -> int:
    """Write the .aic files only.

    The .err snapshots are generated by the Go side
    (`AIC_UPDATE_GOLDEN=1 go test ./internal/golden -run TestUpdateErrGoldens`),
    so the wording a snapshot claims can never drift from what the checker prints.
    This script only decides *which rules deserve a corpus file*（幂等：主题已在册
    即跳过，重跑不产生同主题副本）。
    """
    if not os.path.exists(AIC):
        print("bin/aic.exe 不存在：先 go build -o bin/aic.exe ./cmd/aic", file=sys.stderr)
        return 1
    os.makedirs(ERR_DIR, exist_ok=True)
    idx = next_index()
    known = existing_topics()
    written = 0
    skipped = []
    accepted, rejected = [], []
    for topic, src, want in CASES:
        if topic in known:
            skipped.append(topic)
            continue
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
            rejected.append((topic, "诊断与预期不符，期望包含 %r，实际首行 %r"
                             % (want, text.strip().splitlines()[0] if text.strip() else "")))
            os.remove(aic_path)
            continue
        accepted.append(topic)
        written += 1
    print("写入 %d 条检查期 .aic 语料" % written)
    if skipped:
        print("跳过 %d 条已在册的主题（幂等）：%s" % (len(skipped), ", ".join(sorted(skipped))))
    if rejected:
        print("未写入（请修正用例或修编译器）：")
        for topic, why in rejected:
            print("  %s: %s" % (topic, why))
    print("接着运行：$env:AIC_UPDATE_GOLDEN=1; go test ./internal/types -run TestUpdateCheckGoldens")
    return 0


if __name__ == "__main__":
    sys.exit(main())
