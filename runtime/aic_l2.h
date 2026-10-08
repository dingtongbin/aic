/* AIC runtime L2 —— 调度层（核心设计 §七）。
 *
 * **本文件 = 任务/作用域/通道/互斥的唯一定义处**：emit 只按这里的 API 发射，
 * 换调度实现只改本层（§七 的原始设计意图：T1 串行 → 真并发只换这一处）。
 *
 * 后端（按事实选择，记录在案）：
 *   - **任务 = 真线程（1:1）**：POSIX/pthreads、Windows/CreateThread —— 见 aic_plat.h；
 *   - **通道等待 = 「锁 + 让出 CPU 轮询」**，不用条件变量（tcc 的 windows.h 里没有
 *     CONDITION_VARIABLE；设计 §七 已定）。轮询分两段：先让出时间片，若干轮后睡 1ms。
 *   - **串行模式** `AIC_SCHED_SERIAL=1`：全局令牌保证同一时刻只有一个任务在跑，
 *     任务在**等待点释放令牌** ⇒ 单线程也能推进"生产者/消费者"。它与并行模式共用
 *     同一套语义，因此可作为并发的**对照配置**（§23 判据：两模式 stdout 逐位一致）。
 *   - **tcc（无 TLS）**：区域状态不是线程局部的 ⇒ 真并行会串区。该配置退化为
 *     "scope 退出时按登记序跑完"（与旧 T1 行为一致）；并发语料锚点必须与调度顺序无关。
 *
 * 死锁判定（O2/N6 的运行时载体）：
 *   - 全局计数 g_live（存活任务，含 main）与 g_waiting（**无截止点**的等待者）；
 *   - `g_waiting == g_live` = 全体任务都停在无超时的等待上 ⇒ 没有任何任务能推进 ⇒
 *     当场 trap `AIC_TRAP_DEADLOCK`（绝不静默挂死，也不静默跳过）；
 *   - 带截止点/取消点的等待（time 分支、scope.timeout、sleep）**不计入**等待者。
 */
#ifndef AIC_L2_H
#define AIC_L2_H

#include "aic_l0.h"
#include "aic_plat.h"

/* --- 调度核心（实现见 aic_l2.c）------------------------------------------- */

void aic_sched_init(void);          /* 幂等；任何用到 L2 的程序在 main 起手调一次 */

bool aic_sched_serial(void);        /* AIC_SCHED_SERIAL=1（读一次后缓存） */

/* 让出 CPU；has_deadline=true 表示调用方自带超时/取消点（不参与全体阻塞判定）。
 * 串行模式下先释放全局令牌再让出，醒来重新取回 —— 这就是协作式切换。
 * `_at` 变体带**等待点**（file/line + 一句话说明），死锁 trap 因此指到源码位。 */
void aic_sched_park(bool has_deadline);
void aic_sched_park_at(bool has_deadline, const char *file, int line, const char *what);

/* 存活任务数（含 main）。O2 的"还有没有别的任务可能来收发"判据的载体。 */
aic_u32 aic_sched_live_tasks(void);

/* 进展登记：任何可能唤醒别的任务的状态变化（通道收发、任务结束）。
 * 死锁判定 = "全体等待 + 静默窗口内无进展"，故通道侧必须调用（见 aic_l2_chan.h）。 */
void aic_sched_note_progress(void);

/* 全体阻塞 = 死锁：报告并退出（file/line = 等待点，生成代码传 __FILE__/__LINE__）。 */
AIC_COLD_NORETURN void aic_sched_deadlock(const char *file, int line, const char *what);

/* 带截止点的睡眠（`time.sleep` 的落点；串行模式下让出令牌）。 */
void aic_sched_sleep_ms(aic_i64 ms, bool cancellable);

/* 取消（N6 / scope.timeout / Ctx）：任务局部的协作式取消点。 */
bool aic_sched_cancelled(void);
void aic_sched_cancel_here(void);   /* 置位当前任务的取消标记 */
void aic_sched_cancel_clear(void);  /* 清除（scope.timeout 体结束后复位） */

/* --- 任务与作用域 --------------------------------------------------------- */
typedef struct aic_task {
    void (*fn)(void *);
    void *env;
    aic_thr thr;
    bool started;
    struct aic_task *next;
} aic_task;

/* 一个 scope = 任务链；退出时 join 全部（不等结果也等结束，§七）。 */
typedef struct {
    aic_task *head;
    aic_task *tail;
    aic_u32 live; /* 本 scope 内尚未结束的任务数（不含发起者） */
} aic_scope;

void  aic_scope_enter(aic_scope *s);
void  aic_scope_spawn(aic_scope *s, void (*fn)(void *), void *env);
void  aic_scope_exit(aic_scope *s);
void *aic_task_env(aic_usize n);
void  aic_task_run(void (*fn)(void *), void *env);

/* scope.timeout(ms)：登记截止点；到点即**请求取消**（任务树向下传播）。 */
void aic_scope_timeout(aic_i64 ms);

/* --- sync.Mutex（真临界区；零值 = 未初始化 = 未加锁）---------------------- */
typedef struct { aic_mtx_t _m; aic_u8 _ready; } aic_mutex;
void aic_mutex_lock(aic_mutex *m);
void aic_mutex_unlock(aic_mutex *m);

/* --- 通道公共头（N6/L3）----------------------------------------------------
 * 所有 `chan[T]` 实例的**头布局相同**（后缀只改元素类型），因此运行期能做泛型查询：
 * 就绪/可发/等待。C11 6.5.2.3：共同初始序列可经任一成员类型访问。 */
typedef struct {
    aic_hdr hdr;
    void *slots;
    aic_usize head, len, cap, bound;
} aic_chan_head;

static inline bool aic_chan_head_ready(const void *ch) {
    const aic_chan_head *h = (const aic_chan_head *)ch;
    return h != NULL && h->len != 0;
}
static inline bool aic_chan_head_can_send(const void *ch) {
    const aic_chan_head *h = (const aic_chan_head *)ch;
    if (h == NULL) return false;
    return h->bound == 0 || h->len < h->bound;
}

/* 阻塞等待（已就绪则立即返回）：
 *   wait_send 返回时保证可发；不可能就绪（满且无人能收）= 死锁 trap；
 *   wait_recv 返回 true = 有值；false = **不可能再有值** ⇒ 调用方返回 `(零值, false)`
 *   （§七【O2】）。 */
void aic_chan_wait_send(const void *ch, const char *file, int line);
bool aic_chan_wait_recv(const void *ch, const char *file, int line);

/* --- select 真等待（N6）---------------------------------------------------
 * 分支描述符：kind = AIC_SEL_RECV / AIC_SEL_SEND，ch = 通道实例（公共头即可）。
 * 返回：≥0 = 该分支就绪（随即做一次不会阻塞的收发）；-1 = 时间分支到点；-2 = 被取消。
 * 无就绪、无截止点、无取消 ⇒ 全体阻塞 ⇒ 本函数内 trap 死锁。 */
#define AIC_SEL_RECV 0u
#define AIC_SEL_SEND 1u
typedef struct { aic_u32 kind; const void *ch; } aic_sel_arm;
int aic_select_wait(const aic_sel_arm *arms, aic_u32 n, aic_i64 deadline_ms,
                    const char *file, int line);

/* --- 时间（N6）：单调毫秒截止点；不依赖 std 目标文件（红线 16 的依赖纪律）--- */
static inline bool aic_time_after_ready(aic_i64 deadline_ms) {
    return aic_plat_monotonic_ms() >= deadline_ms;
}
static inline aic_i64 aic_time_deadline(aic_i64 ms) {
    return aic_plat_monotonic_ms() + (ms > 0 ? ms : 0);
}

#include "aic_l2_chan.h"

#endif /* AIC_L2_H */
