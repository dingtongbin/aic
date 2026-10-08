/* L4-g 冒烟：Err 的 cause 链（§16 N5 ④）。
 *
 * 冻结两件事：
 *   1. aic_err_wrap 把 cause **拷贝进当前区域**（栈上的临时 Err 也能安全上链）；
 *   2. aic_err_dump 的逐级文本（语言级契约：第一级 msg，其后 "  caused by: msg"）。
 * 判据在 scripts/runtime_smoke.py 里逐字节比对 stderr。 */
#include "aic_l0.h"

static aic_Err inner(void) {
    return (aic_Err){ 2, (aic_str){ "file not found", 14 }, NULL };
}

static aic_Err outer(void) {
    aic_Err e = inner();
    return aic_err_wrap(10, (aic_str){ "cannot load config", 18 }, e);
}

int main(void) {
    aic_task_init("<runtime: smoke>", 1); /* 区域/分配器必须先就位：wrap 会分配链节点 */
    aic_Err e = outer();
    aic_Err top = aic_err_wrap(20, (aic_str){ "startup failed", 14 }, e);
    aic_err_report(top);
    return 0;
}
