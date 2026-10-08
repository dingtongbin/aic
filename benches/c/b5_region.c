/* B5 区域回收（C 基线）：N 轮「块内建对象 → 退出整块回收」。
 * 与 benches/b5_region.aic 逐行同构。malloc/free 每轮一次配对 —— glibc 会复用
 * 同一块，故本基线回答的是"AIC 的区域模型相对逐节点 malloc/free 的开销与稳定性"，
 * 不是"C 会不会漏"（两者都不该漏）。 */
#include <stdio.h>
#include <stdlib.h>

typedef struct Buf {
    int n;
} Buf;

int main(void) {
    int rounds = 2000000;
    int acc = 0;
    for (int round = 0; round < rounds; round++) {
        Buf *b = (Buf *)malloc(sizeof(Buf));
        b->n = round;
        acc += b->n;
        free(b);
    }
    printf("%d\n", acc);
    return 0;
}
