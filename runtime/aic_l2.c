/* AIC runtime L2 —— 调度层实现（核心设计 §七；L3 = 真并发）。
 *
 * 内存纪律：任务节点与实参块由 L2 自己 malloc/free（不占区域——任务实参可能指向
 * 父任务对象，而父任务区域在 scope 内仍然存活，§七「spawn 实参 = 共享」）。
 *
 * 为什么任务 = 真线程：设计 §七 的【真并发后端决议】已按事实定案（C11 `<threads.h>`
 * 在本机三套工具链都不存在）—— POSIX 走 pthreads、Windows 走 Win32 子集，
 * 等待用「锁 + 让出 CPU 轮询」而不是条件变量（tcc 的 windows.h 无 CONDITION_VARIABLE）。
 * 任务池/工作窃取属阶段 O 的性能工作（当前 1:1，spawn 密集时线程创建是瓶颈）。
 */

#include "aic_l2.h"

#include <stdlib.h>

/* --------------------------------------------------------------------------
 * 全局调度状态
 * ------------------------------------------------------------------------*/
static aic_mtx_t g_mtx;          /* 保护 g_live / g_waiting / 静默轮数 */
static aic_u32   g_live = 0;     /* 存活任务数（含 main） */
static aic_u32   g_waiting = 0;  /* 停在**无截止点**等待上的任务数 */
static aic_u32   g_quiet_rounds = 0; /* "全体等待且无进展"的连续轮数 */
static aic_i64   g_quiet_since = 0;  /* 该状态起始时刻（单调毫秒；0 = 无） */
static aic_mtx_t g_token;        /* 串行模式的全局令牌 */
static aic_mtx_t g_mtx_init_lock;/* 惰性初始化 aic_mutex 的全局锁 */
static int       g_ready = 0;
static int       g_serial = -1;  /* -1 未探测；0 并行；1 串行 */

/* 死锁判定 = **全体等待 + 连续 K 轮无进展 + 静默 T 毫秒**（两个条件都要）：
 *   - "全体等待"只说明此刻没人跑，不等于死锁 —— 某个等待者的条件可能**已经成立**
 *     （如"生产者满写"与"消费者待收"同一通道），它下次醒来就会推进并登记进展；
 *   - **只看轮数不够**：自旋中的任务每秒能 park 上千轮，K 轮可能在微秒内凑齐，
 *     而对方还睡在 Sleep(1) 里（Windows 的 Sleep(1) 实际约 15.6ms，默认定时器精度）
 *     ⇒ 必须再加一个时间下限，保证对方有机会醒来；
 *   - **只看时间也不够**（实测教训：100ms 墙钟阈值在管道锚点上误报）；
 *   - 进展 = 通道收发 / 任务结束（aic_sched_note_progress）。本语言的等待点只有
 *     chan/select（无外部事件源）⇒ 两条件同时满足就是真的没人能推进。
 *   **窗口取值（3000ms）的理由**：这个判据是"没人能推进"的启发式，误报的代价是
 *   **把一个活的程序打成 trap**。250ms 在重负载下不够（实测三档门禁并发跑时
 *   `smoke_l3_pipeline` 偶发假 trap：生产者两次 note_progress 之间的正常计算 + 一串
 *   调度抖动就跨过了窗口）。窗口的语义是"**全体挂着且无进展**持续多久"，
 *   3s 既远大于任何调度抖动（Windows 睡眠粒度 15.6ms 的百倍以上），又远小于人的
 *   "卡死了"体感；真死锁照旧 trap（trap/769、773 与 smoke_l3_deadlock 实测）。
 *
 *   阶段 O 换等待图/条件变量后删除本启发式。 */
#define AIC_DEADLOCK_QUIET_ROUNDS 8u
#define AIC_DEADLOCK_QUIET_MS     3000

/* 任何可能唤醒别的任务的状态变化都走这里（通道收发、任务结束）。 */
void aic_sched_note_progress(void) {
    if (!g_ready) {
        return;
    }
    aic_mtx_lock(&g_mtx);
    g_quiet_rounds = 0;
    g_quiet_since = 0;
    aic_mtx_unlock(&g_mtx);
}

/* 线程局部：是否持有令牌、自旋截止点、取消标记与截止点。 */
static AIC_TLS int      t_tok = 0;
static AIC_TLS aic_i64  t_spin_until = 0; /* 本任务的自旋窗口截止（单调毫秒） */
static AIC_TLS aic_u32  t_cancel = 0;
static AIC_TLS aic_i64  t_deadline = 0; /* 0 = 无 scope.timeout */

bool aic_sched_serial(void) {
    if (g_serial < 0) {
        /* 环境变量只在 init（单线程时刻）读一次；这里兜底为并行。 */
        g_serial = 0;
    }
    return g_serial != 0;
}

void aic_sched_init(void) {
    if (g_ready) {
        return;
    }
    const char *v = getenv("AIC_SCHED_SERIAL");
    g_serial = (v != NULL && v[0] != '\0' && v[0] != '0') ? 1 : 0;
    aic_mtx_init(&g_mtx);
    aic_mtx_init(&g_mtx_init_lock);
    g_live = 1; /* main 自己 */
    if (g_serial) {
        aic_mtx_init(&g_token);
        aic_mtx_lock(&g_token); /* main 起手持有令牌 */
        t_tok = 1;
    }
    g_ready = 1;
}

aic_u32 aic_sched_live_tasks(void) {
    aic_sched_init();
    aic_u32 n;
    aic_mtx_lock(&g_mtx);
    n = g_live;
    aic_mtx_unlock(&g_mtx);
    return n;
}

/* 令牌：串行模式下"当前能跑"的唯一凭据。非串行模式是空操作。 */
static void token_release(void) {
    if (t_tok) {
        t_tok = 0;
        aic_mtx_unlock(&g_token);
    }
}
static void token_acquire(void) {
    if (aic_sched_serial() && !t_tok) {
        aic_mtx_lock(&g_token);
        t_tok = 1;
    }
}

void aic_sched_park(bool has_deadline) {
    aic_sched_park_at(has_deadline, "<runtime: scheduler>", 0, "waiting for another task");
}

void aic_sched_park_at(bool has_deadline, const char *file, int line, const char *what) {
    aic_sched_init();
    if (!has_deadline) {
        /* 进入无超时等待：登记等待者；**全体等待 + K 轮 + T 毫秒无进展** = 死锁。
         *
         * 两个信号的职责必须分清（实测教训）：
         *   - `g_quiet_rounds` = "观察到全体等待"的连续次数；
         *   - `g_quiet_since`  = 该窗口的起点；
         *   **窗口只由"进展"复位**（aic_sched_note_progress：通道收发/任务结束），
         *   **不因"某个任务又跑起来了"复位** —— 一个被唤醒、发现没东西可取、立刻重新
         *   挂起的任务**没有推进任何东西**，此前那种"有人能跑就复位窗口"的写法会让
         *   两个互相等待的任务交替挂起时窗口永远攒不够 ⇒ **真死锁反而不报**（trap/769、
         *   773 与 smoke_l3_deadlock 实测：窗口从 250ms 提到 1500ms 后彻底不报，正是因为
         *   交替挂起把起点不断清零）。 */
        aic_mtx_lock(&g_mtx);
        g_waiting++;
        if (g_waiting >= g_live) {
            aic_i64 now = aic_plat_monotonic_ms();
            if (g_quiet_since == 0) {
                g_quiet_since = now;
            }
            g_quiet_rounds++;
            if (g_quiet_rounds >= AIC_DEADLOCK_QUIET_ROUNDS &&
                now - g_quiet_since >= AIC_DEADLOCK_QUIET_MS) {
                g_waiting--; /* 报告前回退，保证 trap 路径的计数一致 */
                g_quiet_rounds = 0;
                g_quiet_since = 0;
                aic_mtx_unlock(&g_mtx);
                aic_sched_deadlock(file, line, what);
            }
        }
        aic_mtx_unlock(&g_mtx);
    }
    token_release();
    /* 两段式让出：先让时间片（低延迟），约 1ms 后睡 1ms（不吃满 CPU）。
     * 不能只按"自旋次数"决定：Windows 的 Sleep(1) 实际约 15.6ms，纯计数会在
     * 热交接（管道/背压）上退化成每轮 15.6ms。 */
    aic_i64 now = aic_plat_monotonic_ms();
    if (t_spin_until == 0) {
        t_spin_until = now + 1;
    }
    if (now < t_spin_until) {
        aic_thr_yield();
    } else {
        aic_plat_sleep_ms(1);
        t_spin_until = 0;
    }
    token_acquire();
    if (!has_deadline) {
        aic_mtx_lock(&g_mtx);
        g_waiting--;
        aic_mtx_unlock(&g_mtx);
    }
}

void aic_sched_deadlock(const char *file, int line, const char *what) {
    /* 死锁 = 语言级 bug 的一种（不是恢复得了的错误）：与其余 trap 同路退出。
     * what 是等待内容的短描述，进 trap 的上下文行（诊断要能直接定位）。 */
    aic_trap_ctx(AIC_TRAP_DEADLOCK, file, line, what);
}

void aic_sched_sleep_ms(aic_i64 ms, bool cancellable) {
    aic_sched_init();
    if (ms <= 0) {
        aic_thr_yield();
        return;
    }
    /* 睡眠有截止点 ⇒ 不计入"全体阻塞"；串行模式下让出令牌，别的任务才能推进。 */
    aic_i64 left = ms;
    while (left > 0) {
        if (cancellable && aic_sched_cancelled()) {
            return;
        }
        aic_i64 step = left > 10 ? 10 : left;
        token_release();
        aic_plat_sleep_ms(step);
        token_acquire();
        left -= step;
    }
}

bool aic_sched_cancelled(void) {
    if (t_cancel != 0) {
        return true;
    }
    if (t_deadline != 0 && aic_plat_monotonic_ms() >= t_deadline) {
        t_cancel = 1;
        return true;
    }
    return false;
}
void aic_sched_cancel_here(void) { t_cancel = 1; }
void aic_sched_cancel_clear(void) { t_cancel = 0; }

/* --------------------------------------------------------------------------
 * 通道等待（通用：只看公共头，不看元素类型）
 * ------------------------------------------------------------------------*/
void aic_chan_wait_send(const void *ch, const char *file, int line) {
    while (!aic_chan_head_can_send(ch)) {
        /* 只有"别的任务可能来收"才值得等；否则满 = 无处可去 ⇒ 背压点 trap
         * （与串行 L2 的观测一致：`AIC_TRAP_CHAN_FULL`，§16 N11）。 */
        if (aic_sched_live_tasks() <= 1) {
            aic_trap(AIC_TRAP_CHAN_FULL, file, line);
        }
        if (aic_sched_cancelled()) {
            return; /* 取消：调用方按被取消处理（发送不再进行） */
        }
        aic_sched_park_at(false, file, line, "waiting to send on a bounded channel that is full");
    }
}

bool aic_chan_wait_recv(const void *ch, const char *file, int line) {
    for (;;) {
        if (aic_chan_head_ready(ch)) {
            return true;
        }
        /* §七【O2】：本 scope 已无存活任务 ⇒ 永远不会有值了 ⇒ (零值, false)。 */
        if (aic_sched_live_tasks() <= 1) {
            return false;
        }
        if (aic_sched_cancelled()) {
            return false;
        }
        aic_sched_park_at(false, file, line, "waiting to receive from a channel");
    }
}

int aic_select_wait(const aic_sel_arm *arms, aic_u32 n, aic_i64 deadline_ms,
                    const char *file, int line) {
    for (;;) {
        for (aic_u32 i = 0; i < n; i++) {
            const void *ch = arms[i].ch;
            if (arms[i].kind == AIC_SEL_RECV) {
                if (aic_chan_head_ready(ch)) {
                    return (int)i;
                }
                continue;
            }
            if (aic_chan_head_can_send(ch)) {
                return (int)i;
            }
        }
        if (deadline_ms != 0 && aic_time_after_ready(deadline_ms)) {
            return -1;
        }
        if (aic_sched_cancelled()) {
            return -2;
        }
        /* 没有别的存活任务能推进：按 §16 N6 的两条落地 ——
         *   有 recv 臂 ⇒ 返回它（空队列的 recv 得 (零值, false)，即 §七【O2】）；
         *   只有 send/超时臂 ⇒ 真死锁，当场 trap（位置 = select 点）。 */
        if (aic_sched_live_tasks() <= 1) {
            for (aic_u32 i = 0; i < n; i++) {
                if (arms[i].kind == AIC_SEL_RECV) {
                    return (int)i;
                }
            }
            aic_sched_deadlock(file, line, "select has no ready arm and no task can unblock it");
        }
        aic_sched_park_at(deadline_ms != 0, file, line, "select has no ready arm");
    }
}

/* --------------------------------------------------------------------------
 * 任务与作用域
 * ------------------------------------------------------------------------*/
void aic_scope_enter(aic_scope *s) {
    aic_sched_init();
    s->head = NULL;
    s->tail = NULL;
    s->live = 0;
}

void *aic_task_env(aic_usize n) {
    /* 至少一个字节：C 里 malloc(0) 的返回值不可依赖。 */
    void *p = malloc(n == 0 ? 1 : n);
    if (p == NULL) {
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: scheduler>", 0);
    }
    return p;
}

/* 任务入口：任务 = **独立区域根**（自己的区域栈从深度 0 起）+ 实参块释放在最后。
 * 存活计数在**线程侧**增减（tcc 退化后端在同线程跑，不能动这个计数）。 */
void aic_task_run(void (*fn)(void *), void *env) {
    aic_task_init("<runtime: task>", 0);
    fn(env);
    free(env);
    aic_region_release_task(); /* 区域页归还全局空闲链：spawn 循环不泄漏 */
}

AIC_THR_ENTRY(aic_task_entry) {
    aic_task *t = (aic_task *)raw;
    aic_sched_init();
    token_acquire();
    aic_task_run(t->fn, t->env);
    aic_mtx_lock(&g_mtx);
    g_live--;
    aic_mtx_unlock(&g_mtx);
    aic_sched_note_progress(); /* 任务结束 = 可能有人因此能推进（O2 的 (零值,false)） */
    token_release();
    AIC_THR_RETURN();
}

void aic_scope_spawn(aic_scope *s, void (*fn)(void *), void *env) {
    aic_sched_init();
    aic_task *t = (aic_task *)malloc(sizeof(aic_task));
    if (t == NULL) {
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<runtime: scheduler>", 0);
    }
    t->fn = fn;
    t->env = env;
    t->started = false;
    t->next = NULL;
    if (s->tail == NULL) {
        s->head = t;
        s->tail = t;
    } else {
        s->tail->next = t;
        s->tail = t;
    }
    s->live++;
#if AIC_NO_TLS
    /* tcc 没有任何 TLS（区域状态会串线程）⇒ 该配置退化为"scope 退出时跑完"。 */
    t->started = true;
    return;
#else
    aic_mtx_lock(&g_mtx);
    g_live++;
    aic_mtx_unlock(&g_mtx);
    t->started = aic_thr_create(&t->thr, aic_task_entry, t);
    if (!t->started) {
        aic_mtx_lock(&g_mtx);
        g_live--;
        aic_mtx_unlock(&g_mtx);
        aic_trap(AIC_TRAP_OUT_OF_MEMORY, "<l2/thread>", 0);
    }
#endif
}

void aic_scope_exit(aic_scope *s) {
#if AIC_NO_TLS
    /* 退化后端：按登记序**在同线程**串行跑完（旧 T1 行为）。
     * 区域栈是全局的，故必须把调用者的区域状态搬开再搬回 —— 否则任务体覆盖了
     * 调用者的区域栈，任务结束后调用者的分配会落进"已被弹出的区域"（实测 trap）。 */
    aic_region_state st;
    aic_region_state_save(&st);
    aic_u32 saved_cancel = t_cancel;
    aic_i64 saved_deadline = t_deadline;
    aic_task *t = s->head;
    while (t != NULL) {
        aic_task *next = t->next;
        aic_task_run(t->fn, t->env);
        free(t);
        t = next;
    }
    t_cancel = saved_cancel;
    t_deadline = saved_deadline;
    aic_region_state_restore(&st);
#else
    /* 关键：join 期间必须**交出令牌**，否则子任务永远拿不到 CPU（串行模式下会挂死）。 */
    token_release();
    for (aic_task *t = s->head; t != NULL; t = t->next) {
        if (t->started) {
            aic_thr_join(t->thr);
        }
        s->live--;
    }
    token_acquire();
    aic_task *t = s->head;
    while (t != NULL) {
        aic_task *next = t->next;
        free(t);
        t = next;
    }
#endif
    s->head = NULL;
    s->tail = NULL;
}

void aic_scope_timeout(aic_i64 ms) {
    aic_sched_init();
    t_deadline = aic_plat_monotonic_ms() + (ms > 0 ? ms : 0);
}

/* --------------------------------------------------------------------------
 * sync.Mutex：真临界区
 * ----------------------------------------------------------------------------
 * 零值 = 未初始化（C 里结构体零值即 memset 0），而 CRITICAL_SECTION/pthread_mutex_t
 * 不能"零值直接用" ⇒ 惰性初始化：用全局初始化锁保护 `_ready` 的发布。
 * 初始化只发生一次；`g_mtx_init_lock` 本身在 aic_sched_init（单线程时刻）建好。 */
static void mutex_init_once(aic_mutex *m) {
    if (!g_ready) {
        aic_sched_init();
    }
    aic_mtx_lock(&g_mtx_init_lock);
    if (!m->_ready) {
        aic_mtx_init(&m->_m);
        m->_ready = 1;
    }
    aic_mtx_unlock(&g_mtx_init_lock);
}

void aic_mutex_lock(aic_mutex *m) {
    if (m == NULL) {
        return;
    }
    if (!m->_ready) {
        mutex_init_once(m);
    }
    aic_mtx_lock(&m->_m);
}

void aic_mutex_unlock(aic_mutex *m) {
    if (m == NULL) {
        return;
    }
    if (!m->_ready) {
        mutex_init_once(m);
    }
    aic_mtx_unlock(&m->_m);
}
