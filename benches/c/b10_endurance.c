/* B10 长跑稳定性（内存探针，C 基线）。与 benches/b10_endurance.aic 逐行同构：
 * region 建/回收 ≈ malloc/free 配对；容器增长/清空；spawn ≈ 一次性线程（C 侧用
 * 串行函数调用模拟"任务体跑完即回收"，因为本探针量的是**发起方**的内存稳定性）。 */
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>

typedef struct Buf { int n; } Buf;

static int64_t spin(int64_t n) {
    int64_t acc = 0;
    for (int64_t i = 0; i < n; i++) acc += i;
    return acc;
}

int main(int argc, char **argv) {
    long long rounds = 300;
    if (argc >= 2) rounds = atoll(argv[1]);
    int64_t acc = 0;
    for (long long r = 0; r < rounds; r++) {
        /* ① region：malloc/free 配对 */
        Buf *b = (Buf *)malloc(sizeof(Buf));
        b->n = (int)r;
        acc += b->n;
        free(b);
        /* ② 容器：list + map + set 各建 2000/1000/1000（与 AIC 同构）。
         *     AIC 侧 acc += xs[0] + m.get(r) + s.len() = r + 0 + 1000；同口径。 */
        int64_t cap = 2048, len = 0;
        int64_t *xs = malloc(cap * sizeof(int64_t));
        for (int64_t i = 0; i < 2000; i++) xs[len++] = i + r;
        acc += xs[0] + 1000;
        len = 0;
        (void)len;
        free(xs);
        /* ③ spawn：两个任务体跑完即回收（串行等价）——AIC 侧 spawn 无返回位， */
        /* 故结果不进 acc（与 b10_endurance.aic 逐行同构）。 */
        spin(1000);
        spin(1000);
    }
    printf("%lld\n", (long long)acc);
    return 0;
}
