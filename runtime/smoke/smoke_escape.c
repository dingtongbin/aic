/* 悬垂引用 trap 冒烟：对象所在区域已弹出，任何存储守卫必 trap。
 * 期望：非 0 退出，stderr 前缀 "reference to a popped region"。 */
#include "aic_l1.h"

typedef struct { aic_hdr hdr; aic_i32 v; } smoke_obj;

int main(void) {
    aic_task_init("<smoke>", 1);
    aic_region_push("<smoke>", 5);
    smoke_obj *obj = (smoke_obj *)aic_alloc_hdr(sizeof(smoke_obj), 6);
    obj->v = 7;
    aic_region_pop();
    aic_guard(obj, 0, "smoke_escape.aic", 8);
    return 0;
}
