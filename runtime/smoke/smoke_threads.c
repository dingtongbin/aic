/* R19 分代区域的**并发锚点**：多个真线程同时做
 * 「task_init → N 轮 region push/pop + 区域分配」，压力全打在**无锁空闲链**
 * 与每槽状态字上（旧形态这里是全局 id 分配器的互斥量）。
 *
 * 判据（三条，缺一不可）：
 *   ① TSan 零报告 —— 无锁链的 tag/ABA 设计与"取到槽即独占"的推论成立；
 *   ② stdout 逐位确定（每线程工作量固定 ⇒ 总和固定）—— 没有槽被两个线程
 *      同时拿到（那会让两个区域共享一条页链，表现为数字错乱/崩溃）；
 *   ③ 退出码 0 —— 无 trap、无段错误。
 */
#include <stdint.h>
#include "aic_l2.h" /* aic_plat.h 的线程面（AIC_THR_ENTRY）经此入 */

typedef struct { aic_hdr hdr; aic_i32 v; } smoke_obj;

#define SMOKE_THREADS 4
#define SMOKE_ROUNDS  20000

static aic_i64 g_sum[SMOKE_THREADS];

AIC_THR_ENTRY(smoke_thread_worker) {
    int id = (int)(intptr_t)raw;
    aic_task_init("<smoke-threads>", 1);
    aic_i64 acc = 0;
    for (int r = 0; r < SMOKE_ROUNDS; r++) {
        aic_region_push("<smoke-threads>", 3);
        smoke_obj *o = (smoke_obj *)aic_alloc_hdr(sizeof(smoke_obj), 4);
        o->v = (aic_i32)(id * 1000003 + r);
        acc += o->v;
        aic_region_pop();
    }
    g_sum[id] = acc;
    aic_region_release_task();
    AIC_THR_RETURN();
}

int main(void) {
    aic_task_init("<smoke-threads>", 1);
    aic_thr th[SMOKE_THREADS];
    for (int i = 0; i < SMOKE_THREADS; i++) {
        if (aic_thr_create(&th[i], smoke_thread_worker, (void *)(intptr_t)i) == 0) {
            printf("thread create failed\n");
            return 1;
        }
    }
    for (int i = 0; i < SMOKE_THREADS; i++) {
        aic_thr_join(th[i]);
    }
    aic_i64 total = 0;
    for (int i = 0; i < SMOKE_THREADS; i++) {
        total += g_sum[i];
    }
    printf("%lld\n", (long long)total);
    return 0;
}
