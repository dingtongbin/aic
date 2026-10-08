/* L4-g 冒烟：读**无 cause** 的错误的 cause = null dereference trap（§九）。
 * 判据：非 0 退出 + stderr 含 "null dereference"（不是静默读到垃圾）。 */
#include "aic_l0.h"

int main(void) {
    aic_task_init("<runtime: smoke>", 1);
    aic_Err e = (aic_Err){ 2, (aic_str){ "plain", 5 }, NULL };
    const aic_Err *p = (const aic_Err *)aic_deref_or_trap((void *)e.cause, "smoke_err_nil_cause.c", 9);
    aic_print_str(p->msg);
    return 0;
}
