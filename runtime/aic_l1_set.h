/* AIC runtime L1 —— set[T]（核心设计 §二.2）. 引用对象；插入序遍历。
 * 方法：new/add/has/remove/at/len/isEmpty；add 已存在 = 无操作保原位；
 * remove 不存在 = 无操作；at 越界 = trap。 */
#ifndef AIC_L1_SET_H
#define AIC_L1_SET_H

#include "aic_l0.h"
#include <string.h>

/* set 槽字段名的唯一定义处（布局中立访问器，emit 侧 printgen 用它）——
 * 与 aic_l1_map.h 的 AIC_MAP_SLOT_* 同一约定。 */
#define AIC_SET_SLOT(s, i) ((s)->slots[i])

#define AIC_SET_OP(short, ctype, HASHF, EQF) \
    typedef struct aic_set_##short { \
        aic_hdr hdr; ctype *slots; aic_u8 *used; aic_usize *order; \
        aic_usize *ordpos; \
        aic_usize len; aic_usize cap; aic_usize holes; \
    } aic_set_##short; \
    /* used 的三态：0 = 空、1 = 存活、2 = 墓碑（已删除）。 */ \
    static inline aic_u64 aic_set_hash_##short(ctype v) { return (aic_u64)(HASHF(v)); } \
    static inline aic_usize aic_set_find_##short(const aic_set_##short *s, ctype v) { \
        if (s->cap == 0) return (aic_usize)-1; \
        aic_usize mask = s->cap - 1; \
        aic_usize i = (aic_usize)aic_set_hash_##short(v) & mask; \
        while (s->used[i]) { \
            if (s->used[i] == 1 && EQF(s->slots[i], v)) return i; \
            i = (i + 1) & mask; \
        } \
        return (aic_usize)-1; \
    } \
    static inline aic_usize aic_set_slot_##short(const aic_set_##short *s, ctype v) { \
        aic_usize mask = s->cap - 1; \
        aic_usize i = (aic_usize)aic_set_hash_##short(v) & mask; \
        aic_usize first_hole = (aic_usize)-1; \
        for (;;) { \
            aic_u8 u = s->used[i]; \
            if (u == 0) return first_hole != (aic_usize)-1 ? first_hole : i; \
            if (u == 2) { \
                if (first_hole == (aic_usize)-1) first_hole = i; \
            } else if (EQF(s->slots[i], v)) { \
                return i; \
            } \
            i = (i + 1) & mask; \
        } \
    } \
    static inline aic_set_##short *aic_set_new_##short(aic_u32 line) { \
        aic_set_##short *s = (aic_set_##short *)aic_alloc_hdr(sizeof(aic_set_##short), line); \
        s->slots = NULL; s->used = NULL; s->order = NULL; s->ordpos = NULL; s->len = 0; s->cap = 0; s->holes = 0; \
        return s; \
    } \
    static inline void aic_set_rehash_##short(aic_set_##short *s, aic_usize ncap, aic_u32 line) { \
        ctype *os = s->slots; aic_usize *oo = s->order; aic_usize olen = s->len; \
        s->slots = (ctype *)aic_alloc_in(s->hdr.reg, ncap * sizeof(ctype), line); \
        s->used = (aic_u8 *)aic_alloc_in(s->hdr.reg, ncap * sizeof(aic_u8), line); \
        s->order = (aic_usize *)aic_alloc_in(s->hdr.reg, ncap * sizeof(aic_usize), line); \
        s->ordpos = (aic_usize *)aic_alloc_in(s->hdr.reg, ncap * sizeof(aic_usize), line); \
        memset(s->used, 0, ncap * sizeof(aic_u8)); \
        s->cap = ncap; s->len = 0; s->holes = 0; \
        aic_usize i; \
        for (i = 0; i < olen; i++) { \
            ctype e = os[oo[i]]; \
            aic_usize j = aic_set_slot_##short(s, e); \
            s->slots[j] = e; s->used[j] = 1; s->order[s->len] = j; s->ordpos[j] = s->len; s->len++; \
        } \
    } \
    static inline void aic_set_add_##short(aic_set_##short *s, ctype v, \
                                           const char *file, int line) { \
        AIC_LIST_WRITABLE(s, file, line); \
        if (s->cap != 0 && aic_set_find_##short(s, v) != (aic_usize)-1) return; \
        if (s->cap == 0 || (s->len + s->holes + 1) * 4 >= s->cap * 3) { \
            aic_set_rehash_##short(s, s->cap ? s->cap * 2 : 8, (aic_u32)line); \
        } \
        aic_usize j = aic_set_slot_##short(s, v); \
        if (s->used[j] == 2) s->holes--; \
        s->slots[j] = v; s->used[j] = 1; s->order[s->len] = j; s->ordpos[j] = s->len; s->len++; \
    } \
    static inline bool aic_set_has_##short(const aic_set_##short *s, ctype v) { \
        return aic_set_find_##short(s, v) != (aic_usize)-1; \
    } \
    static inline void aic_set_remove_##short(aic_set_##short *s, ctype v, \
                                              const char *file, int line) { \
        if (s == NULL || s->cap == 0) return; \
        aic_usize hit = aic_set_find_##short(s, v); \
        if (hit == (aic_usize)-1) return; \
        /* 墓碑 + order 末位交换（零分配）。曾经 remove 每次分配 3 个满容量新数组， \
         * 区域分配器不归还旧的 ⇒ 大规模删除内存失控（与 map.remove 同病）。 \
         * ordpos 反向索引（槽 → order 位置）让删除是 O(1)。 */ \
        s->used[hit] = 2; s->holes++; \
        aic_usize p = s->ordpos[hit]; \
        aic_usize last = s->order[--s->len]; \
        s->order[p] = last; s->ordpos[last] = p; \
        (void)file; (void)line; \
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
        s->len = 0; s->holes = 0; \
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
AIC_SET_OP(f64, aic_f64, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(bool, bool, aic_pod_hash, aic_pod_eq)
AIC_SET_OP(str, aic_str, aic_str_hash, aic_str_keq)

#endif /* AIC_L1_SET_H */
