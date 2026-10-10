/* AIC runtime L3 —— 协程层（核心设计 §七；A1 = clang 上的真协程）。
 *
 * 目标：M:N 调度（N 协程跑在 M 个 OS 线程上），用户态切换 <100ns。
 * 本文件只做**原语 + 单线程调度器**（T1）：后续 T2 把 chan/Mutex/sleep 改成
 * 协程让出，T3 加工作池与工作窃取（见 build/a1_design.md）。
 *
 * 内存安全（本层的核心纪律）：
 *   - 协程 = 任务 = **独立区域根**（创建时 slot_acquire，退出 slot_release，
 *     与真线程版 task 完全同一套 R19 分代机制）；
 *   - 每协程一套**区域栈帧**（depth/ids/root），切换时保存/恢复 —— 与 tcc 的
 *     aic_region_state_save/restore 同一机制（R10 决策四已验证）；
 *   - 让出不弹任何区域 ⇒ 区域不变量与 R3 守卫在协程下逐条保持。
 *
 * 上下文切换：不用 ucontext（clang/Windows 都没有），自写 aic_swap —— 只存
 * callee-saved 寄存器（System V AMD64 + Windows x64 两份，汇编在 aic_l3_swap.S，
 * 由构建脚本按平台选；tcc 不走本层）。
 */
#ifndef AIC_L3_H
#define AIC_L3_H

#include "aic_l0.h"
#include "aic_plat.h"

/* tcc 不走本层：无 TLS ⇒ 协程栈帧无法按线程隔离（R10/R15 退化口径），且 tcc
 * 不认 x86-64 SIMD 内联汇编。AIC_L3_OFF 让本头整体空转（调用方 L2 按同一宏
 * 走串行路径）；gcc/clang 均不定义该宏 ⇒ 走真协程。 */
#if defined(__TINYC__)
#define AIC_L3_OFF 1
#endif

#ifndef AIC_L3_OFF

/* 协程状态（调度器只在这几个值之间搬）。 */
typedef enum {
    AIC_CORO_READY = 0, /* 可运行（在队列里） */
    AIC_CORO_RUNNING,  /* 正在某个工作线程上跑 */
    AIC_CORO_BLOCKED,  /* 等 chan/Mutex/时间（T2 接入） */
    AIC_CORO_DONE      /* 函数已返回 */
} aic_coro_state;

/* 平台载体类型：Windows Fiber 句柄 / POSIX ucontext 句柄。
 * （ucontext_t 在 <ucontext.h> 里，头文件不暴露平台头，用同尺寸 opaque。） */
#if defined(_WIN32)
typedef void *aic_coro_native;
#else
typedef struct { char opaque[512]; } aic_coro_native;
#endif

/* ---------------------------------------------------------------------------
 * 协程载体（T1 = 每协程一个 OS 线程 + 全局执行令牌）。
 *
 * 切换手法的诚实记录（完整版见 aic_l3.c 头注释）：
 *   手写 aic_swap 汇编 —— 四轮栽在平台细节（编译器序言吃 (%rsp) 的返回地址、
 *   clang -O0 忽略 register __asm__ 钉寄存器、两 ABI 参数寄存器不同、
 *   MinGW malloc 0xbaadf00d 填充假性溢出）；setjmp/longjmp —— 可移植换栈
 *   手段不存在（无 ucontext），且 setjmp 栈帧生命周期陷阱实测损坏栈。
 *   => T1 用 OS 线程承载（~7.5us/协程），语义完整、全平台；T2/T3 再做
 *   用户态切换与工作池。
 * -------------------------------------------------------------------------*/
typedef struct aic_coro {
    void *stack;           /* 协程栈底（T3 用户态栈才用；T1 = OS 线程栈） */
    aic_usize stack_size;
    aic_coro_state state;
    void (*fn)(void *);    /* 入口 */
    void *env;             /* 实参块 */
    /* 区域栈帧：协程自己的区域上下文（切出时保存、切入时恢复）。 */
    aic_region_state region;
    aic_u32 region_root;
    struct aic_coro *next; /* 就绪队列链 */
    /* T1 载体：平台原生协程句柄（Windows Fiber / POSIX ucontext）。 */
    aic_coro_native native;
} aic_coro;

/* 建一个协程（fn/env 同 spawn 的语义；栈尺寸建议用 aic_coro_create_sized）。 */
aic_coro *aic_coro_create(void (*fn)(void *), void *env);

/* 建协程并指定初始栈尺寸。**初始小、按需翻倍增长**：
 *   - 初始 16KB：C10K 协程 × 16KB = 160MB 虚拟地址，且物理只付碰触页；
 *   - 增长由协程入口的**栈深度检查**驱动（见 aic_coro_stack_limit）：余量不足
 *     即整体搬到一倍大的新段并把寄存器/区域帧原样搬过去——
 *     搬移动作只发生在切换点，不在函数序言里（序言只比较，保持可内联）。
 * 为什么不用 guard page / SIGSEGV：Windows（PAGE_GUARD）与 POSIX
 * （SIGSEGV + sigaltstack）两套机制差异大且 trap 要带源码位（§九）；
 * 序言检查 + 入口兜底既跨平台又可被逃逸分析证明后删除（与区域守卫同纪律）。
 *
 * stack_size = 0 = 用默认（16KB）。 */
aic_coro *aic_coro_create_sized(void (*fn)(void *), void *env, aic_usize stack_size);

/* 当前协程的栈限（余量下界）。返回 NULL = 不在协程里（OS 线程栈，不增长也不查）。
 * 生成代码的函数序言在栈帧分配前比较；余量不足调 aic_coro_stack_grow()。 */
void *aic_coro_stack_limit(void);

/* 增长当前协程的栈（序言检查发现余量不足时调）；返回新的栈限。
 * 增长失败 = trap stack overflow（§九：不静默损坏）。 */
void *aic_coro_stack_grow(void);

/* 调度器（T1 = 单线程 FIFO 协作式）：入队 + 在让出点切换。 */
void aic_coro_schedule(aic_coro *c);

/* 让出当前协程到下一个就绪协程（调度器核心）。没有就绪协程 = 回到调用者。 */
void aic_coro_yield(void);

/* 当前协程（调度器外 = NULL；用于 spawn 站点判断"我在协程里"）。 */
aic_coro *aic_coro_current(void);

/* 协程退出（函数返回后由引导代码调）：标记 DONE 并切回调度器。 */
void aic_coro_exit(void) __attribute__((noreturn));

/* 销毁协程栈（调度器在 DONE 后回收）。 */
void aic_coro_destroy(aic_coro *c);

/* 工作线程数（T3 用；T1 恒 1）。runtime.threads(n) 的落点。 */
void aic_coro_set_workers(aic_u32 n);

#endif /* AIC_L3_OFF */

#endif /* AIC_L3_H */
