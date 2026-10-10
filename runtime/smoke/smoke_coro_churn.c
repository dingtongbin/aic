/* 协程压力：N 轮「创建协程 → 协程内 region 分配 → 销毁」。
 * 判据 = stdout 确定 + RSS 收敛（协程载体/区域槽/页链全部回收）。
 */
#include "aic_l0.h"
#include "aic_l1.h"
#include "aic_l2.h"
#include "aic_l3.h"

#include <stdio.h>
#include <stdlib.h>

static void churn_once(void *raw) {
    (void)raw;
    /* Fiber 入口已建好任务根；这里只做 region 块（push/pop 自配平）。
     * 不再调 task_init/release_task —— 那会拿第二个根槽且永不归还。 */
    aic_region_push("smoke_coro_churn.c", __LINE__);
    aic_list_i64 *xs = aic_list_new_i64(8);
    for (int i = 0; i < 8; i++) {
        aic_list_append_i64(xs, i, "smoke_coro_churn.c", __LINE__);
    }
    aic_region_pop();
}

int main(void) {
    int N = atoi(getenv("N") ? getenv("N") : "200000");
    for (int i = 0; i < N; i++) {
        aic_coro *c = aic_coro_create(churn_once, NULL);
        aic_coro_schedule(c);
        aic_coro_destroy(c);
    }
    printf("coro-churn %d ok\n", N);
    return 0;
}
