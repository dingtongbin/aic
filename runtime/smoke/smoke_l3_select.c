/* L3 冒烟：**select 的真等待**（N6 的运行时 ABI `aic_select_wait`）。
 *
 * 场景：一个任务延迟发送（`time.sleep`），主任务 select 两臂（recv / time.after）。
 * 先验"时间臂到点"（-1），再验"recv 臂就绪"（0）—— 两种结果都不靠调度巧合。
 * 期望 stdout（2 行）：
 *   timeout   ← 空通道 + time.after(0) 已到点 ⇒ 时间臂
 *   got 7     ← 任务已发值 ⇒ recv 臂 */
#include "aic_l2.h"

static aic_chan_i32 *g_ch;

static void sender(void *env) {
    (void)env;
    aic_sched_sleep_ms(20, false);
    aic_chan_send_i32(g_ch, 7, "smoke_l3_select.c", 18);
}

int main(void) {
    aic_task_init("<smoke>", 1);
    aic_sched_init();
    g_ch = aic_chan_new_i32(0, 8);

    /* ① 空通道 + 已到点的 time.after(0)：必须走时间臂（不阻塞、不 trap）。 */
    aic_sel_arm arms[2];
    arms[0].kind = AIC_SEL_RECV;
    arms[0].ch = g_ch;
    arms[1].kind = AIC_SEL_SEND;
    arms[1].ch = g_ch;
    aic_i64 dl = aic_time_deadline(0);
    int idx = aic_select_wait(arms, 1, dl, "smoke_l3_select.c", 26);
    if (idx != -1) {
        aic_println_str((aic_str){"FAIL: expected the time arm", 26});
        return 1;
    }
    aic_println_str((aic_str){"timeout", 7});

    /* ② 任务发值后再 select：必须走 recv 臂（下标 0）。 */
    aic_scope s;
    aic_scope_enter(&s);
    aic_scope_spawn(&s, sender, aic_task_env(1));
    int idx2 = aic_select_wait(arms, 1, 0, "smoke_l3_select.c", 36);
    if (idx2 != 0) {
        aic_println_str((aic_str){"FAIL: expected the recv arm", 26});
        return 1;
    }
    aic_chan_i32_recv_t r = aic_chan_recv_i32(g_ch, "smoke_l3_select.c", 40);
    aic_print_str((aic_str){"got ", 4});
    aic_println_i32(r._0);
    aic_scope_exit(&s);
    return 0;
}
