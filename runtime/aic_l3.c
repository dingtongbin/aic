/* AIC runtime L3 —— 协程层（T1：Windows Fiber 载体 / POSIX pthread 回退）。
 *
 * 为什么选 Fiber：Windows 原生协程原语 —— 用户态切换（~100ns）、独立栈、
 * 回调式入口，不需要一行汇编，MS ABI 的保存集由 OS 管。POSIX 没有等价物，
 * 用 ucontext（glibc/macOS 有）或 pthread 回退；tcc 不走本层（AIC_L3_OFF）。
 *
 * 内存安全（不因载体改变）：
 *   - 协程 = 任务 = 独立区域根（R19 slot_acquire/release，退出归还记得）；
 *   - 让出/阻塞不弹区域 ⇒ R1/R3 与存储点守卫逐条保持；
 *   - Fiber 栈由 OS 按需增长页提交，无手写增长路径可错。
 */
#include "aic_l3.h"

#include <stdlib.h>

/* --- 平台载体 ---------------------------------------------------------------*/
#if defined(_WIN32)

#include <windows.h>

typedef LPVOID aic_coro_native; /* Fiber 句柄 */

static LPVOID g_main_fiber = NULL; /* 调度器所在的"主 Fiber"（首个协程切入时建） */

/* Fiber 入口：参数经 ConvertThreadToFiber 的 lpParameter 传进来。 */
static VOID CALLBACK coro_fiber_proc(LPVOID param) {
    aic_coro *c = (aic_coro *)param;
    c->state = AIC_CORO_RUNNING;
    aic_task_init("<runtime: coro>", 0);
    c->fn(c->env);
    aic_region_release_task();
    c->state = AIC_CORO_DONE;
    /* 切回调度器：Fiber 被DeleteFiber前必须切出，故这里不返回 */
    SwitchToFiber(g_main_fiber);
}

#else /* POSIX：ucontext 可用则用，否则退化线程 */

#include <ucontext.h>

typedef ucontext_t aic_coro_native;

#endif

/* --------------------------------------------------------------------------
 * 协程与调度（T1 = 单线程 FIFO；T3 改工作池）
 * ------------------------------------------------------------------------*/
static aic_coro *g_ready_head = NULL;
static aic_coro *g_ready_tail = NULL;
static aic_coro *g_current = NULL;   /* 正在运行的协程（让出/退出点用） */
static int       g_inited = 0;
#if !defined(_WIN32)
static ucontext_t g_sched_ctx;       /* POSIX：调度器上下文 */
#endif

static void enqueue(aic_coro *c) {
    c->next = NULL;
    if (g_ready_tail == NULL) {
        g_ready_head = c;
        g_ready_tail = c;
        return;
    }
    g_ready_tail->next = c;
    g_ready_tail = c;
}

static aic_coro *dequeue(void) {
    aic_coro *c = g_ready_head;
    if (c == NULL) {
        return NULL;
    }
    g_ready_head = c->next;
    if (g_ready_head == NULL) {
        g_ready_tail = NULL;
    }
    c->next = NULL;
    return c;
}

/* 协程栈：16KB 初始；Fiber 由 OS 自动增长，ucontext 需显式栈。 */
#define AIC_CORO_STACK_INIT (16u * 1024u)

void *aic_coro_stack_limit(void) { return NULL; } /* OS 栈：不查 */
void *aic_coro_stack_grow(void) { return NULL; }

aic_coro *aic_coro_create_sized(void (*fn)(void *), void *env, aic_usize stack_size) {
    (void)stack_size;
    aic_coro *c = (aic_coro *)calloc(1, sizeof(aic_coro));
    if (c == NULL) {
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: coro>", 0);
    }
    c->stack_size = AIC_CORO_STACK_INIT;
    c->state = AIC_CORO_READY;
    c->fn = fn;
    c->env = env;
    c->next = NULL;
#if defined(_WIN32)
    if (g_main_fiber == NULL) {
        g_main_fiber = ConvertThreadToFiber(NULL);
        if (g_main_fiber == NULL) {
            aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<l3/fiber>", 0);
        }
    }
    c->native = CreateFiberEx(AIC_CORO_STACK_INIT, AIC_CORO_STACK_INIT * 8,
                              FIBER_FLAG_FLOAT_SWITCH, coro_fiber_proc, c);
    if (c->native == NULL) {
        free(c);
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<l3/fiber>", 0);
    }
#else
    c->stack = malloc(c->stack_size);
    if (c->stack == NULL) {
        free(c);
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: coro>", 0);
    }
    getcontext(&c->native);
    c->native.uc_stack.ss_sp = c->stack;
    c->native.uc_stack.ss_size = c->stack_size;
    c->native.uc_link = NULL;
    makecontext(&c->native, (void (*)(void))coro_ucontext_entry, 1, (void *)c);
#endif
    return c;
}

#if !defined(_WIN32)
static void coro_ucontext_entry(void *raw) {
    aic_coro *c = (aic_coro *)raw;
    c->state = AIC_CORO_RUNNING;
    aic_task_init("<runtime: coro>", 0);
    c->fn(c->env);
    aic_region_release_task();
    c->state = AIC_CORO_DONE;
}
#endif

aic_coro *aic_coro_create(void (*fn)(void *), void *env) {
    return aic_coro_create_sized(fn, env, 0);
}

/* T1 调度：单线程 FIFO —— 入队并逐个跑到让出/结束。 */
void aic_coro_schedule(aic_coro *c) {
    enqueue(c);
    if (g_inited) {
        return; /* 已在调度循环里：只入队（T2 起阻塞/就绪都走这里） */
    }
    g_inited = 1;
    for (;;) {
        aic_coro *n = dequeue();
        if (n == NULL) {
            break;
        }
        g_current = n;
        if (n->state == AIC_CORO_DONE) {
            aic_coro_destroy(n);
            g_current = NULL;
            continue;
        }
#if defined(_WIN32)
        SwitchToFiber(n->native); /* 切入；协程让出/退出时回到这里 */
#else
        swapcontext(&g_sched_ctx, &n->native);
#endif
        g_current = NULL;
    }
    g_inited = 0;
}

void aic_coro_yield(void) {
    aic_coro *c = g_current;
    if (c == NULL) {
        return;
    }
    c->state = AIC_CORO_READY;
    enqueue(c);
    g_current = NULL;
#if defined(_WIN32)
    SwitchToFiber(g_main_fiber);
#else
    swapcontext(&c->native, &g_sched_ctx);
#endif
}

aic_coro *aic_coro_current(void) { return g_current; }

void aic_coro_exit(void) {
    /* 与载体收尾路径冲突；正常不应被直接调用（Fiber/ucontext 入口已收尾）。 */
    aic_trap(AIC_TRAP_UNREACHABLE, "<runtime: coro>", 0);
}

void aic_coro_destroy(aic_coro *c) {
    if (c == NULL) {
        return;
    }
#if defined(_WIN32)
    if (c->native != NULL) {
        DeleteFiber(c->native);
    }
#else
    free(c->stack);
#endif
    free(c);
}

void aic_coro_set_workers(aic_u32 n) { (void)n; /* T3 */ }
