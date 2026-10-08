/* AIC runtime L2 —— channel（核心设计 §七 / §16 N11 / N6）。
 *
 * 语义（§七 + 【O2 决议】，见规划 §十二 R9）：
 *   - `chan[T]` 的 T ∈ 安全集（基本类型 / str / @packed / 无负载 enum 及其容器）；
 *   - `new()` / `new(0)` = **无界**（send 入队即返回）；`new(cap>0)` = **有界**（满 = 背压点）；
 *   - `send(v)`：无界恒成立；有界且满 = **阻塞**（真并发下），若没有任何别的任务能收 ⇒ 死锁 trap；
 *   - `recv() -> (T, bool)`：空则**阻塞**；本 scope 已无存活任务 ⇒ `(零值, false)`（永不靠 close）；
 *   - channel 随 scope 存亡（在 scope 所在区域分配，句柄为零值 = 未初始化）。
 *
 * 头布局 = `aic_chan_head`（aic_l2.h）的**共同初始序列**：等待/就绪查询由运行期
 * 泛型完成（只看 head 的 len/bound），后缀只改元素类型。C11 6.5.2.3 允许这样访问。
 */
#ifndef AIC_L2_CHAN_H
#define AIC_L2_CHAN_H

#include "aic_l0.h"
#include <string.h>

#define AIC_CHAN_OP(short, ctype) \
    typedef struct aic_chan_##short { \
        aic_hdr hdr; ctype *slots; aic_usize head; aic_usize len; aic_usize cap; \
        aic_usize bound; /* 0 = 无界；>0 = 有界容量（背压点，§16 N11） */ \
    } aic_chan_##short; \
    /* 字段名 _0/_1 与 emit 的合成返回结构约定一致（多返回解构按位取字段）。 */ \
    typedef struct { ctype _0; bool _1; } aic_chan_##short##_recv_t; \
    static inline aic_chan_##short *aic_chan_new_##short(aic_usize cap, aic_u32 line) { \
        aic_chan_##short *ch = (aic_chan_##short *)aic_alloc_hdr(sizeof(aic_chan_##short), line); \
        /* cap = **已分配**容量（由 grow 管理）；bound = 语义上界（0 = 无界）。 \
         * 两者必须分开：构造时不预分配 slots（零值 = 惰性增长），但上界从第一 \
         * 次 send 起就生效 —— 用 cap 兼作上界会让 slots 永远为 NULL。 */ \
        ch->slots = NULL; ch->head = 0; ch->len = 0; ch->cap = 0; \
        ch->bound = cap; \
        return ch; \
    } \
    static inline void aic_chan_grow_##short(aic_chan_##short *ch, aic_u32 line) { \
        aic_usize ncap = ch->cap ? ch->cap + ch->cap / 2 + 1 : (ch->bound ? ch->bound : 4); \
        ctype *p = (ctype *)aic_alloc_in(ch->hdr.reg, ncap * sizeof(ctype), line); \
        if (ch->len != 0) memcpy(p, &ch->slots[ch->head], ch->len * sizeof(ctype)); \
        ch->slots = p; ch->head = 0; ch->cap = ncap; \
    } \
    static inline void aic_chan_send_##short(aic_chan_##short *ch, ctype v, \
                                             const char *file, int line) { \
        if (ch == NULL) aic_trap(AIC_TRAP_ZERO_CONTAINER_WRITE, file, line); \
        /* 有界（bound != 0）：满 = **背压点** —— 真并发下在这里阻塞（§16 N11）； \
         * 若无任何任务能收（活着只剩自己）= 死锁，由等待原语 trap。 */ \
        if (ch->bound != 0 && ch->len >= ch->bound) { \
            aic_chan_wait_send(ch, file, line); \
            if (ch->len >= ch->bound) { \
                return; /* 被取消：发送不再进行（协作式取消点，§16 N6） */ \
            } \
        } \
        if (ch->head + ch->len >= ch->cap) { \
            if (ch->head != 0) { \
                memmove(ch->slots, &ch->slots[ch->head], ch->len * sizeof(ctype)); \
                ch->head = 0; \
            } \
            if (ch->len >= ch->cap) aic_chan_grow_##short(ch, (aic_u32)line); \
        } \
        ch->slots[ch->head + ch->len] = v; \
        ch->len++; \
        aic_sched_note_progress(); /* 入队 = 可能唤醒等待的接收方（死锁判定用） */ \
    } \
    static inline aic_chan_##short##_recv_t aic_chan_recv_##short(aic_chan_##short *ch, \
                                                                 const char *file, int line) { \
        aic_chan_##short##_recv_t out; \
        memset(&out, 0, sizeof(out)); \
        if (ch == NULL) return out; \
        if (ch->len == 0) { \
            if (!aic_chan_wait_recv(ch, file, line)) return out; /* (零值, false)：不会再有了 */ \
        } \
        out._0 = ch->slots[ch->head]; \
        out._1 = true; \
        ch->head++; \
        ch->len--; \
        aic_sched_note_progress(); /* 出队 = 可能唤醒等待的发送方（有界背压） */ \
        return out; \
    } \
    static inline aic_usize aic_chan_len_##short(const aic_chan_##short *ch) { \
        return ch ? ch->len : 0; \
    } \
    /* 容量（0 = 无界）：cap() 查询用，也是"背压是否生效"的可观察事实。 */ \
    static inline aic_usize aic_chan_cap_##short(const aic_chan_##short *ch) { \
        return ch ? ch->bound : 0; \
    }

/* N6 select 的就绪判定：无阻塞查询，绝不改状态（真等待见 aic_select_wait）。 */
#define AIC_CHAN_READY_OP(short) \
    static inline bool aic_chan_ready_##short(const aic_chan_##short *ch) { \
        return aic_chan_head_ready(ch); \
    } \
    static inline bool aic_chan_can_send_##short(const aic_chan_##short *ch) { \
        return aic_chan_head_can_send(ch); \
    }

AIC_CHAN_OP(i8, aic_i8)
AIC_CHAN_READY_OP(i8)
AIC_CHAN_OP(i16, aic_i16)
AIC_CHAN_READY_OP(i16)
AIC_CHAN_OP(i32, aic_i32)
AIC_CHAN_READY_OP(i32)
AIC_CHAN_OP(i64, aic_i64)
AIC_CHAN_READY_OP(i64)
AIC_CHAN_OP(u8, aic_u8)
AIC_CHAN_READY_OP(u8)
AIC_CHAN_OP(u16, aic_u16)
AIC_CHAN_READY_OP(u16)
AIC_CHAN_OP(u32, aic_u32)
AIC_CHAN_READY_OP(u32)
AIC_CHAN_OP(u64, aic_u64)
AIC_CHAN_READY_OP(u64)
AIC_CHAN_OP(usize, aic_usize)
AIC_CHAN_READY_OP(usize)
AIC_CHAN_OP(f32, aic_f32)
AIC_CHAN_READY_OP(f32)
AIC_CHAN_OP(f64, aic_f64)
AIC_CHAN_READY_OP(f64)
AIC_CHAN_OP(bool, bool)
AIC_CHAN_READY_OP(bool)

/* --- `chan[str]`：**唯一需要深拷贝的实例**（§七「channel 只传值」）------------
 * str 的字节在**发送方**的区域里；发送方任务结束后该区域整条页链交还全局空闲链，
 * 接收方拿到的 (p,len) 就会悬垂。故 send 时把字节拷进**通道自己所在的区域**
 * （通道活多久，副本活多久），recv 侧零拷贝。基本类型按值拷贝，无需此路径。 */
typedef struct aic_chan_str {
    aic_hdr hdr; aic_str *slots; aic_usize head; aic_usize len; aic_usize cap;
    aic_usize bound;
} aic_chan_str;

typedef struct { aic_str _0; bool _1; } aic_chan_str_recv_t;

static inline aic_chan_str *aic_chan_new_str(aic_usize cap, aic_u32 line) {
    aic_chan_str *ch = (aic_chan_str *)aic_alloc_hdr(sizeof(aic_chan_str), line);
    ch->slots = NULL; ch->head = 0; ch->len = 0; ch->cap = 0;
    ch->bound = cap;
    return ch;
}

static inline void aic_chan_grow_str(aic_chan_str *ch, aic_u32 line) {
    aic_usize ncap = ch->cap ? ch->cap + ch->cap / 2 + 1 : (ch->bound ? ch->bound : 4);
    aic_str *p = (aic_str *)aic_alloc_in(ch->hdr.reg, ncap * sizeof(aic_str), line);
    if (ch->len != 0) memcpy(p, &ch->slots[ch->head], ch->len * sizeof(aic_str));
    ch->slots = p; ch->head = 0; ch->cap = ncap;
}

/* 深拷贝落点：目标区域 = 通道所在区域。 */
static inline aic_str aic_str_copy_into(aic_u32 reg, aic_str s, aic_u32 line) {
    aic_str out;
    if (s.len == 0 || s.p == NULL) {
        out.p = NULL;
        out.len = 0;
        return out;
    }
    char *p = (char *)aic_alloc_in(reg, s.len, line);
    memcpy(p, s.p, s.len);
    out.p = p;
    out.len = s.len;
    return out;
}

static inline void aic_chan_send_str(aic_chan_str *ch, aic_str v, const char *file, int line) {
    if (ch == NULL) aic_trap(AIC_TRAP_ZERO_CONTAINER_WRITE, file, line);
    if (ch->bound != 0 && ch->len >= ch->bound) {
        aic_chan_wait_send(ch, file, line);
        if (ch->len >= ch->bound) {
            return; /* 被取消 */
        }
    }
    aic_str copy = aic_str_copy_into(ch->hdr.reg, v, (aic_u32)line);
    if (ch->head + ch->len >= ch->cap) {
        if (ch->head != 0) {
            memmove(ch->slots, &ch->slots[ch->head], ch->len * sizeof(aic_str));
            ch->head = 0;
        }
        if (ch->len >= ch->cap) aic_chan_grow_str(ch, (aic_u32)line);
    }
    ch->slots[ch->head + ch->len] = copy;
    ch->len++;
    aic_sched_note_progress();
}

static inline aic_chan_str_recv_t aic_chan_recv_str(aic_chan_str *ch, const char *file, int line) {
    aic_chan_str_recv_t out;
    memset(&out, 0, sizeof(out));
    if (ch == NULL) return out;
    if (ch->len == 0) {
        if (!aic_chan_wait_recv(ch, file, line)) return out;
    }
    out._0 = ch->slots[ch->head];
    out._1 = true;
    ch->head++;
    ch->len--;
    aic_sched_note_progress();
    return out;
}

static inline aic_usize aic_chan_len_str(const aic_chan_str *ch) {
    return ch ? ch->len : 0;
}
static inline aic_usize aic_chan_cap_str(const aic_chan_str *ch) {
    return ch ? ch->bound : 0;
}
static inline bool aic_chan_ready_str(const aic_chan_str *ch) {
    return aic_chan_head_ready(ch);
}
static inline bool aic_chan_can_send_str(const aic_chan_str *ch) {
    return aic_chan_head_can_send(ch);
}

#endif /* AIC_L2_CHAN_H */
