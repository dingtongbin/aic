/* ============================================================================
 * AIC runtime L1 —— 容器层（核心设计 §九）.
 * list 增长 / set·map 插入序哈希；用到才链入。容器全部是引用对象（带头），
 * 句柄 = 指向对象的指针，零值 = NULL（读作空、写即 trap，§二.3）。
 * 内存归于对象自身区域（hdr.reg），增长经 aic_alloc_in 在区域内重分配。
 * ==========================================================================*/
#ifndef AIC_L1_H
#define AIC_L1_H

#include "aic_l0.h"

/* --- 内容哈希：FNV-1a，跨配置确定（H3；不依赖地址/ASLR）--------------------- */
static inline aic_u64 aic_hash_bytes(const void *p, aic_usize n) {
    const aic_u8 *b = (const aic_u8 *)p;
    aic_u64 h = 1469598103934665603ULL;
    aic_usize i;
    for (i = 0; i < n; i++) {
        h ^= (aic_u64)b[i];
        h *= 1099511628211ULL;
    }
    return h;
}

/* 增量混合：@derive(Hash) 的字段级哈希按字段序折叠（同一初值 + 同一字段序 =
 * 同一结果，跨配置确定）。数值按值混合、str 按内容、嵌套对象按各自 hash。 */
static inline aic_u64 aic_hash_mix(aic_u64 h, aic_u64 v) {
    h ^= v;
    h *= 1099511628211ULL;
    return h;
}
static inline aic_u64 aic_hash_u64(aic_u64 h, aic_u64 v) { return aic_hash_mix(h, v); }
static inline aic_u64 aic_hash_i64(aic_u64 h, aic_i64 v) { return aic_hash_mix(h, (aic_u64)v); }
static inline aic_u64 aic_hash_f64(aic_u64 h, aic_f64 v) {
    aic_u64 bits;
    /* 按位模式哈希：+0.0 与 -0.0 得到不同结果（与 == 语义有意不同，见 §三 比较语义） */
    memcpy(&bits, &v, sizeof(bits));
    return aic_hash_mix(h, bits);
}
static inline aic_u64 aic_hash_str(aic_u64 h, aic_str s) {
    return aic_hash_mix(h, aic_hash_bytes(s.p, s.len));
}
/* @derive(Hash) 的初值（FNV-1a offset basis）。 */
#define AIC_HASH_SEED 1469598103934665603ULL

/* POD 键：按字节哈希、按 ABI 相等比较。str 键：按内容哈希/比较。 */
#define aic_pod_hash(k)  (aic_hash_bytes(&(k), sizeof(k)))
#define aic_pod_eq(a, b) ((a) == (b))
#define aic_str_hash(k)  (aic_hash_bytes((k).p, (k).len))
#define aic_str_keq(a, b) (aic_str_eq((a), (b)))

#include "aic_l1_list.h"
#include "aic_l1_set.h"
#include "aic_l1_map.h"
#include "aic_l1_defer.h"
#include "aic_l1_print.h"

#endif /* AIC_L1_H */
