/* 守卫 trap 冒烟：region 块内创建的对象存入更浅（任务层）存储 = trap。
 * 期望：非 0 退出，stderr 前缀 "reference outlives its region"。 */
#include "aic_l1.h"

typedef struct { aic_hdr hdr; aic_i32 v; } smoke_obj;

int main(void) {
    aic_task_init("<smoke>", 1);
    aic_region_push("<smoke>", 5);
    smoke_obj *obj = (smoke_obj *)aic_alloc_hdr(sizeof(smoke_obj), 6);
    obj->v = 7;
    /* limit = 0 表示存储深度 0（任务层）；对象区域深度 1 > 0 → trap */
    aic_guard(obj, 0, "smoke_deeper.aic", 7);
    return 0;
}
