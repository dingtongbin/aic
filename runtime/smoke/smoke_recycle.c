/* 分代区域的**判别性锚点**（R19）：槽位回收后，内存**未被覆写**的旧一代对象
 * 依然必须 trap —— 这一条只有 gen 比对能拦下。
 *
 * 为什么这样构造：bump 复位后新一轮分配从同一起点开始，若新旧对象等大，旧对象的
 * 内存（连带头）会被新对象覆写，那时陈旧指针已别名成新对象（新旧设计同构，
 * 见 smoke_recycle 的设计说明）。故第二轮只分配**一个**对象：它落在 obj1 的
 * 地址上覆写 obj1，而 obj1b 的内存**不被覆写**——它的头仍记着第一代的
 * (slot, gen=1)，而该槽现在已是第二代（gen=2, depth=1 活着）。
 *   - 若守卫只看 depth：obj1b 的 reg 指向活槽、depth 1 ≤ limit 1 ⇒ 放过（错）；
 *   - 有 gen 比对：槽内 gen=2 ≠ 头里 gen=1 ⇒ AIC_REGION_DEAD ⇒ trap（对）。
 *
 * 判据 = 退出码非 0，stderr 前缀 "reference to a popped region"。
 */
#include "aic_l1.h"

typedef struct { aic_hdr hdr; aic_i32 v; } smoke_obj;

int main(void) {
    aic_task_init("<smoke>", 1);

    /* 第一代：两个对象。obj1b 的内存不会被第二轮碰到（见上）。 */
    aic_region_push("<smoke>", 5);
    smoke_obj *obj1  = (smoke_obj *)aic_alloc_hdr(sizeof(smoke_obj), 6);
    smoke_obj *obj1b = (smoke_obj *)aic_alloc_hdr(sizeof(smoke_obj), 7);
    obj1->v = 7;
    obj1b->v = 9;
    aic_region_pop();

    /* 第二代：同一个槽（空闲链 LIFO 必然拿回刚还的），gen+1，只分配一个对象。 */
    aic_region_push("<smoke>", 11);
    smoke_obj *obj2 = (smoke_obj *)aic_alloc_hdr(sizeof(smoke_obj), 12);
    obj2->v = 8;

    /* 新对象合法（活区域、深度 1 ≤ limit 1）：先证明活的没被误拦。 */
    aic_guard(obj2, 1, "smoke_recycle.aic", 13);

    /* 旧一代、内存未被覆写的对象必须 trap（gen 不符）。 */
    aic_guard(obj1b, 1, "smoke_recycle.aic", 14);
    return 0;
}
