/* 协程自检（T1）：单/多协程执行、区域根独立、C10K 驻留。 */
#include "aic_l0.h"
#include "aic_l1.h"
#include "aic_l2.h"
#include "aic_l3.h"

#include <stdio.h>
#include <stdlib.h>

static int g_n;

static void spin(void *raw) {
    (void)raw;
    for (int i = 0; i < 1000; i++) {
        g_n++;
    }
}

static void region_worker(void *raw) {
    (void)raw;
    /* 协程 = 任务 = 独立区域根：在协程里建 region 块，退出回滚。 */
    aic_region_state st;
    aic_region_state_save(&st);
    aic_region_push("smoke_coro.c", __LINE__);
    aic_list_i64 *xs = aic_list_new_i64(4);
    for (int i = 0; i < 4; i++) {
        aic_list_append_i64(xs, i, "smoke_coro.c", __LINE__);
    }
    if (xs->len != 4) {
        printf("BAD: region list\n");
    }
    aic_region_pop();
    aic_region_state_restore(&st);
    printf("region-ok\n");
}

int main(void) {
    aic_sched_init();

    /* 1. 单协程 */
    aic_coro *c = aic_coro_create(spin, NULL);
    aic_coro_schedule(c);
    aic_coro_destroy(c);
    printf("single-ok n=%d\n", g_n);

    /* 2. 协程内区域（R19 分代根 + region 块） */
    aic_coro *r = aic_coro_create(region_worker, NULL);
    aic_coro_schedule(r);
    aic_coro_destroy(r);

    /* 3. C10K 创建/销毁（驻留与创建成本） */
    enum { N = 10000 };
    for (int i = 0; i < N; i++) {
        aic_coro *w = aic_coro_create(spin, NULL);
        aic_coro_schedule(w);
        aic_coro_destroy(w);
    }
    printf("c10k-ok total=%d\n", g_n);
    return 0;
}
