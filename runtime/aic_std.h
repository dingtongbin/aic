/* ============================================================================
 * AIC std 绑定（核心设计 §十四；边界语义 R4 以语料冻结【O9】）。
 * str — len() 是字节数 O(1)；码点走 utf8At/codepoints；无 codepointLen（防加糖）。
 * os  — 同步最小面（D17）；两段式异步属圆 3。
 * math— abs/min/max/floor/ceil/sqrt/pow。
 * 本层只提供 C 落点；包/函数面与 mangling 由 emit 生成。不含 option（语言级 enum）。
 * ==========================================================================*/
#ifndef AIC_STD_H
#define AIC_STD_H

#include "aic_l1.h"
#include <math.h>
#include <stdio.h>
#include <string.h>

/* --- 多返回 / Err 结果形状（§六）-------------------------------------------- */
typedef struct { aic_str _0; aic_Err _1; } aic_r_str_err;
typedef struct { aic_i64 _0; aic_Err _1; } aic_r_i64_err;
typedef struct { aic_f64 _0; aic_Err _1; } aic_r_f64_err;
typedef struct { aic_u32 _0; aic_usize _1; } aic_r_u32_usize;

/* --- str：无分配视图操作（返回视图，永不悬垂）------------------------------- */
static inline bool aic_str_contains(aic_str hay, aic_str needle) {
    if (needle.len == 0) return true;
    if (needle.len > hay.len) return false;
    aic_usize i;
    for (i = 0; i + needle.len <= hay.len; i++) {
        if (memcmp(hay.p + i, needle.p, needle.len) == 0) return true;
    }
    return false;
}

static inline bool aic_str_starts_with(aic_str s, aic_str prefix) {
    if (prefix.len > s.len) return false;
    if (prefix.len == 0) return true;
    return memcmp(s.p, prefix.p, prefix.len) == 0;
}

static inline bool aic_str_ends_with(aic_str s, aic_str suffix) {
    if (suffix.len > s.len) return false;
    if (suffix.len == 0) return true;
    return memcmp(s.p + (s.len - suffix.len), suffix.p, suffix.len) == 0;
}

/* indexOf：首个出现的字节偏移；未命中 = (usize)-1。 */
/* str 的字节下标 = trap（§二.3：str 字节不越界；视图只读故无写入路径）。 */
static inline aic_u8 aic_str_at(aic_str s, aic_usize i, const char *file, int line) {
    if (i >= s.len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line);
    return (aic_u8)s.p[i];
}

/* str 视图 s[lo..hi]：**零拷贝**（字节恒在任务区域，视图永不悬垂，§二.5）；
 * lo <= hi <= len 否则 trap。单字节视图（`for ch in s` 的元素）走同一条：
 * 循环条件已保证 lo < len，这里的检查不会触发，但语义只留一处。 */
static inline aic_str aic_str_view(aic_str s, aic_usize lo, aic_usize hi,
                                   const char *file, int line) {
    if (lo > hi || hi > s.len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line);
    aic_str v;
    v.p = s.p + lo;
    v.len = hi - lo;
    return v;
}

static inline aic_usize aic_str_index_of(aic_str hay, aic_str needle) {
    if (needle.len == 0) return 0;
    if (needle.len > hay.len) return (aic_usize)-1;
    aic_usize i;
    for (i = 0; i + needle.len <= hay.len; i++) {
        if (memcmp(hay.p + i, needle.p, needle.len) == 0) return i;
    }
    return (aic_usize)-1;
}

/* indexOf 的语言级返回 = i64，未命中 = -1（语料 682 冻结）。
 * usize 的 (usize)-1 打印成 18446744073709551615，不是 -1——必须在这里转。 */
static inline aic_i64 aic_str_index_of_i64(aic_str hay, aic_str needle) {
    aic_usize i = aic_str_index_of(hay, needle);
    return (i == (aic_usize)-1) ? (aic_i64)-1 : (aic_i64)i;
}

/* sub：视图切片，越界 trap（不夹取）。 */
static inline aic_str aic_str_sub(aic_str s, aic_usize from, aic_usize to,
                                  const char *file, int line) {
    if (from > to || to > s.len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line);
    return (aic_str){ s.p + from, to - from };
}

/* trim：去两端 ASCII 空白，返回视图。 */
static inline aic_str aic_str_trim(aic_str s) {
    aic_usize from = 0, to = s.len;
    while (from < to && (s.p[from] == ' ' || s.p[from] == '\t' ||
                         s.p[from] == '\n' || s.p[from] == '\r')) {
        from++;
    }
    while (to > from && (s.p[to - 1] == ' ' || s.p[to - 1] == '\t' ||
                         s.p[to - 1] == '\n' || s.p[to - 1] == '\r')) {
        to--;
    }
    return (aic_str){ s.p + from, to - from };
}

/* utf8At 解码一个码点并推进 *pos；非法字节 = U+FFFD 前进 1（不越界）。 */
static inline aic_u32 aic_str_utf8_next(aic_str s, aic_usize *pos,
                                        const char *file, int line) {
    if (*pos >= s.len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line);
    const aic_u8 *b = (const aic_u8 *)s.p;
    aic_usize left = s.len - *pos;
    aic_u32 cp;
    aic_usize n;
    if (b[*pos] < 0x80) {
        cp = b[*pos]; n = 1;
    } else if ((b[*pos] & 0xE0) == 0xC0 && left >= 2) {
        cp = (aic_u32)(b[*pos] & 0x1F) << 6 | (aic_u32)(b[*pos + 1] & 0x3F); n = 2;
    } else if ((b[*pos] & 0xF0) == 0xE0 && left >= 3) {
        cp = (aic_u32)(b[*pos] & 0x0F) << 12 | (aic_u32)(b[*pos + 1] & 0x3F) << 6 |
             (aic_u32)(b[*pos + 2] & 0x3F); n = 3;
    } else if ((b[*pos] & 0xF8) == 0xF0 && left >= 4) {
        cp = (aic_u32)(b[*pos] & 0x07) << 18 | (aic_u32)(b[*pos + 1] & 0x3F) << 12 |
             (aic_u32)(b[*pos + 2] & 0x3F) << 6 | (aic_u32)(b[*pos + 3] & 0x3F); n = 4;
    } else {
        cp = 0xFFFD; n = 1;
    }
    *pos += n;
    return cp;
}

/* --- 数值转换的 trap 落点（§二.4）------------------------------------------ */
/* 浮点 → 整数：向零截断；越界/NaN = trap。emit 生成 aic_num_f2i_<target>(v, file, line)。 */
aic_i64 aic_num_f2i_i64(aic_f64 v, const char *file, int line);
aic_i32 aic_num_f2i_i32(aic_f64 v, const char *file, int line);
aic_i16 aic_num_f2i_i16(aic_f64 v, const char *file, int line);
aic_i8  aic_num_f2i_i8(aic_f64 v, const char *file, int line);
aic_u64 aic_num_f2i_u64(aic_f64 v, const char *file, int line);
aic_u32 aic_num_f2i_u32(aic_f64 v, const char *file, int line);
aic_u16 aic_num_f2i_u16(aic_f64 v, const char *file, int line);
aic_u8  aic_num_f2i_u8(aic_f64 v, const char *file, int line);

/* 整数窄化：运行期值越界 = trap（emit 生成 aic_num_narrow_<target>(v, file, line)；
 * 更宽的源一律以 aic_i64 承载，无符号源另走 aic_num_narrow_u_<target>）。 */
aic_i64 aic_num_narrow_i64_to_i32(aic_i64 v, const char *file, int line);
aic_i64 aic_num_narrow_i64_to_i16(aic_i64 v, const char *file, int line);
aic_i64 aic_num_narrow_i64_to_i8(aic_i64 v, const char *file, int line);
aic_i64 aic_num_narrow_i64_to_u32(aic_i64 v, const char *file, int line);
aic_i64 aic_num_narrow_i64_to_u16(aic_i64 v, const char *file, int line);
aic_i64 aic_num_narrow_i64_to_u8(aic_i64 v, const char *file, int line);
aic_u64 aic_num_narrow_u64_to_u32(aic_u64 v, const char *file, int line);
aic_u64 aic_num_narrow_u64_to_u16(aic_u64 v, const char *file, int line);
aic_u64 aic_num_narrow_u64_to_u8(aic_u64 v, const char *file, int line);
aic_u64 aic_num_narrow_u64_to_i32(aic_u64 v, const char *file, int line);
aic_u64 aic_num_narrow_u64_to_i16(aic_u64 v, const char *file, int line);
aic_u64 aic_num_narrow_u64_to_i8(aic_u64 v, const char *file, int line);

/* --- str：分配操作（字节入任务区域）---------------------------------------- *//* split 返回真实列表，可索引/遍历；空分隔符 = trap（无意义，不加糖）。 */
aic_list_str *aic_std_str_split(aic_str s, aic_str sep, const char *file, int line);
/* utf8At(i) -> (码点, 占用字节数)。 */
aic_r_u32_usize aic_std_str_utf8_at(aic_str s, aic_usize i, const char *file, int line);
/* codepoints 物化 u32[]；永不返回计数。 */
aic_list_u32 *aic_std_str_codepoints(aic_str s, const char *file, int line);
/* fromI64/fromU64/fromF64/fromBool：与插值同一套格式化（aic_fmt_*，禁第二份）。 */
aic_str aic_std_str_from_i64(aic_i64 v, aic_u32 line);
aic_str aic_std_str_from_u64(aic_u64 v, aic_u32 line);
aic_str aic_std_str_from_f64(aic_f64 v, aic_u32 line);
aic_str aic_std_str_from_bool(bool v);
/* toI64/toF64：解析失败 = (零值, Err)，非 trap。 */
aic_r_i64_err aic_std_str_to_i64(aic_str s, const char *file, int line);
aic_r_f64_err aic_std_str_to_f64(aic_str s, const char *file, int line);

/* --- os（同步最小面，D17）--------------------------------------------------- */
aic_list_str *aic_std_os_args(const char *file, int line);
aic_r_str_err aic_std_os_read_file(aic_str path, const char *file, int line);
aic_r_str_err aic_std_os_read_stdin(const char *file, int line);

/* --- math ------------------------------------------------------------------- */
static inline aic_f64 aic_math_abs_f64(aic_f64 v) { return v < 0 ? -v : v; }
static inline aic_f64 aic_math_min_f64(aic_f64 a, aic_f64 b) { return a < b ? a : b; }
static inline aic_f64 aic_math_max_f64(aic_f64 a, aic_f64 b) { return a > b ? a : b; }
static inline aic_f64 aic_math_floor_f64(aic_f64 v) { return floor(v); }
static inline aic_f64 aic_math_ceil_f64(aic_f64 v) { return ceil(v); }
static inline aic_f64 aic_math_sqrt_f64(aic_f64 v) { return sqrt(v); }
static inline aic_f64 aic_math_pow_f64(aic_f64 a, aic_f64 b) { return pow(a, b); }

/* --- testing：失败即 trap 并打印两值（§十一）-------------------------------- */
void aic_std_test_assert(bool cond, aic_str msg, const char *file, int line);
void aic_std_test_eq_i64(aic_i64 a, aic_i64 b, aic_str msg, const char *file, int line);
void aic_std_test_eq_str(aic_str a, aic_str b, aic_str msg, const char *file, int line);
/* 比较类断言：eq（==）/ gt（>）/ lt（<）；f64 与 str 各一套（浮点按 IEEE 比较）。 */
void aic_std_test_eq_f64(aic_f64 a, aic_f64 b, aic_str msg, const char *file, int line);
void aic_std_test_gt_i64(aic_i64 a, aic_i64 b, aic_str msg, const char *file, int line);
void aic_std_test_lt_i64(aic_i64 a, aic_i64 b, aic_str msg, const char *file, int line);
void aic_std_test_gt_f64(aic_f64 a, aic_f64 b, aic_str msg, const char *file, int line);
void aic_std_test_lt_f64(aic_f64 a, aic_f64 b, aic_str msg, const char *file, int line);
void aic_std_test_gt_str(aic_str a, aic_str b, aic_str msg, const char *file, int line);
void aic_std_test_lt_str(aic_str a, aic_str b, aic_str msg, const char *file, int line);

#endif /* AIC_STD_H */

/* --- time：墙钟毫秒 / 单调毫秒 / 睡眠毫秒（平台层统一口径） --------------------- */
aic_i64 aic_std_time_now(void);
aic_i64 aic_std_time_monotonic(void);
void    aic_std_time_sleep(aic_i64 ms);
