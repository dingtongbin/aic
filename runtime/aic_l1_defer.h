/* AIC runtime L1 —— defer 栈（核心设计 §三：defer 函数级 LIFO，无条件执行）。
 * C 无 defer，emit 把每个出口汇入一个尾声：defer 点把 (trampoline, ctx) 压栈。
 * 栈长度动态（循环内 defer 必须可用，固定容量会在普通代码上 trap）。
 * 该栈是运行时内部簿记（非 AIC 对象），用 malloc，不占区域。 */
#ifndef AIC_L1_DEFER_H
#define AIC_L1_DEFER_H

#include "aic_l0.h"
#include <stdlib.h>

typedef void (*aic_defer_fn)(void *ctx);
/* onerr = 这条是 errdefer（N8）：只在函数**因 Err 返回**时执行。
 * 两类共用同一条栈 ⇒ LIFO 跨越 defer/errdefer 统一（注册序的严格逆序）。 */
typedef struct { aic_defer_fn fn; void *ctx; int onerr; } aic_defer_entry;
typedef struct { aic_defer_entry *slots; aic_usize n; aic_usize cap; } aic_defer_stack;

#define AIC_DEFER_STACK_EMPTY { NULL, 0, 0 }

static inline void aic_defer_push_ex(aic_defer_stack *st, aic_defer_fn fn, void *ctx,
                                     int onerr, const char *file, int line) {
    if (st->n >= st->cap) {
        aic_usize cap = st->cap ? st->cap * 2 : 8;
        aic_defer_entry *p = (aic_defer_entry *)realloc(st->slots, cap * sizeof(aic_defer_entry));
        if (p == NULL) {
            aic_trap(AIC_TRAP_OUT_OF_MEMORY, file, line);
        }
        st->slots = p;
        st->cap = cap;
    }
    st->slots[st->n].fn = fn;
    st->slots[st->n].ctx = ctx;
    st->slots[st->n].onerr = onerr;
    st->n++;
}

/* 普通 defer：无条件执行。 */
static inline void aic_defer_push(aic_defer_stack *st, aic_defer_fn fn, void *ctx,
                                  const char *file, int line) {
    aic_defer_push_ex(st, fn, ctx, 0, file, line);
}

/* 拷贝式注册：把 ctx 字节复制进栈自有存储。
 * 循环内注册同一 defer 点必须各有独立上下文（034 语料：循环里注册 3 次 = 出口跑 3 次，
 * 若只存指向调用方帧的指针，三次注册会共享同一槽位），故此处再复制一份。 */
static inline void aic_defer_push_copy_ex(aic_defer_stack *st, aic_defer_fn fn,
                                          const void *ctx, aic_usize size, int onerr,
                                          const char *file, int line) {
    void *copy = NULL;
    if (size > 0) {
        copy = malloc(size);
        if (copy == NULL) {
            aic_trap(AIC_TRAP_OUT_OF_MEMORY, file, line);
        }
        memcpy(copy, ctx, size);
    }
    aic_defer_push_ex(st, fn, copy, onerr, file, line);
}

/* 普通 defer 的拷贝式注册（语义同 aic_defer_push_copy_ex(…, 0, …)）。 */
static inline void aic_defer_push_copy(aic_defer_stack *st, aic_defer_fn fn,
                                       const void *ctx, aic_usize size,
                                       const char *file, int line) {
    aic_defer_push_copy_ex(st, fn, ctx, size, 0, file, line);
}

/* LIFO：最后注册的 defer 最先运行；跑完释放上下文与槽位表（二次调用是空操作）。
 * err 是本次返回的 Err code（0 = 正常返回）：onerr 项只在 err != 0 时执行。
 * 条目一律出栈（不跑也要释放上下文），否则函数内二次注册会泄漏。 */
#define AIC_DEFER_RUN_ERR(st, err) \
    do { \
        while ((st)->n > 0) { \
            (st)->n--; \
            if (!(st)->slots[(st)->n].onerr || (err) != 0) { \
                (st)->slots[(st)->n].fn((st)->slots[(st)->n].ctx); \
            } \
            free((st)->slots[(st)->n].ctx); \
            (st)->slots[(st)->n].ctx = NULL; \
        } \
        free((st)->slots); \
        (st)->slots = NULL; \
        (st)->cap = 0; \
    } while (0)

/* 无 errdefer 的函数：全部条目无条件执行。 */
#define AIC_DEFER_RUN(st) AIC_DEFER_RUN_ERR(st, 1)

#endif /* AIC_L1_DEFER_H */
