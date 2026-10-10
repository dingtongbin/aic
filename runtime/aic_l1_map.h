/* AIC runtime L1 —— map[K]V（核心设计 §二.2）. 引用对象；插入序遍历（H3 确定）。
 * 方法：new/put/get/has/remove/len/isEmpty/keyAt/valAt。
 * put 已存在键 = 覆盖值保原位；get 未命中 = 零值；remove 不存在 = 无操作；
 * keyAt/valAt 越界 = trap。
 * **删除用墓碑（used = 2）**：remove 只标记槽位 + order 原地压缩（零分配）。
 * 曾经 remove 每次分配 4 个满容量新数组，而区域分配器不归还旧的 ⇒ 25000 次
 * 删除 = 50GB 失控（B8 bench 实测 OOM）。holes = 墓碑数，与 len 一起参与增长判据。
 * 符号名编码 (K,V) 两者：头未实例化的 (K,V) 无符号可调 = 编译错（§三）。 */
#ifndef AIC_L1_MAP_H
#define AIC_L1_MAP_H

#include "aic_l0.h"
#include <string.h>

/* 槽内字段名只在这里定义一次（布局中立访问器）。aic_l1_print 与 emit 侧
 * printgen 生成的都是这两个宏 ⇒ 改槽布局不必动编译器（曾经 emit 里硬编码
 * `v->keys[...]` / `v->vals[...]`，一改布局就与运行期失配 = 静默坏码）。 */
#define AIC_MAP_SLOT_KEY(m, i) ((m)->slots[i].k)
#define AIC_MAP_SLOT_VAL(m, i) ((m)->slots[i].v)

#define AIC_MAP_OP(KS, KT, VS, VT, HASHF, KEQF) \
    typedef struct aic_mslot_##KS##_##VS { KT k; VT v; } aic_mslot_##KS##_##VS; \
    /* 槽 = {key, val} 连续单数组（B3：探测链只碰 slots+used 两条流；旧布局 \
     * keys/vals/order/ordpos/used 五条流 = 每槽多读 24B 无关数据）。 \
     * order/ordpos 仍是插入序所需的独立数组（槽分散，与 set 同款）。 */ \
    typedef struct aic_map_##KS##_##VS { \
        aic_hdr hdr; aic_mslot_##KS##_##VS *slots; aic_usize *order; aic_u8 *used; \
        aic_usize *ordpos; \
        aic_usize len; aic_usize cap; aic_usize holes; \
    } aic_map_##KS##_##VS; \
    /* used 的三态：0 = 空、1 = 存活、2 = 墓碑（已删除）。 */ \
    static inline aic_usize aic_map_slot_##KS##_##VS(const aic_map_##KS##_##VS *m, KT k) { \
        aic_usize mask = m->cap - 1; \
        aic_usize i = (aic_usize)(HASHF(k)) & mask; \
        aic_usize first_hole = (aic_usize)-1; \
        for (;;) { \
            aic_u8 u = m->used[i]; \
            if (u == 0) return first_hole != (aic_usize)-1 ? first_hole : i; \
            if (u == 2) { \
                if (first_hole == (aic_usize)-1) first_hole = i; \
            } else if (KEQF(m->slots[i].k, k)) { \
                return i; \
            } \
            i = (i + 1) & mask; \
        } \
    } \
    static inline aic_usize aic_map_find_##KS##_##VS(const aic_map_##KS##_##VS *m, KT k) { \
        if (m->cap == 0) return (aic_usize)-1; \
        aic_usize mask = m->cap - 1; \
        aic_usize i = (aic_usize)(HASHF(k)) & mask; \
        while (m->used[i]) { \
            if (m->used[i] == 1 && KEQF(m->slots[i].k, k)) return i; \
            i = (i + 1) & mask; \
        } \
        return (aic_usize)-1; \
    } \
    static inline aic_map_##KS##_##VS *aic_map_new_##KS##_##VS(aic_u32 line) { \
        aic_map_##KS##_##VS *m = (aic_map_##KS##_##VS *)aic_alloc_hdr(sizeof(aic_map_##KS##_##VS), line); \
        m->slots = NULL; m->order = NULL; m->used = NULL; m->ordpos = NULL; \
        m->len = 0; m->cap = 0; m->holes = 0; \
        return m; \
    } \
    static inline void aic_map_rehash_##KS##_##VS(aic_map_##KS##_##VS *m, aic_usize ncap, aic_u32 line) { \
        aic_mslot_##KS##_##VS *os = m->slots; aic_usize *oo = m->order; aic_usize olen = m->len; \
        m->slots = (aic_mslot_##KS##_##VS *)aic_alloc_in(m->hdr.reg, ncap * sizeof(aic_mslot_##KS##_##VS), line); \
        m->order = (aic_usize *)aic_alloc_in(m->hdr.reg, ncap * sizeof(aic_usize), line); \
        m->used = (aic_u8 *)aic_alloc_in(m->hdr.reg, ncap * sizeof(aic_u8), line); \
        m->ordpos = (aic_usize *)aic_alloc_in(m->hdr.reg, ncap * sizeof(aic_usize), line); \
        memset(m->used, 0, ncap * sizeof(aic_u8)); \
        m->cap = ncap; m->len = 0; m->holes = 0; \
        aic_usize i; \
        for (i = 0; i < olen; i++) { \
            aic_usize oi = oo[i]; \
            aic_usize j = aic_map_slot_##KS##_##VS(m, os[oi].k); \
            m->slots[j] = os[oi]; m->used[j] = 1; m->order[m->len] = j; m->ordpos[j] = m->len; m->len++; \
        } \
    } \
    static inline void aic_map_put_##KS##_##VS(aic_map_##KS##_##VS *m, KT k, VT v, \
                                               const char *file, int line) { \
        AIC_LIST_WRITABLE(m, file, line); \
        aic_usize hit = aic_map_find_##KS##_##VS(m, k); \
        if (hit != (aic_usize)-1) { m->slots[hit].v = v; return; } \
        if (m->cap == 0 || (m->len + m->holes + 1) * 4 >= m->cap * 3) { \
            aic_map_rehash_##KS##_##VS(m, m->cap ? m->cap * 2 : 8, (aic_u32)line); \
        } \
        aic_usize i = aic_map_slot_##KS##_##VS(m, k); \
        if (m->used[i] == 2) m->holes--; \
        m->slots[i].k = k; m->slots[i].v = v; m->used[i] = 1; m->order[m->len] = i; m->ordpos[i] = m->len; m->len++; \
    } \
    static inline bool aic_map_has_##KS##_##VS(const aic_map_##KS##_##VS *m, KT k) { \
        return m != NULL && aic_map_find_##KS##_##VS(m, k) != (aic_usize)-1; \
    } \
    static inline VT aic_map_get_##KS##_##VS(const aic_map_##KS##_##VS *m, KT k) { \
        VT zero; \
        memset(&zero, 0, sizeof(zero)); \
        if (m == NULL) return zero; \
        aic_usize i = aic_map_find_##KS##_##VS(m, k); \
        return i == (aic_usize)-1 ? zero : m->slots[i].v; \
    } \
    static inline void aic_map_remove_##KS##_##VS(aic_map_##KS##_##VS *m, KT k, \
                                                  const char *file, int line) { \
        if (m == NULL || m->cap == 0) return; \
        aic_usize hit = aic_map_find_##KS##_##VS(m, k); \
        if (hit == (aic_usize)-1) return; \
        /* 墓碑 + order 原地压缩（零分配）。曾经这里每次删除都分配 4 个满容量 \
         * 新数组而区域分配器不归还旧的 ⇒ 25000 次删除 = 50GB 失控 \
         * （B8 bench 实测 OOM；增长判据把 holes 算进去，墓碑不会把探测链拖长）。 \
         * ordpos 反向索引（槽 → order 位置）让删除是 O(1)：与 order 末位交换。 */ \
        m->used[hit] = 2; m->holes++; \
        aic_usize p = m->ordpos[hit]; \
        aic_usize last = m->order[--m->len]; \
        m->order[p] = last; m->ordpos[last] = p; \
        (void)file; (void)line; \
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
        m->len = 0; m->holes = 0; \
    } \
    static inline KT aic_map_key_at_##KS##_##VS(const aic_map_##KS##_##VS *m, aic_usize i, \
                                                const char *file, int line) { \
        if (m == NULL || i >= m->len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); \
        return AIC_MAP_SLOT_KEY(m, m->order[i]); \
    } \
    static inline VT aic_map_val_at_##KS##_##VS(const aic_map_##KS##_##VS *m, aic_usize i, \
                                                const char *file, int line) { \
        if (m == NULL || i >= m->len) aic_trap(AIC_TRAP_INDEX_OUT_OF_BOUNDS, file, line); \
        return AIC_MAP_SLOT_VAL(m, m->order[i]); \
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

/* bool 值槽（R21 审计 ③：`map[str]bool` 是最高频缺口 —— 开关表/存在性标记）。
 * bool 的实例化同样必须**直接调 AIC_MAP_OP**（VS 紧邻 ## 才不被预扫描展开，
 * 见上面 bool 键的同款说明）。 */
AIC_MAP_OP(i32, aic_i32, bool, bool, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(i64, aic_i64, bool, bool, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(u32, aic_u32, bool, bool, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(u64, aic_u64, bool, bool, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(usize, aic_usize, bool, bool, aic_pod_hash, aic_pod_eq)
AIC_MAP_OP(str, aic_str, bool, bool, aic_str_hash, aic_str_keq)
AIC_MAP_OP(bool, bool, bool, bool, aic_pod_hash, aic_pod_eq)

#endif /* AIC_L1_MAP_H */
