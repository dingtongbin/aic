/* L3 冒烟：**死锁判定**（§七 / §16 N6）。
 *
 * 场景：两个任务互相等对方的通道（主任务等任务发，任务等主任务发）⇒ 全体阻塞。
 * 运行时必须当场 trap（"deadlock: every task is waiting and none can make progress"），
 * 绝不静默挂死。期望：非 0 退出 + 该 stderr 前缀。 */
#include "aic_l2.h"

static aic_chan_i32 *g_from_task;
static aic_chan_i32 *g_to_task;

static void waiter(void *env) {
    (void)env;
    aic_chan_i32_recv_t r = aic_chan_recv_i32(g_from_task, "smoke_l3_deadlock.c", 12);
    aic_println_i32(r._0);
}

int main(void) {
    aic_task_init("<smoke>", 1);
    aic_sched_init();
    g_from_task = aic_chan_new_i32(0, 8);
    g_to_task = aic_chan_new_i32(0, 9);
    aic_scope s;
    aic_scope_enter(&s);
    aic_scope_spawn(&s, waiter, aic_task_env(1));
    aic_chan_i32_recv_t r = aic_chan_recv_i32(g_to_task, "smoke_l3_deadlock.c", 20);
    aic_println_i32(r._0);
    aic_scope_exit(&s);
    return 0;
}
