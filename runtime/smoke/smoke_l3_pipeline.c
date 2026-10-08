/* L3 冒烟：**有界通道的真阻塞**（§16 N11）与任务 join。
 *
 * 场景：容量 1 的通道 + 一个生产者任务；主任务在 scope **内**消费。
 * 生产者发第 2 个值时必然满 ⇒ 必须真的阻塞，等消费者取走 —— 这要求两个任务
 * 并发推进（并行模式靠真线程；AIC_SCHED_SERIAL=1 靠等待点释放全局令牌）。
 * 两种模式必须给出**同一份 stdout**（判据见 scripts/runtime_smoke.py）。
 *
 * 期望 stdout（3 行，与调度顺序无关 —— 任务里不打印）：
 *   6        ← 0+1+2+3
 *   0        ← 收完之后的队列长度
 *   joined   ← scope 退出（join）之后才打印 */
#include "aic_l2.h"

static aic_chan_i32 *g_ch;

static void producer(void *env) {
    (void)env;
    for (aic_i32 i = 0; i < 4; i++) {
        aic_chan_send_i32(g_ch, i, "smoke_l3_pipeline.c", 21);
    }
}

int main(void) {
    aic_task_init("<smoke>", 1);
    aic_sched_init();
    g_ch = aic_chan_new_i32(1, 10); /* 有界：容量 1 = 背压点 */
    aic_i32 sum = 0;
    aic_scope s;
    aic_scope_enter(&s);
    aic_scope_spawn(&s, producer, aic_task_env(1));
    for (int k = 0; k < 4; k++) {
        aic_chan_i32_recv_t r = aic_chan_recv_i32(g_ch, "smoke_l3_pipeline.c", 30 + k);
        if (r._1) {
            sum += r._0;
        }
    }
    aic_scope_exit(&s);
    aic_println_i32(sum);
    aic_println_usize(aic_chan_len_i32(g_ch));
    aic_println_str((aic_str){"joined", 6});
    return 0;
}
