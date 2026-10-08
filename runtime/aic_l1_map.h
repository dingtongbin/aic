/* AIC runtime L1 —— map[K]V（核心设计 §二.2）. 引用对象；插入序遍历（H3 确定）。
 * 方法：new/put/get/has/remove/len/isEmpty/keyAt/valAt。
 * put 已存在键 = 覆盖值保原位；get 未命中 = 零值；remove 不存在 = 无操作；
 * keyAt/valAt 越界 = trap。
 * 符号名编码 (K,V) 两者：头未实例化的 (K,V) 无符号可调 = 编译错（§三）。 */
#ifndef AIC_L1_MAP_H
#define AIC_L1_MAP_H

#include "aic_l0.h"
#include <string.h>

#define AIC_MAP_OP(KS, KT, VS, VT, HASHF, KEQF) \
    typedef struct aic_map_##KS##_##VS { \
        aic_hdr hdr; KT *keys; VT *vals; aic_usize *order; aic_u8 *used; \
        aic_usize len; aic_usize cap; \
    } aic_map_##KS##_##VS; \
    static inline aic_usize aic_map_slot_##KS##_##VS(const aic_map_##KS##_##VS *m, KT k) { \
        aic_usize mask = m->cap - 1; \
        aic_usize i = (aic_usize)(HASHF(k)) & mask; \
        while (m->used[i]) { \
            if (KEQF(m->keys[i], k)) return i; \
            i = (i + 1) & mask; \
        } \
        return i; \
    } \
    static inline aic_usize aic_map_find_##KS##_##VS(const aic_map_##KS##_##VS *m, KT k) { \
        if (m->cap == 0) return (aic_usize)-1; \
        aic_usize mask = m->cap - 1; \
        aic_usize i = (aic_usize)(HASHF(k)) & mask; \
        while (m->used[i]) { \
            if (KEQF(m->keys[i], k)) return i; \
            i = (i + 1) & mask; \
        } \
        return (aic_usize)-1; \
    } \
    static inline aic_map_##KS##_##VS *aic_map_new_##KS##_##VS(aic_u32 line) { \
        aic_map_##KS##_##VS *m = (aic_map_##KS##_##VS *)aic_alloc_hdr(sizeof(aic_map_##KS##_##VS), line); \
        m->keys = NULL; m->vals = NULL; m->order = NULL; m->used = NULL; m->len = 0; m->cap = 0; \
        return m; \
    } \
    static inline void aic_map_rehash_##KS##_##VS(aic_map_##KS##_##VS *m, aic_usize ncap, aic_u32 line) { \
        KT *ok = m->keys; VT *ov = m->vals; aic_usize *oo = m->order; aic_usize olen = m->len; \
        m->keys = (KT *)aic_alloc_in(m->hdr.reg, ncap * sizeof(KT), line); \
        m->vals = (VT *)aic_alloc_in(m->hdr.reg, ncap * sizeof(VT), line); \
        m->order = (aic_usize *)aic_alloc_in(m->hdr.reg, ncap * sizeof(aic_usize), line); \
        m->used = (aic_u8 *)aic_alloc_in(m->hdr.reg, ncap * sizeof(aic_u8), line); \
        memset(m->used, 0, ncap * sizeof(aic_u8)); \
        m->cap = ncap; m->len = 0; \
        aic_usize i; \
        for (i = 0; i < olen; i++) { \
            aic_usize oi = oo[i]; \
            aic_usize j = aic_map_slot_##KS##_##VS(m, ok[oi]); \
            m->keys[j] = ok[oi]; m->vals[j] = ov[oi]; m->used[j] = 1; m->order[m->len++] = j; \
        } \
    } \
    static inline void aic_map_put_##KS##_##VS(aic_map_##KS##_##VS *m, KT k, VT v, \
                                               const char *file, int line) { \
        AIC_LIST_WRITABLE(m, file, line); \
        aic_usize hit = aic_map_find_##KS##_##VS(m, k); \
        if (hit != (aic_usize)-1) { m->vals[hit] = v; return; } \
        if (m->cap == 0 || (m->len + 1) * 4 >= m->cap * 3) { \
            aic_map_rehash_##KS##_##VS(m, m->cap ? m->cap * 2 : 8, (aic_u32)line); \
        } \
        aic_usize i = aic_map_slot_##KS##_##VS(m, k); \
        m->keys[i] = k; m->vals[i] = v; m->used[i] = 1; m->order[m->len++] = i; \
    } \
    static inline bool aic_map_has_##KS##_##VS(const aic_map_##KS##_##VS *m, KT k) { \
        return m != NULL && aic_map_find_##KS##_##VS(m, k) != (aic_usize)-1; \
    } \
    static inline VT aic_map_get_##KS##_##VS(const aic_map_##KS##_##VS *m, KT k) { \
        VT zero; \
        memset(&zero, 0, sizeof(zero)); \
        if (m == NULL) return zero; \
        aic_usize i = aic_map_find_##KS##_##VS(m, k); \
        return i == (aic_usize)-1 ? zero : m->vals[i]; \
    } \
    static inline void aic_map_remove_##KS##_##VS(aic_map_##KS##_##VS *m, KT k, \
                                                  const char *file, int line) { \
        if (m == NULL || m->cap == 0) return; \
        aic_usize hit = aic_map_find_##KS##_##VS(m, k); \
        if (hit == (aic_usize)-1) return; \
        KT *ok = m->keys; VT *ov = m->vals; aic_usize *oo = m->order; aic_usize olen = m->len, i; \
        aic_u32 rline = (aic_u32)line; \
        m->keys = (KT *)aic_alloc_in(m->hdr.reg, m->cap * sizeof(KT), rline); \
        m->vals = (VT *)aic_alloc_in(m->hdr.reg, m->cap * sizeof(VT), rline); \
        m->order = (aic_usize *)aic_alloc_in(m->hdr.reg, m->cap * sizeof(aic_usize), rline); \
        m->used = (aic_u8 *)aic_alloc_in(m->hdr.reg, m->cap * sizeof(aic_u8), rline); \
        memset(m->used, 0, m->cap * sizeof(aic_u8)); \
        m->len = 0; \
        for (i = 0; i < olen; i++) { \
            aic_usize oi = oo[i]; \
            if (KEQF(ok[oi], k)) continue; \
            aic_usize j = aic_map_slot_##KS##_##VS(m, ok[oi]); \
            m->keys[j] = ok[oi]; m->vals[j] = ov[oi]; m->used[j] = 1; m->order[m->len++] = j; \
        } \
        (void)file; \
    } \
    static inline aic_usize aic_map_len_##KS##_##VS(const aic_map_##KS##_##VS *m) { \
        return m ? m->len : 0; \
    } \
    static inline bool aic_map_isempty_##KS##_##VS(const aic_map_##KS##_##VS *m) { \
        return aic_map_len_##KS##_##VS(m) == 0; \
    } \
    /* clear：只清"已用"标记与长度，容量与缓冲留着（与 list.clear 同口径：不归还）。 */ \
    static inline void aic_map_clear_##KS##_##VS(aic_map_##KS##_##VS *m, \
                                                const char *file, int line) { \
        AIC_LIST_WRITABLE(m, file, line); \
        if (m->cap != 0 && m->used != NULL) { \
            memset(m->used, 0, m->cap * sizeof(aic_u8)); \
        } \
        m->len = 0; \
    } \
    static inline KT aic_map_key_at_##KS##_##VS(const aic_map_##KS##_##VS *m, aic_usize i, \
                                                const char *file, int line) { \
        if (m == NULL || i >= m->len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); \
        return m->keys[m->order[i]]; \
    } \
    static inline VT aic_map_val_at_##KS##_##VS(const aic_map_##KS##_##VS *m, aic_usize i, \
                                                const char *file, int line) { \
        if (m == NULL || i >= m->len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); \
        return m->vals[m->order[i]]; \
    }

/* POD 键（整数/bool）：按字节哈希。 */
#define AIC_MAP_POD(KS, KT, VS, VT) AIC_MAP_OP(KS, KT, VS, VT, aic_pod_hash, aic_pod_eq)

AIC_MAP_POD(i32, aic_i32, i32, aic_i32)
AIC_MAP_POD(i32, aic_i32, i64, aic_i64)
AIC_MAP_POD(i32, aic_i32, f64, aic_f64)
AIC_MAP_POD(i32, aic_i32, str, aic_str)
AIC_MAP_POD(i32, aic_i32, Box, void *)
AIC_MAP_POD(i64, aic_i64, i32, aic_i32)
AIC_MAP_POD(i64, aic_i64, i64, aic_i64)
AIC_MAP_POD(i64, aic_i64, f64, aic_f64)
AIC_MAP_POD(i64, aic_i64, str, aic_str)
AIC_MAP_POD(i64, aic_i64, Box, void *)
AIC_MAP_POD(u32, aic_u32, i32, aic_i32)
AIC_MAP_POD(u32, aic_u32, str, aic_str)
AIC_MAP_POD(u32, aic_u32, Box, void *)
AIC_MAP_POD(u64, aic_u64, i32, aic_i32)
AIC_MAP_POD(u64, aic_u64, str, aic_str)
AIC_MAP_POD(u64, aic_u64, Box, void *)
AIC_MAP_POD(usize, aic_usize, i32, aic_i32)
AIC_MAP_POD(usize, aic_usize, f64, aic_f64)
AIC_MAP_POD(usize, aic_usize, str, aic_str)
AIC_MAP_POD(usize, aic_usize, Box, void *)
/* bool 键的实例化必须**直接调 AIC_MAP_OP**，不能走 AIC_MAP_POD：
 * AIC_MAP_POD 把 KS 当普通实参传给下一层宏，实参预扫描会把 bool 展开成 _Bool，
 * 于是类型名成了 aic_map__Bool_i32（emit 侧按 aic_map_bool_i32 生成 = 链接期
 * 找不到符号）。直接调 AIC_MAP_OP 时 KS 紧邻 ##，按标准不做展开 → aic_map_bool_i32。 */
AIC_MAP_OP(bool, bool, i32, aic_i32, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(bool, bool, str, aic_str, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(bool, bool, Box, void *, aic_pod_hash, aic_pod_eq)

/* str 键：按内容哈希/比较。 */
AIC_MAP_OP(str, aic_str, i32, aic_i32, aic_str_hash, aic_str_keq)
AIC_MAP_OP(str, aic_str, i64, aic_i64, aic_str_hash, aic_str_keq)
AIC_MAP_OP(str, aic_str, str, aic_str, aic_str_hash, aic_str_keq)
AIC_MAP_OP(str, aic_str, Box, void *, aic_str_hash, aic_str_keq)

/* 接口值槽（§四：接口值 = {data, vt} 两机器字，不能塞 void* 槽）。
 * 与 AIC_LIST_OP(iface, aic_iface) 同款：`map[K]接口` 必须有自己的实例。 */
AIC_MAP_OP(str, aic_str, iface, aic_iface, aic_str_hash, aic_str_keq)
AIC_MAP_OP(i32, aic_i32, iface, aic_iface, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(i64, aic_i64, iface, aic_iface, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(u32, aic_u32, iface, aic_iface, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(u64, aic_u64, iface, aic_iface, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(usize, aic_usize, iface, aic_iface, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(bool, bool, iface, aic_iface, aic_pod_hash, aic_pod_eq)

#endif /* AIC_L1_MAP_H */
