/* AIC runtime L1 —— set[T]（核心设计 §二.2）. 引用对象；插入序遍历。
 * 方法：new/add/has/remove/at/len/isEmpty；add 已存在 = 无操作保原位；
 * remove 不存在 = 无操作；at 越界 = trap。 */
#ifndef AIC_L1_SET_H
#define AIC_L1_SET_H

#include "aic_l0.h"
#include <string.h>

#define AIC_SET_OP(short, ctype, HASHF, EQF) \
    typedef struct aic_set_##short { \
        aic_hdr hdr; ctype *slots; aic_u8 *used; aic_usize *order; \
        aic_usize len; aic_usize cap; \
    } aic_set_##short; \
    static inline aic_u64 aic_set_hash_##short(ctype v) { return (aic_u64)(HASHF(v)); } \
    static inline aic_usize aic_set_slot_##short(const aic_set_##short *s, ctype v) { \
        aic_usize mask = s->cap - 1; \
        aic_usize i = (aic_usize)aic_set_hash_##short(v) & mask; \
        while (s->used[i]) { \
            if (EQF(s->slots[i], v)) return i; \
            i = (i + 1) & mask; \
        } \
        return i; \
    } \
    static inline aic_set_##short *aic_set_new_##short(aic_u32 line) { \
        aic_set_##short *s = (aic_set_##short *)aic_alloc_hdr(sizeof(aic_set_##short), line); \
        s->slots = NULL; s->used = NULL; s->order = NULL; s->len = 0; s->cap = 0; \
        return s; \
    } \
    static inline void aic_set_rehash_##short(aic_set_##short *s, aic_usize ncap, aic_u32 line) { \
        ctype *os = s->slots; aic_usize *oo = s->order; aic_usize olen = s->len; \
        s->slots = (ctype *)aic_alloc_in(s->hdr.reg, ncap * sizeof(ctype), line); \
        s->used = (aic_u8 *)aic_alloc_in(s->hdr.reg, ncap * sizeof(aic_u8), line); \
        s->order = (aic_usize *)aic_alloc_in(s->hdr.reg, ncap * sizeof(aic_usize), line); \
        memset(s->used, 0, ncap * sizeof(aic_u8)); \
        s->cap = ncap; s->len = 0; \
        aic_usize i; \
        for (i = 0; i < olen; i++) { \
            ctype e = os[oo[i]]; \
            aic_usize j = aic_set_slot_##short(s, e); \
            s->slots[j] = e; s->used[j] = 1; s->order[s->len++] = j; \
        } \
    } \
    static inline void aic_set_add_##short(aic_set_##short *s, ctype v, \
                                           const char *file, int line) { \
        AIC_LIST_WRITABLE(s, file, line); \
        if (s->cap != 0 && s->used[aic_set_slot_##short(s, v)]) return; \
        if (s->cap == 0 || (s->len + 1) * 4 >= s->cap * 3) { \
            aic_set_rehash_##short(s, s->cap ? s->cap * 2 : 8, (aic_u32)line); \
        } \
        aic_usize j = aic_set_slot_##short(s, v); \
        s->slots[j] = v; s->used[j] = 1; s->order[s->len++] = j; \
    } \
    static inline bool aic_set_has_##short(const aic_set_##short *s, ctype v) { \
        if (s == NULL || s->cap == 0) return false; \
        return s->used[aic_set_slot_##short(s, v)] != 0; \
    } \
    static inline void aic_set_remove_##short(aic_set_##short *s, ctype v, \
                                              const char *file, int line) { \
        if (s == NULL || s->cap == 0) return; \
        aic_usize hit = aic_set_slot_##short(s, v); \
        if (!s->used[hit]) return; \
        ctype *os = s->slots; aic_u8 *ou = s->used; aic_usize *oo = s->order; \
        aic_usize olen = s->len, i; \
        aic_u32 rline = (aic_u32)line; \
        s->slots = (ctype *)aic_alloc_in(s->hdr.reg, s->cap * sizeof(ctype), rline); \
        s->used = (aic_u8 *)aic_alloc_in(s->hdr.reg, s->cap * sizeof(aic_u8), rline); \
        s->order = (aic_usize *)aic_alloc_in(s->hdr.reg, s->cap * sizeof(aic_usize), rline); \
        memset(s->used, 0, s->cap * sizeof(aic_u8)); \
        s->len = 0; \
        for (i = 0; i < olen; i++) { \
            ctype e = os[oo[i]]; \
            if (EQF(e, v)) continue; \
            aic_usize j = aic_set_slot_##short(s, e); \
            s->slots[j] = e; s->used[j] = 1; s->order[s->len++] = j; \
        } \
        (void)ou; (void)file; \
    } \
    static inline aic_usize aic_set_len_##short(const aic_set_##short *s) { \
        return s ? s->len : 0; \
    } \
    static inline bool aic_set_isempty_##short(const aic_set_##short *s) { \
        return aic_set_len_##short(s) == 0; \
    } \
    /* clear：只清"已用"标记与长度，容量与缓冲留着（与 list.clear 同口径：不归还）。 */ \
    static inline void aic_set_clear_##short(aic_set_##short *s, \
                                             const char *file, int line) { \
        AIC_LIST_WRITABLE(s, file, line); \
        if (s->cap != 0 && s->used != NULL) { \
            memset(s->used, 0, s->cap * sizeof(aic_u8)); \
        } \
        s->len = 0; \
    } \
    static inline ctype aic_set_at_##short(const aic_set_##short *s, aic_usize i, \
                                           const char *file, int line) { \
        if (s == NULL || i >= s->len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); \
        return s->slots[s->order[i]]; \
    }

AIC_SET_OP(i8, aic_i8, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(i16, aic_i16, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(i32, aic_i32, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(i64, aic_i64, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(u8, aic_u8, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(u16, aic_u16, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(u32, aic_u32, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(u64, aic_u64, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(usize, aic_usize, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(bool, bool, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(str, aic_str, aic_str_hash, aic_str_keq)

#endif /* AIC_L1_SET_H */
