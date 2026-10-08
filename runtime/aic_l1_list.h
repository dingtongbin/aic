/* AIC runtime L1 —— list[T]（核心设计 §二.2）. 引用对象 + bump 区域存储。
 * 方法：new/from/len/isEmpty/get/set/append/pop/insert/remove/clear + 下标读写。 */
#ifndef AIC_L1_LIST_H
#define AIC_L1_LIST_H

#include "aic_l0.h"
#include <string.h>

#define AIC_LIST_OP(short, ctype) \
    static inline ctype *aic_list_alloc_##short(aic_list_##short *l, aic_usize cap, \
                                                aic_u32 line) { \
        return (ctype *)aic_alloc_in(l->hdr.reg, cap * sizeof(ctype), line); \
    } \
    static inline void aic_list_grow_##short(aic_list_##short *l, aic_usize need, \
                                             const char *file, int line) { \
        aic_usize cap = l->cap ? l->cap : 4; \
        while (cap < need) cap = cap + cap / 2 + 1; \
        ctype *p = aic_list_alloc_##short(l, cap, (aic_u32)line); \
        if (l->len != 0) memcpy(p, l->data, l->len * sizeof(ctype)); \
        l->data = p; \
        l->cap = cap; \
        (void)file; \
    } \
    static inline aic_list_##short *aic_list_new_##short(aic_u32 line) { \
        aic_list_##short *l = (aic_list_##short *)aic_alloc_hdr(sizeof(aic_list_##short), line); \
        l->data = NULL; l->len = 0; l->cap = 0; \
        return l; \
    } \
    static inline aic_list_##short *aic_list_from_##short(const ctype *v, aic_usize n, aic_u32 line) { \
        aic_list_##short *l = aic_list_new_##short(line); \
        if (n != 0) { \
            aic_list_grow_##short(l, n, "<literal>", (int)line); \
            memcpy(l->data, v, n * sizeof(ctype)); \
            l->len = n; \
        } \
        return l; \
    } \
    static inline aic_usize aic_list_len_##short(const aic_list_##short *l) { \
        return l ? l->len : 0; \
    } \
    static inline bool aic_list_isempty_##short(const aic_list_##short *l) { \
        return aic_list_len_##short(l) == 0; \
    } \
    static inline ctype aic_list_get_##short(const aic_list_##short *l, aic_usize i, \
                                             const char *file, int line) { \
        /* 热路径：一次比较 + 直接读（§10.4 第 1 条）；越界/空句柄走 cold 慢路径。 */ \
        if (AIC_LIKELY(l != NULL && i < l->len)) return l->data[i]; \
        aic_cold_list_oob_##short(file, line); \
    } \
    static inline ctype *aic_list_ref_##short(const aic_list_##short *l, aic_usize i, \
                                              const char *file, int line) { \
        if (AIC_LIKELY(l != NULL && i < l->len)) return &l->data[i]; \
        aic_cold_list_oob_##short(file, line); \
    } \
    static inline void aic_list_set_##short(aic_list_##short *l, aic_usize i, ctype v, \
                                            const char *file, int line) { \
        /* 热路径：写入既有元素 = 一次比较 + 直接写（§10.4 第 1 条）。 */ \
        if (AIC_LIKELY(l != NULL && i < l->len)) { l->data[i] = v; return; } \
        aic_cold_list_set_slow_##short(l, i, v, file, line); \
    } \
    static inline void aic_list_append_##short(aic_list_##short *l, ctype v, \
                                               const char *file, int line) { \
        AIC_LIST_WRITABLE(l, file, line); \
        if (l->len + 1 > l->cap) aic_list_grow_##short(l, l->len + 1, file, line); \
        l->data[l->len] = v; \
        l->len++; \
    } \
    static inline ctype aic_list_pop_##short(aic_list_##short *l, bool *ok) { \
        ctype zero; \
        memset(&zero, 0, sizeof(zero)); \
        if (l == NULL || l->len == 0) { *ok = false; return zero; } \
        *ok = true; \
        l->len--; \
        return l->data[l->len]; \
    } \
    static inline void aic_list_insert_##short(aic_list_##short *l, aic_usize i, ctype v, \
                                               const char *file, int line) { \
        AIC_LIST_WRITABLE(l, file, line); \
        if (i > l->len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); \
        if (l->len + 1 > l->cap) aic_list_grow_##short(l, l->len + 1, file, line); \
        if (i < l->len) memmove(&l->data[i + 1], &l->data[i], (l->len - i) * sizeof(ctype)); \
        l->data[i] = v; \
        l->len++; \
    } \
    static inline void aic_list_remove_##short(aic_list_##short *l, aic_usize i, \
                                               const char *file, int line) { \
        AIC_LIST_WRITABLE(l, file, line); \
        if (i >= l->len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); \
        if (i + 1 < l->len) memmove(&l->data[i], &l->data[i + 1], (l->len - i - 1) * sizeof(ctype)); \
        l->len--; \
    } \
    static inline void aic_list_clear_##short(aic_list_##short *l, const char *file, int line) { \
        AIC_LIST_WRITABLE(l, file, line); \
        l->len = 0; \
        (void)file; \
    }


/* 容器访问的缓存形态（§10.4 第 2 条）：循环前取一次 len/data，热路径只比较 + 读写；
 * 慢路径（越界/增长）走原访问器并**刷新缓存** —— 缓存永不过期，故无需任何证明。
 * 声明由发射器直接发（形如 `aic_list_i32 *aic_cc_1_h = xs; aic_usize aic_cc_1_n = …;`）。 */
#define AIC_LIST_GET_CACHED(short, cache, i, file, line) \
    (AIC_LIKELY((i) < cache##_n) \
         ? cache##_p[(i)] \
         : (cache##_n = cache##_h->len, cache##_p = cache##_h->data, \
            aic_list_get_##short(cache##_h, (i), (file), (line))))
#define AIC_LIST_SET_CACHED(short, cache, i, v, file, line) \
    do { \
        if (AIC_LIKELY((i) < cache##_n)) { \
            cache##_p[(i)] = (v); \
        } else { \
            aic_list_set_##short(cache##_h, (i), (v), (file), (line)); \
            cache##_n = cache##_h->len; \
            cache##_p = cache##_h->data; \
        } \
    } while (0)

/* 无检查访问器（§10.4 第 12 条②）：**仅当发射侧已证明在界内**时使用。
 * 语义：直接读写，不做 NULL/越界检查；证明义务在编译器（不是运行时的责任）。 */
#define AIC_LIST_UNCHECKED(short, ctype) \
    static inline ctype aic_list_get_unchecked_##short(const aic_list_##short *l, \
                                                       aic_usize i) { \
        return l->data[i]; \
    } \
    static inline void aic_list_set_unchecked_##short(aic_list_##short *l, aic_usize i, ctype v) { \
        l->data[i] = v; \
    }

/* 慢路径（§10.4 第 1/4 条）：cold + noinline —— 热路径只留一次比较。
 * 语义与旧实现完全一致：空句柄/越界读 = trap；越界写 = 增长 + 补零值（语料 695 冻结）。 */



#define AIC_LIST_SLOW(short, ctype) \
    static AIC_COLD_NORETURN void aic_cold_list_oob_##short(const char *file, int line) { \
        aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); \
    } \
    static AIC_COLD void aic_cold_list_set_slow_##short(aic_list_##short *l, aic_usize i, \
                                                        ctype v, const char *file, int line) { \
        AIC_LIST_WRITABLE(l, file, line); \
        if (i >= l->cap) aic_list_grow_##short(l, i + 1, file, line); \
        if (i >= l->len) { \
            memset(&l->data[l->len], 0, (i + 1 - l->len) * sizeof(ctype)); \
            l->len = i + 1; \
        } \
        l->data[i] = v; \
    }

/* 慢路径前向声明（热路径 inline 函数要调用它们，定义在本文件末尾）。 */
#define AIC_LIST_SLOW_DECL(short, ctype) \
    static AIC_COLD_NORETURN void aic_cold_list_oob_##short(const char *file, int line); \
    static AIC_COLD void aic_cold_list_set_slow_##short(aic_list_##short *l, aic_usize i, \
                                                        ctype v, const char *file, int line);

AIC_LIST_SLOW_DECL(i8, aic_i8)
AIC_LIST_SLOW_DECL(i16, aic_i16)
AIC_LIST_SLOW_DECL(i32, aic_i32)
AIC_LIST_SLOW_DECL(i64, aic_i64)
AIC_LIST_SLOW_DECL(u8, aic_u8)
AIC_LIST_SLOW_DECL(u16, aic_u16)
AIC_LIST_SLOW_DECL(u32, aic_u32)
AIC_LIST_SLOW_DECL(u64, aic_u64)
AIC_LIST_SLOW_DECL(usize, aic_usize)
AIC_LIST_SLOW_DECL(f32, aic_f32)
AIC_LIST_SLOW_DECL(f64, aic_f64)
AIC_LIST_SLOW_DECL(bool, bool)
AIC_LIST_SLOW_DECL(str, aic_str)
AIC_LIST_SLOW_DECL(Box, void *)
AIC_LIST_SLOW_DECL(iface, aic_iface)

AIC_LIST_OP(i8, aic_i8)
AIC_LIST_OP(i16, aic_i16)
AIC_LIST_OP(i32, aic_i32)
AIC_LIST_OP(i64, aic_i64)
AIC_LIST_OP(u8, aic_u8)
AIC_LIST_OP(u16, aic_u16)
AIC_LIST_OP(u32, aic_u32)
AIC_LIST_OP(u64, aic_u64)
AIC_LIST_OP(usize, aic_usize)
AIC_LIST_OP(f32, aic_f32)
AIC_LIST_OP(f64, aic_f64)
AIC_LIST_OP(bool, bool)
AIC_LIST_OP(str, aic_str)
AIC_LIST_OP(Box, void *)
/* 接口元素：aic_iface 是两机器字结构，专用实例（§四：接口值 = {data, vt}）。 */
AIC_LIST_OP(iface, aic_iface)



/* 慢路径实例化（在 OP 之后：引用 aic_list_grow_<suf>）。 */
AIC_LIST_SLOW(i8, aic_i8)
AIC_LIST_SLOW(i16, aic_i16)
AIC_LIST_SLOW(i32, aic_i32)
AIC_LIST_SLOW(i64, aic_i64)
AIC_LIST_SLOW(u8, aic_u8)
AIC_LIST_SLOW(u16, aic_u16)
AIC_LIST_SLOW(u32, aic_u32)
AIC_LIST_SLOW(u64, aic_u64)
AIC_LIST_SLOW(usize, aic_usize)
AIC_LIST_SLOW(f32, aic_f32)
AIC_LIST_SLOW(f64, aic_f64)
AIC_LIST_SLOW(bool, bool)
AIC_LIST_SLOW(str, aic_str)
AIC_LIST_SLOW(Box, void *)
AIC_LIST_SLOW(iface, aic_iface)

/* 无检查形态实例化（与带检查形态并列）。 */
AIC_LIST_UNCHECKED(i8, aic_i8)
AIC_LIST_UNCHECKED(i16, aic_i16)
AIC_LIST_UNCHECKED(i32, aic_i32)
AIC_LIST_UNCHECKED(i64, aic_i64)
AIC_LIST_UNCHECKED(u8, aic_u8)
AIC_LIST_UNCHECKED(u16, aic_u16)
AIC_LIST_UNCHECKED(u32, aic_u32)
AIC_LIST_UNCHECKED(u64, aic_u64)
AIC_LIST_UNCHECKED(usize, aic_usize)
AIC_LIST_UNCHECKED(f32, aic_f32)
AIC_LIST_UNCHECKED(f64, aic_f64)
AIC_LIST_UNCHECKED(bool, bool)
AIC_LIST_UNCHECKED(str, aic_str)
AIC_LIST_UNCHECKED(Box, void *)
AIC_LIST_UNCHECKED(iface, aic_iface)

/* ============================================================================
 * N7 `bytes`：区域托管、可增长的字节缓冲
 *
 * **复用 aic_list_u8 实例**（禁第二份缓冲实现）：bytes 是它的独立类型身份 +
 * 独立方法集。越界读 = trap、越界写 = 增长补零（与 list 同源）。
 * ==========================================================================*/
static inline aic_list_u8 *aic_bytes_new(aic_u32 line) { return aic_list_new_u8(line); }

/* withCap：预留容量（不改变长度）。cap 是"已分配容量"的下界。 */
static inline aic_list_u8 *aic_bytes_with_cap(aic_usize cap, aic_u32 line) {
    aic_list_u8 *b = aic_list_new_u8(line);
    if (cap != 0) {
        b->data = (aic_u8 *)aic_alloc_in(b->hdr.reg, cap, line);
        b->cap = cap;
    }
    return b;
}

static inline aic_usize aic_bytes_cap(const aic_list_u8 *b) { return b ? b->cap : 0; }

/* slice(a, b)：半开区间 [a, b) 的**拷贝**（新缓冲，与 str 视图不同 —— bytes 可写）。 */
static inline aic_list_u8 *aic_bytes_slice(const aic_list_u8 *b, aic_usize lo, aic_usize hi,
                                           const char *file, int line) {
    if (b == NULL) {
        aic_trap(AIC_TRAP_ZERO_CONTAINER_WRITE, file, line);
    }
    if (lo > hi || hi > b->len) {
        aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line);
    }
    aic_list_u8 *out = aic_list_new_u8((aic_u32)line);
    for (aic_usize i = lo; i < hi; i++) {
        aic_list_append_u8(out, b->data[i], file, line);
    }
    return out;
}

static inline void aic_bytes_append_bytes(aic_list_u8 *b, const aic_list_u8 *src,
                                          const char *file, int line) {
    if (b == NULL) {
        aic_trap(AIC_TRAP_ZERO_CONTAINER_WRITE, file, line);
    }
    if (src == NULL) {
        return;
    }
    /* 长度必须**先快照**：b == src（自追加）时 `src->len` 每轮都在涨 ⇒ 死循环 +
     * 无限增长（实测 OOM trap）。快照后语义 = "把追加前的这份字节再追加一遍"。 */
    aic_usize n = src->len;
    for (aic_usize i = 0; i < n; i++) {
        aic_list_append_u8(b, src->data[i], file, line);
    }
}

/* toStr：拷贝成 str（字节入任务区域，与 aic_str_alloc 同源）。 */
static inline aic_str aic_bytes_to_str(const aic_list_u8 *b, aic_u32 line) {
    if (b == NULL || b->len == 0) {
        return (aic_str){ 0, 0 };
    }
    aic_str s = aic_str_alloc(b->len, line);
    memcpy((void *)s.p, b->data, b->len);
    return (aic_str){ s.p, b->len };
}

/* fromStr：拷贝成 bytes（显式转换，无隐式）。 */
static inline aic_list_u8 *aic_bytes_from_str(aic_str s, aic_u32 line) {
    aic_list_u8 *b = aic_list_new_u8(line);
    for (aic_usize i = 0; i < s.len; i++) {
        aic_list_append_u8(b, (aic_u8)s.p[i], "<bytes>", (int)line);
    }
    return b;
}

#endif /* AIC_L1_LIST_H */
