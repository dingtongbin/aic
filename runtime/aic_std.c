/* AIC std 绑定实现（核心设计 §十四）——分配操作 + os + testing + 数值转换落点。 */

#include "aic_std.h"
#include "aic_plat.h"
#include <stdlib.h>

/* ============================================================================
 * 数值转换（§二.4：显式转换 T(x) 仅数值↔数值；运行期窄化越界 = trap）
 * 浮点 → 整数：向零截断（C 的转换语义），越界/NaN = trap。
 * 整数窄化：值落在目标区间外 = trap。字面量超界由编译期范围证明拦下，走不到这里。
 * ==========================================================================*/
#define AIC_NUM_F2I(fn, CT, LO, HI)                                            \
    CT fn(aic_f64 v, const char *file, int line) {                            \
        if (v != v) {                                                         \
            aic_trap(AIC_TRAP_INTEGER_OVERFLOW, file, line);                  \
        }                                                                     \
        if (v < (aic_f64)(LO) || v > (aic_f64)(HI)) {                         \
            aic_trap(AIC_TRAP_INTEGER_OVERFLOW, file, line);                  \
        }                                                                     \
        return (CT)v;                                                         \
    }

AIC_NUM_F2I(aic_num_f2i_i8, aic_i8, -128, 127)
AIC_NUM_F2I(aic_num_f2i_i16, aic_i16, -32768, 32767)
AIC_NUM_F2I(aic_num_f2i_i32, aic_i32, -2147483648.0, 2147483647.0)
AIC_NUM_F2I(aic_num_f2i_i64, aic_i64, -9223372036854775808.0, 9223372036854775807.0)
AIC_NUM_F2I(aic_num_f2i_u8, aic_u8, 0, 255)
AIC_NUM_F2I(aic_num_f2i_u16, aic_u16, 0, 65535)
AIC_NUM_F2I(aic_num_f2i_u32, aic_u32, 0, 4294967295.0)
AIC_NUM_F2I(aic_num_f2i_u64, aic_u64, 0, 18446744073709551615.0)

/* 有符号源 → 更窄（含无符号目标）：按数值区间判定，非按位提升（§二.4）。 */
#define AIC_NARROW_I64(fn, LO, HI)                                            \
    aic_i64 fn(aic_i64 v, const char *file, int line) {                       \
        if (v < (LO) || v > (HI)) {                                           \
            aic_trap(AIC_TRAP_INTEGER_OVERFLOW, file, line);                  \
        }                                                                     \
        return v;                                                             \
    }

AIC_NARROW_I64(aic_num_narrow_i64_to_i8, -128, 127)
AIC_NARROW_I64(aic_num_narrow_i64_to_i16, -32768, 32767)
AIC_NARROW_I64(aic_num_narrow_i64_to_i32, -2147483648LL, 2147483647LL)
AIC_NARROW_I64(aic_num_narrow_i64_to_u8, 0, 255)
AIC_NARROW_I64(aic_num_narrow_i64_to_u16, 0, 65535)
AIC_NARROW_I64(aic_num_narrow_i64_to_u32, 0, 4294967295LL)

/* 无符号源 → 更窄：上界判定即可（无符号域无负值）。 */
#define AIC_NARROW_U64(fn, HI)                                                \
    aic_u64 fn(aic_u64 v, const char *file, int line) {                       \
        if (v > (aic_u64)(HI)) {                                              \
            aic_trap(AIC_TRAP_INTEGER_OVERFLOW, file, line);                  \
        }                                                                     \
        return v;                                                             \
    }

AIC_NARROW_U64(aic_num_narrow_u64_to_u8, 255)
AIC_NARROW_U64(aic_num_narrow_u64_to_u16, 65535)
AIC_NARROW_U64(aic_num_narrow_u64_to_u32, 4294967295ULL)
AIC_NARROW_U64(aic_num_narrow_u64_to_i8, 127)
AIC_NARROW_U64(aic_num_narrow_u64_to_i16, 32767)
AIC_NARROW_U64(aic_num_narrow_u64_to_i32, 2147483647ULL)


/* --- str 解析辅助 ----------------------------------------------------------- */
static bool aic_str_try_i64(aic_str s, aic_i64 *out) {
    aic_str t = aic_str_trim(s);
    if (t.len == 0) return false;
    aic_usize i = 0;
    bool neg = false;
    if (t.p[0] == '-' || t.p[0] == '+') {
        neg = t.p[0] == '-';
        i = 1;
        if (t.len == 1) return false;
    }
    aic_u64 acc = 0;
    for (; i < t.len; i++) {
        if (t.p[i] < '0' || t.p[i] > '9') return false;
        acc = acc * 10 + (aic_u64)(t.p[i] - '0');
    }
    *out = neg ? -(aic_i64)acc : (aic_i64)acc;
    return true;
}

static bool aic_str_try_f64(aic_str s, aic_f64 *out) {
    aic_str t = aic_str_trim(s);
    if (t.len == 0 || t.len >= 128) return false;
    char buf[128];
    memcpy(buf, t.p, t.len);
    buf[t.len] = 0;
    char *end = NULL;
    double v = strtod(buf, &end);
    if (end != buf + t.len) return false;
    *out = (aic_f64)v;
    return true;
}

/* --- str 分配操作 ----------------------------------------------------------- */
aic_list_str *aic_std_str_split(aic_str s, aic_str sep, const char *file, int line) {
    if (sep.len == 0) {
        aic_trap(AIC_TRAP_ASSERT_FAILED, file, line);
    }
    aic_usize count = 1, i = 0;
    while (i + sep.len <= s.len) {
        if (memcmp(s.p + i, sep.p, sep.len) == 0) {
            count++;
            i += sep.len;
        } else {
            i++;
        }
    }
    aic_str *items = (aic_str *)aic_alloc_in(0, count * sizeof(aic_str), (aic_u32)line);
    aic_usize n = 0, start = 0;
    i = 0;
    while (i + sep.len <= s.len) {
        if (memcmp(s.p + i, sep.p, sep.len) == 0) {
            items[n++] = (aic_str){ s.p + start, i - start };
            i += sep.len;
            start = i;
        } else {
            i++;
        }
    }
    items[n++] = (aic_str){ s.p + start, s.len - start };
    return aic_list_from_str(items, n, (aic_u32)line);
}

aic_r_u32_usize aic_std_str_utf8_at(aic_str s, aic_usize i, const char *file, int line) {
    aic_usize pos = i;
    aic_u32 cp = aic_str_utf8_next(s, &pos, file, line);
    return (aic_r_u32_usize){ cp, pos - i };
}

aic_list_u32 *aic_std_str_codepoints(aic_str s, const char *file, int line) {
    aic_usize cap = 0, pos = 0;
    while (pos < s.len) {
        aic_str_utf8_next(s, &pos, file, line);
        cap++;
    }
    if (cap == 0) cap = 1;
    aic_u32 *out = (aic_u32 *)aic_alloc_in(0, cap * sizeof(aic_u32), (aic_u32)line);
    aic_usize n = 0;
    pos = 0;
    while (pos < s.len) {
        aic_usize before = pos;
        out[n++] = aic_str_utf8_next(s, &pos, file, line);
        if (pos == before) pos = before + 1; /* 非法字节：绝不空转 */
    }
    return aic_list_from_u32(out, n, (aic_u32)line);
}

aic_str aic_std_str_from_i64(aic_i64 v, aic_u32 line) { return aic_fmt_i64(v, line); }

aic_str aic_std_str_from_u64(aic_u64 v, aic_u32 line) { return aic_fmt_u64(v, line); }

aic_str aic_std_str_from_f64(aic_f64 v, aic_u32 line) { return aic_fmt_f64(v, line); }

aic_str aic_std_str_from_bool(bool v) { return aic_fmt_bool(v); }

aic_r_i64_err aic_std_str_to_i64(aic_str s, const char *file, int line) {
    aic_i64 v = 0;
    if (!aic_str_try_i64(s, &v)) {
        (void)file;
        (void)line;
        return (aic_r_i64_err){ 0, (aic_Err){ 1, (aic_str){ "invalid integer", 15 } } };
    }
    return (aic_r_i64_err){ v, AIC_ERR_NONE };
}

aic_r_f64_err aic_std_str_to_f64(aic_str s, const char *file, int line) {
    aic_f64 v = 0;
    if (!aic_str_try_f64(s, &v)) {
        (void)file;
        (void)line;
        return (aic_r_f64_err){ 0, (aic_Err){ 1, (aic_str){ "invalid float", 13 } } };
    }
    return (aic_r_f64_err){ v, AIC_ERR_NONE };
}

/* --- os --------------------------------------------------------------------- */
aic_list_str *aic_std_os_args(const char *file, int line) {
    aic_usize argc = aic_os_args_len();
    aic_str *items = (aic_str *)aic_alloc_in(0, (argc ? argc : 1) * sizeof(aic_str), (aic_u32)line);
    aic_usize i;
    for (i = 0; i < argc; i++) {
        items[i] = aic_os_args_at(i);
    }
    (void)file;
    return aic_list_from_str(items, argc, (aic_u32)line);
}

aic_r_str_err aic_std_os_read_file(aic_str path, const char *file, int line) {
    aic_Err err = AIC_ERR_NONE;
    aic_str s = aic_os_read_file(path, &err);
    (void)file;
    (void)line;
    return (aic_r_str_err){ s, err };
}

aic_r_str_err aic_std_os_read_stdin(const char *file, int line) {
    aic_Err err = AIC_ERR_NONE;
    aic_str s = aic_os_read_stdin(&err);
    (void)file;
    (void)line;
    return (aic_r_str_err){ s, err };
}

/* --- testing：失败即 trap，消息含两值（§十一）------------------------------- */
/* 整数走自实现十进制写 stderr（禁 libc printf 的整数格式化：跨平台 int64_t
 * 的 printf 长度修饰符并不统一，例如 Linux 上 int64_t 是 long、Windows 上是
 * long long —— 而裸 long 又是 C10 禁止的拼写）。 */
static void aic_err_i64(aic_i64 v) {
    aic_u8 buf[24];
    aic_usize n = aic_i64_fmt(buf, v);
    fwrite(buf, 1, n, stderr);
}

void aic_std_test_assert(bool cond, aic_str msg, const char *file, int line) {
    if (cond) return;
    fprintf(stderr, "assertion failed: %.*s\n", (int)msg.len, msg.p);
    fprintf(stderr, "  at %s:%d\n", file, line);
    fflush(stderr);
    aic_trap(AIC_TRAP_ASSERT_FAILED, file, line);
}

void aic_std_test_eq_i64(aic_i64 a, aic_i64 b, aic_str msg, const char *file, int line) {
    if (a == b) return;
    fprintf(stderr, "assertion failed: %.*s (got ", (int)msg.len, msg.p);
    aic_err_i64(a);
    fputs(", want ", stderr);
    aic_err_i64(b);
    fputs(")\n", stderr);
    fprintf(stderr, "  at %s:%d\n", file, line);
    fflush(stderr);
    aic_trap(AIC_TRAP_ASSERT_FAILED, file, line);
}

void aic_std_test_eq_str(aic_str a, aic_str b, aic_str msg, const char *file, int line) {
    if (aic_str_eq(a, b)) return;
    fprintf(stderr, "assertion failed: %.*s\n", (int)msg.len, msg.p);
    fprintf(stderr, "  got  %.*s\n", (int)a.len, a.p);
    fprintf(stderr, "  want %.*s\n", (int)b.len, b.p);
    fprintf(stderr, "  at %s:%d\n", file, line);
    fflush(stderr);
    aic_trap(AIC_TRAP_ASSERT_FAILED, file, line);
}

/* 比较类断言（gt/lt/eq 的浮点与整数两套）：失败即 trap 并打印两值。 */
static void aic_test_fail_i64(const char *op, aic_i64 a, aic_i64 b, aic_str msg,
                              const char *file, int line) {
    fprintf(stderr, "assertion failed: %.*s (%s: got ", (int)msg.len, msg.p, op);
    aic_err_i64(a);
    fputs(", want ", stderr);
    aic_err_i64(b);
    fputs(")\n", stderr);
    fprintf(stderr, "  at %s:%d\n", file, line);
    fflush(stderr);
    aic_trap(AIC_TRAP_ASSERT_FAILED, file, line);
}

static void aic_test_fail_f64(const char *op, aic_f64 a, aic_f64 b, aic_str msg,
                              const char *file, int line) {
    fprintf(stderr, "assertion failed: %.*s (%s)\n", (int)msg.len, msg.p, op);
    fprintf(stderr, "  got  %g\n  want %g\n", (double)a, (double)b);
    fprintf(stderr, "  at %s:%d\n", file, line);
    fflush(stderr);
    aic_trap(AIC_TRAP_ASSERT_FAILED, file, line);
}

static void aic_test_fail_str(const char *op, aic_str a, aic_str b, aic_str msg,
                              const char *file, int line) {
    fprintf(stderr, "assertion failed: %.*s (%s)\n", (int)msg.len, msg.p, op);
    fprintf(stderr, "  got  %.*s\n  want %.*s\n", (int)a.len, a.p, (int)b.len, b.p);
    fprintf(stderr, "  at %s:%d\n", file, line);
    fflush(stderr);
    aic_trap(AIC_TRAP_ASSERT_FAILED, file, line);
}

void aic_std_test_eq_f64(aic_f64 a, aic_f64 b, aic_str msg, const char *file, int line) {
    if (a == b) return;
    aic_test_fail_f64("==", a, b, msg, file, line);
}

void aic_std_test_gt_i64(aic_i64 a, aic_i64 b, aic_str msg, const char *file, int line) {
    if (a > b) return;
    aic_test_fail_i64(">", a, b, msg, file, line);
}

void aic_std_test_lt_i64(aic_i64 a, aic_i64 b, aic_str msg, const char *file, int line) {
    if (a < b) return;
    aic_test_fail_i64("<", a, b, msg, file, line);
}

void aic_std_test_gt_f64(aic_f64 a, aic_f64 b, aic_str msg, const char *file, int line) {
    if (a > b) return;
    aic_test_fail_f64(">", a, b, msg, file, line);
}

void aic_std_test_lt_f64(aic_f64 a, aic_f64 b, aic_str msg, const char *file, int line) {
    if (a < b) return;
    aic_test_fail_f64("<", a, b, msg, file, line);
}

void aic_std_test_gt_str(aic_str a, aic_str b, aic_str msg, const char *file, int line) {
    if (aic_str_cmp(a, b) > 0) return;
    aic_test_fail_str(">", a, b, msg, file, line);
}

void aic_std_test_lt_str(aic_str a, aic_str b, aic_str msg, const char *file, int line) {
    if (aic_str_cmp(a, b) < 0) return;
    aic_test_fail_str("<", a, b, msg, file, line);
}

/* --- time（§十四 后续圆次提前落地；实现全部经平台层 aic_plat.h） ---------------- */

aic_i64 aic_std_time_now(void) { return aic_plat_now_ms(); }

aic_i64 aic_std_time_monotonic(void) { return aic_plat_monotonic_ms(); }

void aic_std_time_sleep(aic_i64 ms) { aic_plat_sleep_ms(ms); }
