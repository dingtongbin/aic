/* AIC runtime L1 —— 打印原语表（核心设计 §十四：println(v) 接受任意单值）.
 *
 * 形态（与 emit 侧的定型分派同源，禁第二份格式）：
 *   标量   数值十进制 / bool true|false / str 原样（不加引号）/ Err：code 0 = nil，否则 msg
 *   list   [a, b, c]      set  {a, b}      map  {k: v}      数组 [a, b, c]
 *   nil    nil（零值容器/引用）
 *   嵌套   深度上限 4，超出打 …（U+2026）——环安全，且永不打印地址（H3 确定性）
 *
 * 容器元素按各自的单值形态打印（str 元素原样，不加引号）。
 * 表与 aic_l1_list/set/map 的实例化表一一对应：头未实例化的形状无符号可调 = 编译错。
 * enum / class(@derive(ToString)) / Box 元素的打印由 emit 侧按需合成（名字不可预知）。
 */
#ifndef AIC_L1_PRINT_H
#define AIC_L1_PRINT_H

#include "aic_l1.h"

/* 嵌套深度上限（§十四）：超过即打省略号，保证双向引用不死循环。 */
#define AIC_PRINT_DEPTH_MAX 4

static inline void aic_print_nl(void) { aic_print_str((aic_str){"\n", 1}); }
static inline void aic_pr_ellipsis(void) { aic_print_str((aic_str){"\xe2\x80\xa6", 3}); }
static inline void aic_pr_nil(void) { aic_print_str((aic_str){"nil", 3}); }
static inline void aic_pr_sep(void) { aic_print_str((aic_str){", ", 2}); }
static inline void aic_pr_lbracket(void) { aic_print_str((aic_str){"[", 1}); }
static inline void aic_pr_rbracket(void) { aic_print_str((aic_str){"]", 1}); }
static inline void aic_pr_lbrace(void) { aic_print_str((aic_str){"{", 1}); }
static inline void aic_pr_rbrace(void) { aic_print_str((aic_str){"}", 1}); }
static inline void aic_pr_colon(void) { aic_print_str((aic_str){": ", 2}); }
static inline void aic_pr_lparen(void) { aic_print_str((aic_str){"(", 1}); }
static inline void aic_pr_rparen(void) { aic_print_str((aic_str){")", 1}); }

/* Err：code 0 = 无错（打 nil），否则打 msg（§六 code 0 保留为无错）。 */
static inline void aic_pr_err(aic_Err v, int depth) {
    (void)depth;
    if (v.code == 0) aic_pr_nil();
    else aic_print_str(v.msg);
}

/* 标量叶子：统一签名 (T, int depth)，depth 对叶子无意义（保持调用点同形）。 */
#define AIC_PRINT_SCALAR(suf, T, EXPR) \
    static inline void aic_pr_##suf(T v, int depth) { (void)depth; EXPR; }

AIC_PRINT_SCALAR(i8, aic_i8, aic_print_i32((aic_i32)v))
AIC_PRINT_SCALAR(i16, aic_i16, aic_print_i32((aic_i32)v))
AIC_PRINT_SCALAR(i32, aic_i32, aic_print_i32(v))
AIC_PRINT_SCALAR(i64, aic_i64, aic_print_i64(v))
AIC_PRINT_SCALAR(u8, aic_u8, aic_print_u32((aic_u32)v))
AIC_PRINT_SCALAR(u16, aic_u16, aic_print_u32((aic_u32)v))
AIC_PRINT_SCALAR(u32, aic_u32, aic_print_u32(v))
AIC_PRINT_SCALAR(u64, aic_u64, aic_print_usize((aic_usize)v))
AIC_PRINT_SCALAR(usize, aic_usize, aic_print_usize(v))
AIC_PRINT_SCALAR(f32, aic_f32, aic_print_f64((aic_f64)v))
AIC_PRINT_SCALAR(f64, aic_f64, aic_print_f64(v))
AIC_PRINT_SCALAR(bool, bool, aic_print_bool(v))
AIC_PRINT_SCALAR(str, aic_str, aic_print_str(v))

/* list[T]：插入序 [a, b, c]；nil 句柄打 nil。 */
#define AIC_LIST_PRINT(suf) \
    static inline void aic_pr_list_##suf(aic_list_##suf *xs, int depth) { \
        aic_usize i; \
        if (xs == NULL) { aic_pr_nil(); return; } \
        if (depth >= AIC_PRINT_DEPTH_MAX) { aic_pr_ellipsis(); return; } \
        aic_pr_lbracket(); \
        for (i = 0; i < xs->len; i++) { \
            if (i != 0) aic_pr_sep(); \
            aic_pr_##suf(xs->data[i], depth + 1); \
        } \
        aic_pr_rbracket(); \
    }

AIC_LIST_PRINT(i8)
AIC_LIST_PRINT(i16)
AIC_LIST_PRINT(i32)
AIC_LIST_PRINT(i64)
AIC_LIST_PRINT(u8)
AIC_LIST_PRINT(u16)
AIC_LIST_PRINT(u32)
AIC_LIST_PRINT(u64)
AIC_LIST_PRINT(usize)
AIC_LIST_PRINT(f32)
AIC_LIST_PRINT(f64)
AIC_LIST_PRINT(bool)
AIC_LIST_PRINT(str)

/* set[T]：插入序 {a, b}（与 list 的 [ ] 区分，便于人读）。 */
#define AIC_SET_PRINT(suf) \
    static inline void aic_pr_set_##suf(aic_set_##suf *s, int depth) { \
        aic_usize i; \
        if (s == NULL) { aic_pr_nil(); return; } \
        if (depth >= AIC_PRINT_DEPTH_MAX) { aic_pr_ellipsis(); return; } \
        aic_pr_lbrace(); \
        for (i = 0; i < s->len; i++) { \
            if (i != 0) aic_pr_sep(); \
            aic_pr_##suf(s->slots[s->order[i]], depth + 1); \
        } \
        aic_pr_rbrace(); \
    }

AIC_SET_PRINT(i8)
AIC_SET_PRINT(i16)
AIC_SET_PRINT(i32)
AIC_SET_PRINT(i64)
AIC_SET_PRINT(u8)
AIC_SET_PRINT(u16)
AIC_SET_PRINT(u32)
AIC_SET_PRINT(u64)
AIC_SET_PRINT(usize)
AIC_SET_PRINT(bool)
AIC_SET_PRINT(str)

/* map[K]V：插入序 {k: v}。 */
#define AIC_MAP_PRINT(KS, VS) \
    static inline void aic_pr_map_##KS##_##VS(aic_map_##KS##_##VS *m, int depth) { \
        aic_usize i; \
        if (m == NULL) { aic_pr_nil(); return; } \
        if (depth >= AIC_PRINT_DEPTH_MAX) { aic_pr_ellipsis(); return; } \
        aic_pr_lbrace(); \
        for (i = 0; i < m->len; i++) { \
            if (i != 0) aic_pr_sep(); \
            aic_pr_##KS(AIC_MAP_SLOT_KEY(m, m->order[i]), depth + 1); \
            aic_pr_colon(); \
            aic_pr_##VS(AIC_MAP_SLOT_VAL(m, m->order[i]), depth + 1); \
        } \
        aic_pr_rbrace(); \
    }

AIC_MAP_PRINT(i32, i32)
AIC_MAP_PRINT(i32, i64)
AIC_MAP_PRINT(i32, f64)
AIC_MAP_PRINT(i32, str)
AIC_MAP_PRINT(i64, i32)
AIC_MAP_PRINT(i64, i64)
AIC_MAP_PRINT(i64, f64)
AIC_MAP_PRINT(i64, str)
AIC_MAP_PRINT(u32, i32)
AIC_MAP_PRINT(u32, str)
AIC_MAP_PRINT(u64, i32)
AIC_MAP_PRINT(u64, str)
AIC_MAP_PRINT(usize, i32)
AIC_MAP_PRINT(usize, f64)
AIC_MAP_PRINT(usize, str)
AIC_MAP_PRINT(bool, i32)
AIC_MAP_PRINT(bool, str)
AIC_MAP_PRINT(str, i32)
AIC_MAP_PRINT(str, i64)
AIC_MAP_PRINT(str, str)

#endif /* AIC_L1_PRINT_H */
