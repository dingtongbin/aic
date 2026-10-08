"""Generate the ok/ and trap/ halves of the circle-0 corpus.

The rule this script exists to enforce: an .out snapshot is never recorded from the
compiler's own output. Every expected line is computed here, independently, from what
the program is supposed to mean. If the compiler disagrees, the gate fails instead of
quietly blessing whatever it happens to print today.

Run from aic-src:  python scripts/gen_ok_corpus.py
"""

import hashlib
import io
import os
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OK_DIR = os.path.join(ROOT, "testdata", "ok")
TRAP_DIR = os.path.join(ROOT, "testdata", "trap")
AIC = os.path.join(ROOT, "bin", "aic.exe")

# (topic, source, expected stdout lines)
OK_CASES = [
    # --- str: only the frozen 圆0 surface, every answer a property of the word ---
    ("str_prefix_suffix", '''import print
import str

func main() {
    print.println(str.startsWith("compiler", "comp"))
    print.println(str.startsWith("compiler", "pile"))
    print.println(str.endsWith("compiler", "iler"))
    print.println(str.endsWith("compiler", "comp"))
}
''', ["true", "false", "true", "false"]),

    ("str_indexof_and_slice", '''import print
import str

func main() {
    print.println(str.indexOf("compiler", "pil"))
    print.println(str.indexOf("compiler", "zzz"))
    print.println(str.indexOf("abc", "abc"))
    print.println(str.slice("compiler", 0, 5))
    print.println(str.slice("compiler", 4, 8))
}
''', ["3", "-1", "0", "compi", "iler"]),

    ("str_concat_replace_trim", '''import print
import str

func main() {
    print.println(str.concat("a", "b"))
    print.println(str.replace("a-b-c", "-", "+"))
    print.println(str.replace("aaa", "a", "bb"))
    print.println(str.concat("[", str.concat(str.trim("  pad  "), "]")))
}
''', ["ab", "a+b+c", "bbbbbb", "[pad]"]),

    ("str_split_join", '''import print
import str

func main() {
    var parts = str.split("a,b,c", ",")
    print.println(parts.len())
    print.println(str.join(parts, "-"))
    print.println(str.join([], "-"))
    print.println(str.split("", ",").len())
}
''', ["3", "a-b-c", "", "1"]),

    ("str_len_and_isempty", '''import print

func main() {
    print.println("abc".len())
    print.println("".len())
    print.println("abc".isEmpty())
    print.println("".isEmpty())
    print.println("héllo".len())
}
''', ["3", "0", "false", "true", "6"]),

    ("str_eq", '''import print
import str

func main() {
    print.println(str.eq("same", "same"))
    print.println(str.eq("same", "different"))
    print.println(str.eq("", ""))
}
''', ["true", "false", "true"]),

    ("str_toi64", '''import print
import option
import str

func main() {
    var good = str.toI64(" 42")
    print.println(option.isSome(good))
    print.println(option.unwrap(good))
    var bad = str.toI64("4x2")
    print.println(option.isSome(bad))
}
''', ["true", "42", "false"]),

    # --- math: the 圆0 surface is abs/min/max, integer and float kept apart ----
    ("math_integers", '''import print
import math

func main() {
    print.println(math.abs(i64(0) - i64(9)))
    print.println(math.abs(i64(9)))
    print.println(math.min(2, 8))
    print.println(math.max(2, 8))
}
''', ["9", "9", "2", "8"]),

    ("math_floats", '''import print
import math

func main() {
    print.println(math.minF(1.5, 2.5))
    print.println(math.maxF(1.5, 2.5))
    print.println(math.absF(0.0 - 2.5))
}
''', ["1.5", "2.5", "2.5"]),

    # --- list: D15 surface is len/isEmpty/append; elements come back by index ---
    ("list_basics", '''import print

func main() {
    var xs i32[] = [3, 1, 2]
    print.println(xs.len())
    xs.append(4)
    print.println(xs.len())
    for var i usize = 0; i < xs.len(); i = i + 1 {
        print.println(xs[i])
    }
    print.println(xs.isEmpty())
}
''', ["3", "4", "3", "1", "2", "4", "false"]),

    ("list_empty_then_writable", '''import print

func main() {
    var xs i64[] = []
    print.println(xs.isEmpty())
    xs.append(7)
    print.println(xs.len())
    print.println(xs[0])
}
''', ["true", "1", "7"]),

    ("list_explicit_empty_literal", '''import print

func main() {
    var xs i32[] = []
    print.println(xs.isEmpty())
    xs.append(1)
    print.println(xs[0])
}
''', ["true", "1"]),

    ("list_iteration_order", '''import print

func main() {
    var xs str[] = ["b", "a", "c"]
    for x in xs {
        print.println(x)
    }
    for ch in "hey" {
        print.println(ch)
    }
}
''', ["b", "a", "c", "h", "e", "y"]),

    ("list_element_types_are_separate", '''import print

func main() {
    var ints i32[] = [1]
    var strs str[] = ["1"]
    print.println(ints[0])
    print.println(strs[0])
    print.println(ints.len())
    print.println(strs.len())
}
''', ["1", "1", "1", "1"]),

    ("list_index_write_grows", '''import print

func main() {
    var xs i32[] = [1]
    xs[3] = 9
    print.println(xs.len())
    print.println(xs[3])
}
''', ["4", "9"]),

    # --- set / map: insertion order is the defined iteration order -------------
    ("set_dedup_and_order", '''import print

func main() {
    var s set[i32] = set.new()
    s.add(3)
    s.add(1)
    s.add(3)
    s.add(2)
    print.println(s.len())
    print.println(s.has(3))
    print.println(s.has(9))
    for x in s {
        print.println(x)
    }
}
''', ["3", "true", "false", "3", "1", "2"]),

    ("set_at_follows_insertion", '''import print

func main() {
    var s set[str] = set.new()
    s.add("z")
    s.add("y")
    print.println(s.at(0))
    print.println(s.at(1))
    print.println(s.len())
}
''', ["z", "y", "2"]),

    ("map_insertion_order_and_zero", '''import print

func main() {
    var m map[str]i32 = map.new()
    m.put("one", 1)
    m.put("two", 2)
    m.put("one", 11)
    print.println(m.len())
    print.println(m.get("one"))
    print.println(m.get("two"))
    print.println(m.get("missing"))
    print.println(m.has("two"))
    print.println(m.has("nope"))
}
''', ["2", "11", "2", "0", "true", "false"]),

    ("map_key_at_val_at", '''import print

func main() {
    var m map[i32]str = map.new()
    m.put(7, "seven")
    m.put(3, "three")
    print.println(m.len())
    print.println(m.keyAt(0))
    print.println(m.valAt(0))
    print.println(m.keyAt(1))
    print.println(m.valAt(1))
}
''', ["2", "7", "seven", "3", "three"]),

    ("map_iterates_in_insertion_order", '''import print

func main() {
    var m map[str]i32 = map.new()
    m.put("a", 1)
    m.put("b", 2)
    for k in m {
        print.println(k)
    }
}
''', ["a", "b"]),

    # --- control flow ----------------------------------------------------------
    ("if_else_chain", '''import print

func classify(n i32) -> str {
    if n < 0 {
        return "neg"
    }
    if n == 0 {
        return "zero"
    }
    return "pos"
}

func main() {
    print.println(classify(i32(0) - i32(5)))
    print.println(classify(0))
    print.println(classify(5))
}
''', ["neg", "zero", "pos"]),

    ("while_and_break", '''import print

func main() {
    var i i32 = 0
    var total i32 = 0
    while i < 10 {
        i = i + 1
        if i == 4 {
            break
        }
        total = total + i
    }
    print.println(i)
    print.println(total)
}
''', ["4", "6"]),

    ("continue_skips_rest", '''import print

func main() {
    var total i32 = 0
    for var i i32 = 0; i < 5; i = i + 1 {
        if i % 2 == 0 {
            continue
        }
        total = total + i
    }
    print.println(total)
}
''', ["4"]),

    ("nested_loops", '''import print

func main() {
    var n i32 = 0
    for var i i32 = 0; i < 3; i = i + 1 {
        for var j i32 = 0; j < 3; j = j + 1 {
            n = n + 1
        }
    }
    print.println(n)
}
''', ["9"]),

    # --- enum / match: exhaustiveness is the feature, not a convenience ---------
    ("enum_match_values", '''import print

enum Color {
    Red,
    Green,
    Blue
}

func rank(c Color) -> i32 {
    match c {
        Red => { return 3 }
        Green => { return 2 }
        Blue => { return 1 }
    }
}

func main() {
    print.println(rank(Color.Red))
    print.println(rank(Color.Green))
    print.println(rank(Color.Blue))
}
''', ["3", "2", "1"]),

    ("option_match_binds_payload", '''import print

func doubled(o Option[i32]) -> i32 {
    match o {
        Some(v) => { return v * 2 }
        None => { return 0 }
    }
}

func main() {
    print.println(doubled(Some(21)))
    print.println(doubled(None))
}
''', ["42", "0"]),

    ("option_ok_none_helpers", '''import print
import option

func main() {
    var a = option.ok(5)
    var b = option.none()
    print.println(option.isSome(a))
    print.println(option.isSome(b))
    print.println(!option.isSome(b))
    print.println(option.unwrap(a))
}
''', ["true", "false", "true", "5"]),

    ("option_expect_on_none", '''import print
import option

func main() {
    var v, err = option.expect(option.none(), "not a number")
    print.println(v)
    print.println(err.code)
}
''', ["0", "1"]),

    # --- Err / error edge ------------------------------------------------------
    ("err_constructor_and_fields", '''import print

func main() {
    var e Err = Err(7, "seven")
    print.println(e.code)
    print.println(e.msg)
    var z Err = nil
    print.println(z.code)
    print.println(z.msg)
}
''', ["7", "seven", "0", ""]),

    ("check_and_handle", '''import print

func parse(v i32) -> (i32, Err) {
    if v < 0 {
        return 0, Err(2, "negative")
    }
    return v * 2, nil
}

func run(v i32) -> (i32, Err) {
    handle err {
        return err
    }
    var out = check parse(v)
    return out, nil
}

func main() {
    var ok, e1 = run(21)
    print.println(ok)
    print.println(e1.code)
    var bad, e2 = run(i32(0) - i32(1))
    print.println(bad)
    print.println(e2.code)
    print.println(e2.msg)
}
''', ["42", "0", "0", "2", "negative"]),

    ("err_propagates_by_hand", '''import print

func inner(fail bool) -> Err {
    if fail {
        return Err(3, "inner failed")
    }
    return nil
}

func outer(fail bool) -> Err {
    var e = inner(fail)
    if e.code != 0 {
        return e
    }
    return nil
}

func main() {
    print.println(outer(false).code)
    print.println(outer(true).code)
    print.println(outer(true).msg)
}
''', ["0", "3", "inner failed"]),

    # --- defer / errdefer ------------------------------------------------------
    ("defer_runs_lifo", '''import print

func note(tag str) {
    print.println(tag)
}

func f() {
    defer note("first")
    defer note("second")
    note("body")
}

func main() {
    f()
}
''', ["body", "second", "first"]),

    ("errdefer_only_on_error", '''import print

func note(tag str) {
    print.println(tag)
}

func f(fail bool) -> Err {
    errdefer note("cleanup")
    if fail {
        return Err(1, "boom")
    }
    return nil
}

func main() {
    var a = f(false)
    print.println(a.code)
    var b = f(true)
    print.println(b.code)
}
''', ["0", "cleanup", "1"]),

    ("defer_captures_arguments", '''import print

func emit(tag str, n i32) {
    print.println(tag)
    print.println(n)
}

func f(n i32) {
    var tag str = "done"
    defer emit(tag, n)
    print.println("start")
}

func main() {
    f(9)
}
''', ["start", "done", "9"]),

    # --- fixed arrays: inline storage, zero-initialised, index-checked ---------
    ("fixed_array_basics", '''import print

func main() {
    var a [i32; 3] = [1, 2, 3]
    print.println(a.len())
    print.println(a[0])
    print.println(a[2])
    a[1] = 20
    print.println(a[1])
    for var i usize = 0; i < a.len(); i = i + 1 {
        print.println(a[i])
    }
}
''', ["3", "1", "3", "20", "1", "20", "3"]),

    ("fixed_array_zero_filled", '''import print

func main() {
    var a [i32; 4]
    print.println(a.len())
    print.println(a.isEmpty())
    for var i usize = 0; i < a.len(); i = i + 1 {
        print.println(a[i])
    }
}
''', ["4", "false", "0", "0", "0", "0"]),

    # --- Unicode: len is bytes, codepoints is not -------------------------------
    ("str_len_is_bytes", '''import print
import str

func main() {
    print.println("abc".len())
    print.println("héllo".len())
    print.println(str.codepoints("héllo").len())
    print.println(str.codepoints("中文").len())
}
''', ["3", "6", "5", "2"]),

    ("str_utf8_at_decodes", '''import print

func main() {
    var cp, width = "中".utf8At(0)
    print.println(cp)
    print.println(width)
    var tail, tailWidth = "中a".utf8At(3)
    print.println(tail)
    print.println(tailWidth)
}
''', ["20013", "3", "97", "1"]),

    ("str_codepoints_values", '''import print
import str

func main() {
    var cps = str.codepoints("a中")
    print.println(cps.len())
    print.println(cps[0])
    print.println(cps[1])
}
''', ["2", "97", "20013"]),

    # --- misc semantics ---------------------------------------------------------
    ("bool_logic", '''import print

func main() {
    print.println(true && false)
    print.println(true || false)
    print.println(!true)
    print.println(1 < 2)
    print.println(2 <= 2)
    print.println(3 > 4)
    print.println(4 >= 4)
    print.println(1 == 1)
    print.println(1 != 1)
}
''', ["false", "true", "false", "true", "true", "false", "true", "true", "false"]),

    ("integer_arithmetic", '''import print

func main() {
    print.println(7 / 2)
    print.println(7 % 2)
    print.println(i32(7) - i32(9))
    print.println(i32(6) * i32(7))
    print.println(i64(100000) * i64(100000))
}
''', ["3", "1", "-2", "42", "10000000000"]),

    ("shadowing_is_block_scoped", '''import print

func main() {
    var x i32 = 1
    {
        var x i32 = 2
        print.println(x)
    }
    print.println(x)
}
''', ["2", "1"]),

    ("recursion", '''import print

func fact(n i32) -> i32 {
    if n <= 1 {
        return 1
    }
    return n * fact(n - 1)
}

func main() {
    print.println(fact(5))
    print.println(fact(1))
}
''', ["120", "1"]),

    # --- R1 新增锚点：region 块 / 复合字面量 / for 三形态 / lambda / match 双形态 /
    # str 视图 / `_` 弃位。这些 .aic 在 R1 就手写落盘，.out 由本脚本独立算出 ---
    ("region_block", '''import print

class Buf {
    var n i32
}

func use(b Buf) -> i32 {
    return b.n
}

func main() -> Err {
    region {
        var b = Buf{n: 3}
        print.println(use(b))
    }
    region {
        region {
            print.println("nested")
        }
    }
    return Err(0, "")
}
''', ["3", "nested"]),

    ("composite_literal", '''import print

class Point {
    var x i32
    var y i32
}

class Line {
    var a Point
    var b Point
}

func main() {
    var p = Point{x: 1, y: 2}
    var q = Point{x: 5}
    var l = Line{a: p, b: q}
    print.println(p.x)
    print.println(q.x)
    print.println(l.a.y)
    print.println(l.b.y)
}
''', ["1", "5", "2", "0"]),

    ("for_forms", '''import print

func main() {
    var xs i32[] = [10, 20, 30]
    for x in xs {
        print.println(x)
    }
    for i, x in xs {
        print.println(i)
        print.println(x)
    }
    for i in 0..3 {
        print.println(i)
    }
    var n i32 = 3
    var i i32 = 0
    for i < n {
        print.println("cond")
        i = i + 1
    }
    for var j i32 = 0; j < 2; j += 1 {
        print.println("cstyle")
    }
    for _, x in xs {
        print.println(x)
    }
}
''', ["10", "20", "30",
      "0", "10", "1", "20", "2", "30",
      "0", "1", "2",
      "cond", "cond", "cond",
      "cstyle", "cstyle",
      "10", "20", "30"]),

    ("lambda", '''import print

func apply(g (i32) -> i32, v i32) -> i32 {
    return g(v)
}

func add2(a (i32, i32) -> i32, x i32, y i32) -> i32 {
    return a(x, y)
}

func main() {
    var inc (i32) -> i32 = (x) => x + 1
    print.println(apply(inc, 41))
    print.println(apply(x => x * 2, 21))
    var sum (i32, i32) -> i32 = (a, b) => {
        return a + b
    }
    print.println(add2(sum, 20, 22))
}
''', ["42", "42", "42"]),

    ("match_expr", '''import print

enum Color { Red, Green, Blue }

func name(c Color) -> str {
    return match c {
        Color.Red => "red",
        Color.Green => "green",
        Color.Blue => "blue",
    }
}

func code(c Color) -> i32 {
    return match c {
        Color.Red => 1,
        Color.Green => 2,
        Color.Blue => 3,
    }
}

func main() {
    print.println(name(Color.Red))
    print.println(code(Color.Green))
    match Color.Blue {
        Color.Red => print.println("r"),
        Color.Green => print.println("g"),
        Color.Blue => print.println("b"),
    }
}
''', ["red", "2", "b"]),

    ("str_view", '''import print

func main() {
    var s str = "hello"
    var v str = s[1..3]
    print.println(v)
    print.println(s[0..5])
    print.println(s.len())
    print.println(s[2..2].len())
}
''', ["el", "hello", "5", "0"]),

    ("blank_discard", '''import print

func pair() -> (i32, Err) {
    return 7, Err(0, "")
}

func main() {
    var v, _ = pair()
    print.println(v)
    var _, err = pair()
    print.println(err.code)
}
''', ["7", "0"]),

    # --- R1 手写锚点：源文已在册并被 ANCHOR_SOURCE_SHA256 钉死，此处只声明
    # --- **独立算出**的期望（脚本不重写这些源文；期望与实现对不上就报错）。
    ("region_block", "", ["3", "nested"]),
    ("composite_literal", "", ["1", "5", "2", "0"]),
    ("for_forms", "", ["10", "20", "30",
                       "0", "10", "1", "20", "2", "30",
                       "0", "1", "2",
                       "cond", "cond", "cond",
                       "cstyle", "cstyle",
                       "10", "20", "30"]),
    ("lambda", "", ["42", "42", "42"]),
    ("match_expr", "", ["red", "2", "b"]),
    ("str_view", "", ["el", "hello", "5", "0"]),
    ("blank_discard", "", ["7", "0"]),
]

# R1 期手写落盘的 ok 锚点：.out 同样必须独立算出，且源文一旦改动即报错，
# 防止「实现改了、期望跟着改」这种最隐蔽的放松（黄金执行规划 §六、防坑 9）。
# 键 = testdata/ok 下的文件主干名，值 = 该 .aic 文本（LF 归一）的 sha256。
ANCHOR_SOURCE_SHA256 = {
    "715_region_block": "3f00998bd397ac1326507ddab3fa261689154b8dce77d3db15613adbc150b67b",
    "716_composite_literal": "531a5f18d16df90613e17b4510d962e3fcf5c605310f84fc8fee3544170fde45",
    "717_for_forms": "cb927784855ebf69ad4d0e914d8c0e4073b8ec46436e025e7ca73f2200d518c6",
    "718_lambda": "8bf192e39c6444630229b4ca8048c389908be520a9e8c2b1bc18568014081ab5",
    "719_match_expr": "6a8d47e87e8dbae317e1a7432407668e477645ea3bc66ee1d471debaf2cc5b11",
    "720_str_view": "6f73f1d3bf8c26c1ad2038a40e7b629f7b980cbd76646bca241aa0a1dc8ea412",
    "721_blank_discard": "40cf321ebefb2d4490b4f36910c3f37459f0820e9f1e90c6acd1c98b2f1f7895",
}


def source_sha256(text):
    return hashlib.sha256(text.replace("\r\n", "\n").encode("utf-8")).hexdigest()

# (topic, source, expected substring on stderr)
TRAP_CASES = [
    ("divide_by_zero", '''import print

func main() {
    var a i64 = 1
    var b i64 = 0
    print.println(a / b)
}
''', "division by zero"),

    ("divide_by_zero_modulo", '''import print

func main() {
    var a i64 = 7
    var b i64 = 0
    print.println(a % b)
}
''', "division by zero"),

    ("zero_container_write", '''import print

func main() {
    var xs i32[]
    print.println("unreachable")
    xs[0] = 7
}
''', "零值容器"),

    ("list_index_out_of_bounds_read", '''import print

func main() {
    var xs i64[] = [i64(1), i64(2)]
    print.println(xs[5])
}
''', "out of bounds"),

    ("fixed_array_out_of_bounds", '''import print

func main() {
    var a [i32; 2] = [1, 2]
    print.println(a[7])
}
''', "out of bounds"),

    ("str_index_out_of_bounds", '''import print

func main() {
    var i usize = 4
    print.println("ab"[i])
}
''', "out of bounds"),

    ("str_slice_out_of_range", '''import print
import str

func main() {
    print.println(str.slice("abc", 1, 9))
}
''', "slice"),
]


def next_index(directory):
    used = []
    for name in os.listdir(directory):
        if name.endswith(".aic"):
            stem = name.split("_", 1)[0]
            if stem.isdigit():
                used.append(int(stem))
    return max(used) + 1 if used else 1


def source_kind(stem, dirpath):
    """区分「本脚本自有语料」与「R1 手写锚点」——后者不重写源文，只补 .out。

    自建语料：源文由本脚本写入，因此只校验字节数（防误覆盖）。
    手写锚点：源文已有且带 sha256 钉死，只有 .out 由本脚本补齐。
    """
    path = os.path.join(dirpath, stem + ".aic")
    if not os.path.exists(path):
        return "create"
    text = io.open(path, encoding="utf-8").read()
    pinned = ANCHOR_SOURCE_SHA256.get(stem)
    if pinned:
        return "pin" if source_sha256(text) == pinned else "drift"
    return "own"


def main():
    if not os.path.exists(AIC):
        print("bin/aic.exe 不存在：先 go build -o bin/aic.exe ./cmd/aic", file=sys.stderr)
        return 1

    failures = []
    written = 0
    anchored = 0

    idx = next_index(OK_DIR)
    for topic, src, expected in OK_CASES:
        # 与既有锚点同名的条目：沿用既有编号，不新开一条
        existing = None
        for name in sorted(os.listdir(OK_DIR)):
            if name.endswith(".aic") and name.split("_", 1)[-1] == topic + ".aic":
                existing = name[: -len(".aic")]
                break
        if existing:
            stem = existing
        else:
            stem = "%03d_%s" % (idx, topic)
            idx += 1
        aic_path = os.path.join(OK_DIR, stem + ".aic")
        kind = source_kind(stem, OK_DIR)
        if kind == "drift":
            failures.append((stem, "源文已被改动，与 ANCHOR_SOURCE_SHA256 不符："
                                   "期望必须重新独立推导，不得跟着实现改（防坑 9）"))
            continue
        if kind == "create":
            io.open(aic_path, "w", encoding="utf-8", newline="\n").write(src)
        else:
            anchored += 1
        out_exe = os.path.join(os.environ.get("TEMP", "."), "aic_ok_check.exe")
        proc = subprocess.run([AIC, "build", os.path.join("testdata", "ok", stem + ".aic"),
                               "-o", out_exe],
                              cwd=ROOT, capture_output=True)
        if proc.returncode != 0:
            failures.append((stem, "构建失败：\n" +
                             (proc.stderr or proc.stdout).decode("utf-8", "replace")))
            if kind == "create":
                os.remove(aic_path)
            continue
        run = subprocess.run([out_exe], capture_output=True)
        text = run.stdout.decode("utf-8", "replace").replace("\r\n", "\n")
        # Only the final newline is formatting. strip() would also eat the empty line
        # that `println("")` legitimately produces.
        if text.endswith("\n"):
            text = text[:-1]
        got = text.split("\n")
        if run.returncode != 0:
            failures.append((stem, "运行退出码 %d\n%s" % (run.returncode, run.stderr.decode("utf-8", "replace"))))
            if kind == "create":
                os.remove(aic_path)
            continue
        if got != expected:
            failures.append((stem, "输出与独立计算的期望不符\n  期望 %r\n  实际 %r" % (expected, got)))
            if kind == "create":
                os.remove(aic_path)
            continue
        io.open(os.path.join(OK_DIR, stem + ".out"), "w", encoding="utf-8", newline="\n").write(
            "\n".join(expected) + "\n")
        written += 1

    idx = next_index(TRAP_DIR)
    for topic, src, want in TRAP_CASES:
        stem = "%03d_%s" % (idx, topic)
        idx += 1
        aic_path = os.path.join(TRAP_DIR, stem + ".aic")
        io.open(aic_path, "w", encoding="utf-8", newline="\n").write(src)
        out_exe = os.path.join(os.environ.get("TEMP", "."), "aic_trap_check.exe")
        proc = subprocess.run([AIC, "build", os.path.join("testdata", "trap", stem + ".aic"),
                               "-o", out_exe],
                              cwd=ROOT, capture_output=True)
        if proc.returncode != 0:
            failures.append((stem, "构建失败：\n" +
                             (proc.stderr or proc.stdout).decode("utf-8", "replace")))
            os.remove(aic_path)
            continue
        run = subprocess.run([out_exe], capture_output=True)
        err = run.stderr.decode("utf-8", "replace")
        if run.returncode == 0:
            failures.append((stem, "本应 trap 却正常退出"))
            os.remove(aic_path)
            continue
        if want not in err:
            failures.append((stem, "trap 诊断里没有 %r\n%s" % (want, err)))
            os.remove(aic_path)
            continue
        io.open(os.path.join(TRAP_DIR, stem + ".trap"), "w", encoding="utf-8", newline="\n").write(
            err.strip().splitlines()[0] + "\n")
        written += 1

    print("写入 %d 条语料（其中手写锚点沿用源文 %d 条）" % (written, anchored))
    if failures:
        print("未写入（期望与实际不符，必须逐条查明，不得改期望迁就实现）：")
        for stem, why in failures:
            print("  %s: %s" % (stem, why))
    return 1 if failures else 0


if __name__ == "__main__":
    sys.exit(main())